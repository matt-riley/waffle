package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/matt-riley/waffle/internal/id"
)

// SubagentHandoffRecord is the durable typed-envelope storage record. JSON is
// returned verbatim so callers can decode into the current packet/handoff types.
type SubagentHandoffRecord struct {
	PacketJSON  string
	HandoffJSON string
}

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
	records, err := LoadSubagentHandoffs(ctx, db, parentSession)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.HandoffJSON)
	}
	return out, nil
}

// LoadSubagentHandoffs returns packet and normalized handoff JSON together.
func LoadSubagentHandoffs(ctx context.Context, db *sql.DB, parentSession string) ([]SubagentHandoffRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT packet_json, handoff_json FROM subagent_handoffs
		WHERE parent_session = ? ORDER BY created_at DESC`, parentSession)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SubagentHandoffRecord
	for rows.Next() {
		var record SubagentHandoffRecord
		if err := rows.Scan(&record.PacketJSON, &record.HandoffJSON); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}
