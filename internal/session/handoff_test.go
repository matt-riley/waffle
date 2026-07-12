package session

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/store"
)

func TestSubagentPacketHandoffPersistsAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "waffle.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	packet := map[string]any{"task": "persist me", "owned_paths": []string{"pkg"}}
	handoff := map[string]any{"status": "partial", "summary": "normalized", "reasons": []string{"requested verification missing"}}
	if err := PersistSubagentHandoff(ctx, st.DB, "parent", "child", packet, handoff); err != nil {
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
	raws, err := LoadSubagentHandoffJSON(ctx, st.DB, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(raws) != 1 {
		t.Fatalf("handoffs=%d", len(raws))
	}
	raw := raws[0]
	if !strings.Contains(raw, "normalized") || !strings.Contains(raw, "requested verification missing") {
		t.Fatalf("handoff=%q", raw)
	}
}
