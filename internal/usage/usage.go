package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

type Limits struct {
	TokensPerDay          int
	RequestsPerHour       int
	AlertThresholdPercent int
}

type Store struct{ db *sql.DB }

func New(st *store.Store) *Store { return &Store{db: st.DB} }

func period(now time.Time, d time.Duration) string { return now.UTC().Truncate(d).Format(time.RFC3339) }

// usagePeriods are the two rolling windows every usage row is tracked under.
var usagePeriods = []struct {
	name string
	d    time.Duration
}{{"day", 24 * time.Hour}, {"hour", time.Hour}}

func (s *Store) Add(ctx context.Context, session string, u llm.Usage) error {
	return s.addAt(ctx, session, u, time.Now().UTC(), false)
}

func (s *Store) addAt(ctx context.Context, session string, u llm.Usage, now time.Time, hour bool) error {
	if session == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens)
		VALUES (?, 'day', ?, 1, ?, ?)
		ON CONFLICT(session_id,period,period_start) DO UPDATE SET requests=requests+1,input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens`,
		session, period(now, 24*time.Hour), u.InputTokens, u.OutputTokens)
	if err != nil || !hour {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens)
		VALUES (?, 'hour', ?, 1, ?, ?)
		ON CONFLICT(session_id,period,period_start) DO UPDATE SET requests=requests+1,input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens`,
		session, period(now, time.Hour), u.InputTokens, u.OutputTokens)
	return err
}

// AddDelta records only the increase from a cumulative provider observation.
func (s *Store) AddDelta(ctx context.Context, session string, previous, current llm.Usage) error {
	d := llm.Usage{InputTokens: current.InputTokens - previous.InputTokens, OutputTokens: current.OutputTokens - previous.OutputTokens}
	if d.InputTokens < 0 {
		d.InputTokens = 0
	}
	if d.OutputTokens < 0 {
		d.OutputTokens = 0
	}
	if d.InputTokens == 0 && d.OutputTokens == 0 {
		return nil
	}
	return s.AddRequest(ctx, session, d)
}

func (s *Store) Check(ctx context.Context, session string, l Limits, now time.Time) error {
	if session == "" {
		return nil
	}
	var requests, in, out, reserved int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests),0),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(reserved_tokens),0)
		FROM usage WHERE session_id=? AND period='day' AND period_start=?`, session, period(now, 24*time.Hour)).Scan(&requests, &in, &out, &reserved)
	if err != nil {
		return err
	}
	if l.TokensPerDay > 0 && reachesLimit(l.TokensPerDay, in, out, reserved) {
		return fmt.Errorf("usage limit exceeded: daily token budget (%d)", l.TokensPerDay)
	}
	if l.RequestsPerHour > 0 {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests),0) FROM usage WHERE session_id=? AND period='hour' AND period_start=?`, session, period(now, time.Hour)).Scan(&n); err != nil {
			return err
		}
		if n >= l.RequestsPerHour {
			return fmt.Errorf("usage limit exceeded: hourly request budget (%d)", l.RequestsPerHour)
		}
	}
	return nil
}

func (s *Store) AddRequest(ctx context.Context, session string, u llm.Usage) error {
	return s.AddRequestAt(ctx, session, u, time.Now().UTC())
}

// AddRequestAt records a request using a caller-supplied clock.
func (s *Store) AddRequestAt(ctx context.Context, session string, u llm.Usage, now time.Time) error {
	return s.addAt(ctx, session, u, now.UTC(), true)
}

// ReserveRequestAt atomically checks both caps and records one request. This
// prevents concurrent broker calls from all passing a check-before-increment
// race. A trustworthy final usage observation reconciles the reservation.
func (s *Store) ReserveRequestAt(ctx context.Context, session string, l Limits, now time.Time, declared int, reserveRemaining bool) (reserved int, err error) {
	if session == "" {
		return 0, nil
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var in, out, alreadyReserved int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(reserved_tokens),0)
		FROM usage WHERE session_id=? AND period='day' AND period_start=?`, session, period(now, 24*time.Hour)).Scan(&in, &out, &alreadyReserved); err != nil {
		return 0, err
	}
	if l.TokensPerDay > 0 {
		remaining, ok := remainingAllowance(l.TokensPerDay, in, out, alreadyReserved)
		if !ok {
			return 0, fmt.Errorf("usage limit exceeded: daily token budget (%d)", l.TokensPerDay)
		}
		reserved = declared
		if reserveRemaining {
			reserved = remaining
		}
		if reserved < 0 || reserved > remaining {
			return 0, fmt.Errorf("usage limit exceeded: daily token budget (%d)", l.TokensPerDay)
		}
	}
	var requests int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests),0) FROM usage
		WHERE session_id=? AND period='hour' AND period_start=?`, session, period(now, time.Hour)).Scan(&requests); err != nil {
		return 0, err
	}
	if l.RequestsPerHour > 0 && requests >= l.RequestsPerHour {
		return 0, fmt.Errorf("usage limit exceeded: hourly request budget (%d)", l.RequestsPerHour)
	}
	for _, p := range usagePeriods {
		if _, err = tx.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens,reserved_tokens)
			VALUES (?, ?, ?, 1, 0, 0, ?)
			ON CONFLICT(session_id,period,period_start) DO UPDATE SET requests=requests+1,reserved_tokens=reserved_tokens+excluded.reserved_tokens`, session, p.name, period(now, p.d), reserved); err != nil {
			return 0, err
		}
	}
	err = tx.Commit()
	return reserved, err
}

// ReconcileReservationAt replaces a durable pre-dispatch reservation with
// trustworthy final provider usage. A zero usage observation is not final and
// intentionally leaves the reservation charged.
func (s *Store) ReconcileReservationAt(ctx context.Context, session string, now time.Time, reserved int, actual llm.Usage) error {
	if session == "" || (actual.InputTokens <= 0 && actual.OutputTokens <= 0) {
		return nil
	}
	input, output := actual.InputTokens, actual.OutputTokens
	if input < 0 {
		input = 0
	}
	if output < 0 {
		output = 0
	}
	maxInt := int(^uint(0) >> 1)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, p := range usagePeriods {
		res, err := tx.ExecContext(ctx, `UPDATE usage SET
			input_tokens=CASE WHEN input_tokens > ? THEN ? ELSE input_tokens+? END,
			output_tokens=CASE WHEN output_tokens > ? THEN ? ELSE output_tokens+? END,
			reserved_tokens=MAX(0,reserved_tokens-?)
			WHERE session_id=? AND period=? AND period_start=?`,
			maxInt-input, maxInt, input, maxInt-output, maxInt, output,
			reserved, session, p.name, period(now, p.d))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return fmt.Errorf("usage reservation missing for %s %s", session, p.name)
		}
	}
	return tx.Commit()
}

func reachesLimit(limit int, values ...int) bool {
	_, ok := remainingAllowance(limit, values...)
	return !ok
}

func remainingAllowance(limit int, values ...int) (int, bool) {
	remaining := limit
	for _, value := range values {
		if value >= remaining {
			return 0, false
		}
		if value > 0 {
			remaining -= value
		}
	}
	return remaining, remaining > 0
}

// AddTokensAt adds returned provider usage without counting a second request.
// It is used by streaming proxies that reserve the request before forwarding
// and only learn token totals when the response completes.
func (s *Store) AddTokensAt(ctx context.Context, session string, u llm.Usage, now time.Time) error {
	if session == "" || (u.InputTokens == 0 && u.OutputTokens == 0) {
		return nil
	}
	for _, p := range usagePeriods {
		_, err := s.db.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens)
			VALUES (?, ?, ?, 0, ?, ?)
			ON CONFLICT(session_id,period,period_start) DO UPDATE SET input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens`,
			session, p.name, period(now, p.d), u.InputTokens, u.OutputTokens)
		if err != nil {
			return err
		}
	}
	return nil
}

// Alert delivers one notice when a configured budget reaches 80 percent.
// A durable flag suppresses repeat notices for the same session and period.
func (s *Store) Alert(ctx context.Context, session string, l Limits, now time.Time, deliver func(context.Context, string) error) error {
	if session == "" || deliver == nil {
		return nil
	}
	var used int
	start := period(now, 24*time.Hour)
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens+output_tokens+reserved_tokens),0) FROM usage WHERE session_id=? AND period='day' AND period_start=?`, session, start).Scan(&used); err != nil {
		return err
	}
	threshold := l.AlertThresholdPercent
	if threshold == 0 {
		threshold = 80
	}
	if l.TokensPerDay <= 0 || used*100 < l.TokensPerDay*threshold {
		return nil
	}
	key := strings.Join([]string{"usage-alert", session, "day", start}, ":")
	res, err := s.db.ExecContext(ctx, `INSERT INTO runtime_flags(name,value) VALUES(?, '1') ON CONFLICT(name) DO NOTHING`, key)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if err := deliver(ctx, fmt.Sprintf("usage alert: session %s crossed %d%% of daily token budget (%d/%d)", session, threshold, used, l.TokensPerDay)); err != nil {
		_, _ = s.db.ExecContext(context.WithoutCancel(ctx), `DELETE FROM runtime_flags WHERE name=?`, key)
		return err
	}
	return nil
}

type Row struct {
	SessionID, Period, PeriodStart                      string
	Requests, InputTokens, OutputTokens, ReservedTokens int
}

func (s *Store) List(ctx context.Context, session string) (out []Row, err error) {
	q := `SELECT session_id,period,period_start,requests,input_tokens,output_tokens,reserved_tokens FROM usage`
	args := []any{}
	if session != "" {
		q += ` WHERE session_id=?`
		args = append(args, session)
	}
	q += ` ORDER BY period_start DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.SessionID, &r.Period, &r.PeriodStart, &r.Requests, &r.InputTokens, &r.OutputTokens, &r.ReservedTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Paused(ctx context.Context) (bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM runtime_flags WHERE name='paused'`).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return v == "1", err
}
func (s *Store) SetPaused(ctx context.Context, p bool) error {
	v := "0"
	if p {
		v = "1"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runtime_flags(name,value) VALUES('paused',?) ON CONFLICT(name) DO UPDATE SET value=excluded.value`, v)
	return err
}
