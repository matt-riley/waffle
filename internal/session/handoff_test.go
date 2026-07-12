package session_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

func TestSubagentPacketHandoffPersistsAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "waffle.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	packet := agent.WorkPacket{Task: "persist me", OwnedPaths: []string{"pkg"}}
	handoff := agent.Handoff{Status: "partial", Summary: "normalized", Reasons: []string{"requested verification missing"}}
	if err := session.PersistSubagentHandoff(ctx, st.DB, "parent", "child", packet, handoff); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	records, err := session.LoadSubagentHandoffs(ctx, st.DB, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("handoffs=%d", len(records))
	}
	var gotPacket agent.WorkPacket
	var gotHandoff agent.Handoff
	if err := json.Unmarshal([]byte(records[0].PacketJSON), &gotPacket); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(records[0].HandoffJSON), &gotHandoff); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotPacket, packet) || !reflect.DeepEqual(gotHandoff, handoff) {
		t.Fatalf("round trip packet=%+v handoff=%+v", gotPacket, gotHandoff)
	}
}
