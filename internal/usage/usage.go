package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

type Limits struct {
	TokensPerDay    int
	RequestsPerHour int
}

type Store struct{ db *sql.DB }

func New(st *store.Store) *Store { return &Store{db: st.DB} }

func period(now time.Time, d time.Duration) string { return now.UTC().Truncate(d).Format(time.RFC3339) }

func (s *Store) Add(ctx context.Context, session string, u llm.Usage) error {
	if session == "" {
		return nil
	}

	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens)
		VALUES (?, 'day', ?, 1, ?, ?)
		ON CONFLICT(session_id,period,period_start) DO UPDATE SET requests=requests+1,input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens`,
		session, period(now, 24*time.Hour), u.InputTokens, u.OutputTokens)
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
	var requests, in, out int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests),0),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0)
		FROM usage WHERE session_id=? AND period='day' AND period_start=?`, session, period(now, 24*time.Hour)).Scan(&requests, &in, &out)
	if err != nil {
		return err
	}
	if l.TokensPerDay > 0 && in+out >= l.TokensPerDay {
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
	if err := s.Add(ctx, session, u); err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage(session_id,period,period_start,requests,input_tokens,output_tokens)
		VALUES (?, 'hour', ?, 1, ?, ?)
		ON CONFLICT(session_id,period,period_start) DO UPDATE SET requests=requests+1,input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens`,
		session, period(now, time.Hour), u.InputTokens, u.OutputTokens)
	return err
}

type Row struct {
	SessionID, Period, PeriodStart      string
	Requests, InputTokens, OutputTokens int
}

func (s *Store) List(ctx context.Context, session string) ([]Row, error) {
	q := `SELECT session_id,period,period_start,requests,input_tokens,output_tokens FROM usage`
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
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.SessionID, &r.Period, &r.PeriodStart, &r.Requests, &r.InputTokens, &r.OutputTokens); err != nil {
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
