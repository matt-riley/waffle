package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/matt-riley/waffle/internal/id"
)

// PersistSubagentHandoff stores the typed packet and normalized handoff so
// they survive a gateway restart (#78). packet and handoff are any
// JSON-serializable values (typically agent.WorkPacket / agent.Handoff).
func PersistSubagentHandoff(ctx context.Context, db *sql.DB, parentSession, childSession string, packet, handoff any) error {
	if db == nil {
		return nil
	}
	pid, err := id.New("handoff-")
	if err != nil {
		return err
	}
	pj, err := json.Marshal(packet)
	if err != nil {
		return err
	}
	hj, err := json.Marshal(handoff)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO subagent_handoffs (id, parent_session, child_session, packet_json, handoff_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		pid, parentSession, childSession, string(pj), string(hj), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// LoadSubagentHandoffJSON returns raw handoff JSON for a parent session.
func LoadSubagentHandoffJSON(ctx context.Context, db *sql.DB, parentSession string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT handoff_json FROM subagent_handoffs
		WHERE parent_session = ? ORDER BY created_at DESC`, parentSession)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, rows.Err()
}
