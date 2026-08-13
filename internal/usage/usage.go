package usage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

type Limits struct {
	TokensPerDay          int
	RequestsPerHour       int
	AlertThresholdPercent int
	// TunnelBytesPerSession is the rolling-day byte budget for broker
	// CONNECT tunnelled egress (#244). The broker relays tunnel bytes without
	// inspection, so it cannot count requests inside a tunnel; the relay's
	// io.Copy byte counts are the only meter, charged against this budget.
	// Zero means unlimited, preserving pre-#244 behaviour.
	TunnelBytesPerSession int64
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
	provider := providerOrLegacyDefault(u.Provider)
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,provider)
		VALUES (?, 'day', ?, 1, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id,period,period_start,provider) DO UPDATE SET requests=requests+1,input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,cache_creation_input_tokens=cache_creation_input_tokens+excluded.cache_creation_input_tokens,cache_read_input_tokens=cache_read_input_tokens+excluded.cache_read_input_tokens`,
		session, period(now, 24*time.Hour), u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens, provider)
	if err != nil || !hour {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,provider)
		VALUES (?, 'hour', ?, 1, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id,period,period_start,provider) DO UPDATE SET requests=requests+1,input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,cache_creation_input_tokens=cache_creation_input_tokens+excluded.cache_creation_input_tokens,cache_read_input_tokens=cache_read_input_tokens+excluded.cache_read_input_tokens`,
		session, period(now, time.Hour), u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens, provider)
	return err
}

// AddDelta records only the increase from a cumulative provider observation.
func (s *Store) AddDelta(ctx context.Context, session string, previous, current llm.Usage) error {
	d := llm.Usage{
		InputTokens:              current.InputTokens - previous.InputTokens,
		OutputTokens:             current.OutputTokens - previous.OutputTokens,
		CacheCreationInputTokens: current.CacheCreationInputTokens - previous.CacheCreationInputTokens,
		CacheReadInputTokens:     current.CacheReadInputTokens - previous.CacheReadInputTokens,
		Provider:                 current.Provider,
	}
	if d.InputTokens < 0 {
		d.InputTokens = 0
	}
	if d.OutputTokens < 0 {
		d.OutputTokens = 0
	}
	if d.CacheCreationInputTokens < 0 {
		d.CacheCreationInputTokens = 0
	}
	if d.CacheReadInputTokens < 0 {
		d.CacheReadInputTokens = 0
	}
	if d.InputTokens == 0 && d.OutputTokens == 0 && d.CacheCreationInputTokens == 0 && d.CacheReadInputTokens == 0 {
		return nil
	}
	return s.AddRequest(ctx, session, d)
}

func (s *Store) Check(ctx context.Context, session string, l Limits, now time.Time) error {
	if session == "" {
		return nil
	}
	billed, err := s.billedDayTokens(ctx, s.db, session, now)
	if err != nil {
		return err
	}
	if l.TokensPerDay > 0 && reachesLimit(l.TokensPerDay, billed) {
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
	if l.TunnelBytesPerSession > 0 {
		used, err := s.TunnelBytesAt(ctx, session, now)
		if err != nil {
			return err
		}
		if used >= l.TunnelBytesPerSession {
			return fmt.Errorf("usage limit exceeded: tunnelled egress byte budget (%d)", l.TunnelBytesPerSession)
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
// provider is the provider type the request is being sent to ("anthropic" or
// "openai"); it is recorded on the reservation's rows so their cache tokens
// price at the provider's own multipliers (#247).
func (s *Store) ReserveRequestAt(ctx context.Context, session, provider string, l Limits, now time.Time, declared int, reserveRemaining bool) (reserved int, err error) {
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
	billed, err := s.billedDayTokens(ctx, tx, session, now)
	if err != nil {
		return 0, err
	}
	if l.TokensPerDay > 0 {
		remaining, ok := remainingAllowance(l.TokensPerDay, billed)
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,reserved_tokens,provider)
			VALUES (?, ?, ?, 1, 0, 0, 0, 0, ?, ?)
			ON CONFLICT(session_id,period,period_start,provider) DO UPDATE SET requests=requests+1,reserved_tokens=reserved_tokens+excluded.reserved_tokens`,
			session, p.name, period(now, p.d), reserved, providerOrLegacyDefault(provider)); err != nil {
			return 0, err
		}
	}
	err = tx.Commit()
	return reserved, err
}

// ReconcileReservationAt replaces a durable pre-dispatch reservation with
// trustworthy final provider usage. A zero usage observation is not final and
// intentionally leaves the reservation charged. Cache counters are carried
// as raw persisted columns (for reporting) and weight budget binding via
// billedTokens.
func (s *Store) ReconcileReservationAt(ctx context.Context, session string, now time.Time, reserved int, actual llm.Usage) error {
	if session == "" ||
		(actual.InputTokens <= 0 && actual.OutputTokens <= 0 &&
			actual.CacheCreationInputTokens <= 0 && actual.CacheReadInputTokens <= 0) {
		return nil
	}
	input, output := actual.InputTokens, actual.OutputTokens
	cacheWrite, cacheRead := actual.CacheCreationInputTokens, actual.CacheReadInputTokens
	if input < 0 {
		input = 0
	}
	if output < 0 {
		output = 0
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	if cacheRead < 0 {
		cacheRead = 0
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
			cache_creation_input_tokens=CASE WHEN cache_creation_input_tokens > ? THEN ? ELSE cache_creation_input_tokens+? END,
			cache_read_input_tokens=CASE WHEN cache_read_input_tokens > ? THEN ? ELSE cache_read_input_tokens+? END,
			reserved_tokens=MAX(0,reserved_tokens-?)
			WHERE session_id=? AND period=? AND period_start=? AND provider=?`,
			maxInt-input, maxInt, input, maxInt-output, maxInt, output,
			maxInt-cacheWrite, maxInt, cacheWrite, maxInt-cacheRead, maxInt, cacheRead,
			reserved, session, p.name, period(now, p.d), providerOrLegacyDefault(actual.Provider))
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

// queryer is satisfied by *sql.DB and *sql.Tx so the per-provider day-sum
// helper runs identically inside and outside the reservation transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// dayTokenSumQuery aggregates one day's counters per provider type. Usage
// rows are keyed by (session_id, period, period_start, provider) (migration
// 0030), so a budget key that routed requests to more than one upstream
// keeps one row per provider; the billed total must be computed per provider
// with that provider's cost model and summed. Legacy rows default to
// 'anthropic' (migration 0029), preserving their original pricing.
const dayTokenSumQuery = `SELECT provider,COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(cache_creation_input_tokens),0),COALESCE(SUM(cache_read_input_tokens),0),COALESCE(SUM(reserved_tokens),0)
	FROM usage WHERE session_id=? AND period='day' AND period_start=? GROUP BY provider`

// billedDayTokens returns the day's billed-input-equivalent token total for
// session, pricing each provider group with its own cost model. Output and
// reserved tokens count at face value, matching the pre-existing budget
// semantics.
func (s *Store) billedDayTokens(ctx context.Context, q queryer, session string, now time.Time) (total int, err error) {
	rows, err := q.QueryContext(ctx, dayTokenSumQuery, session, period(now, 24*time.Hour))
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	for rows.Next() {
		var provider string
		var input, output, cacheWrite, cacheRead, reserved int
		if err := rows.Scan(&provider, &input, &output, &cacheWrite, &cacheRead, &reserved); err != nil {
			return 0, err
		}
		total += billedTokens(provider, input, output, cacheWrite, cacheRead, reserved)
	}
	return total, rows.Err()
}

// providerOrLegacyDefault maps an empty provider attribution to the legacy
// Anthropic default, so persisted rows always carry an explicit type and
// pre-attribution behavior (Anthropic pricing) is unchanged.
func providerOrLegacyDefault(provider string) string {
	if provider == "" {
		return "anthropic"
	}
	return provider
}

// billedTokens converts one provider group's persisted counters into the
// billed-input-equivalent token count used for budget binding: cache-creation
// tokens carry the provider's cache-write surcharge and cache-read tokens the
// cache-read discount (llm.CostModelForType), so limits and alerts bind on
// true cost rather than pre-cache token counts. Output and reserved tokens
// count at face value, matching the pre-existing budget semantics. Rows that
// predate provider attribution (or callers that never learned the provider)
// price at the Anthropic model, the legacy default (#247 review).
func billedTokens(provider string, input, output, cacheWrite, cacheRead, reserved int) int {
	weighted := llm.CostModelForType(provider).BilledInput(llm.Usage{
		InputTokens:              input,
		CacheCreationInputTokens: cacheWrite,
		CacheReadInputTokens:     cacheRead,
	})
	return int(math.Round(weighted)) + output + reserved
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
	if session == "" || (u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0) {
		return nil
	}
	provider := providerOrLegacyDefault(u.Provider)
	for _, p := range usagePeriods {
		_, err := s.db.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,provider)
			VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id,period,period_start,provider) DO UPDATE SET input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,cache_creation_input_tokens=cache_creation_input_tokens+excluded.cache_creation_input_tokens,cache_read_input_tokens=cache_read_input_tokens+excluded.cache_read_input_tokens`,
			session, p.name, period(now, p.d), u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens, provider)
		if err != nil {
			return err
		}
	}
	return nil
}

// AddTunnelBytesAt records bytes relayed by a broker CONNECT tunnel for a
// session (#244). The relay sees io.Copy byte counts, never the tunnelled
// requests themselves, so bytes are the only meter. Rows carry provider
// 'tunnel' and zero requests, so they never collide with provider token rows
// and never count toward token budgets.
func (s *Store) AddTunnelBytesAt(ctx context.Context, session string, n int64, now time.Time) error {
	if session == "" || n <= 0 {
		return nil
	}
	now = now.UTC()
	for _, p := range usagePeriods {
		_, err := s.db.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,tunnel_bytes,provider)
			VALUES (?, ?, ?, 0, 0, 0, 0, 0, ?, 'tunnel')
			ON CONFLICT(session_id,period,period_start,provider) DO UPDATE SET tunnel_bytes=tunnel_bytes+excluded.tunnel_bytes`,
			session, p.name, period(now, p.d), n)
		if err != nil {
			return err
		}
	}
	return nil
}

// TunnelBytesAt returns the session's rolling-day tunnelled egress bytes.
func (s *Store) TunnelBytesAt(ctx context.Context, session string, now time.Time) (int64, error) {
	if session == "" {
		return 0, nil
	}
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(tunnel_bytes),0) FROM usage WHERE session_id=? AND period='day' AND period_start=?`,
		session, period(now, 24*time.Hour)).Scan(&n)
	return n, err
}

// Alert delivers one notice when a configured budget reaches 80 percent.
// A durable flag suppresses repeat notices for the same session and period.
func (s *Store) Alert(ctx context.Context, session string, l Limits, now time.Time, deliver func(context.Context, string) error) error {
	if session == "" || deliver == nil {
		return nil
	}
	// Cache tokens bill at a discount (reads) or surcharge (writes), so the
	// threshold binds on true cost — priced per row's provider — not
	// pre-cache token counts.
	used, err := s.billedDayTokens(ctx, s.db, session, now)
	if err != nil {
		return err
	}
	threshold := l.AlertThresholdPercent
	if threshold == 0 {
		threshold = 80
	}
	if l.TokensPerDay <= 0 || used*100 < l.TokensPerDay*threshold {
		return nil
	}
	key := strings.Join([]string{"usage-alert", session, "day", period(now, 24*time.Hour)}, ":")
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
	Provider                                            string
	Requests, InputTokens, OutputTokens, ReservedTokens int
	CacheCreationInputTokens, CacheReadInputTokens      int
	TunnelBytes                                         int64
}

func (s *Store) List(ctx context.Context, session string) (out []Row, err error) {
	q := `SELECT session_id,period,period_start,provider,requests,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,reserved_tokens,tunnel_bytes FROM usage`
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
		if err := rows.Scan(&r.SessionID, &r.Period, &r.PeriodStart, &r.Provider, &r.Requests, &r.InputTokens, &r.OutputTokens, &r.CacheCreationInputTokens, &r.CacheReadInputTokens, &r.ReservedTokens, &r.TunnelBytes); err != nil {
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
