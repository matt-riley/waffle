package spill

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

func TestSpillBoundaryAndExpand(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Store{DB: st.DB}

	short := strings.Repeat("a", tool.OutputLimit)
	id, partial, err := s.Save(ctx, "sess", "bash", short)
	if err != nil || id != "" || partial {
		t.Fatalf("short: id=%q partial=%v err=%v", id, partial, err)
	}

	// Unique mid-token for FTS (must exceed tool.OutputLimit runes).
	big := strings.Repeat("x", tool.OutputLimit) + "\nUNIQUE_SPILL_TOKEN_XYZ\n" + strings.Repeat("y", 1000)
	id, partial, err = s.Save(ctx, "sess", "bash", big)
	if err != nil || id == "" {
		t.Fatalf("big: %q %v %v", id, partial, err)
	}
	out, err := s.Expand(ctx, id, 0, 100, "")
	if err != nil || !strings.Contains(out, "x") {
		t.Fatalf("expand range: %q %v", out, err)
	}
	hits, err := s.Expand(ctx, id, 0, 0, "UNIQUE_SPILL_TOKEN_XYZ")
	if err != nil || !strings.Contains(hits, "UNIQUE_SPILL_TOKEN_XYZ") {
		t.Fatalf("grep: %q %v", hits, err)
	}
	if _, err := s.Expand(ctx, "missing", 0, 10, ""); err == nil {
		t.Fatal("expected unknown id")
	}
	fts, err := s.SearchFTS(ctx, "UNIQUE_SPILL_TOKEN_XYZ", 5)
	if err != nil || len(fts) == 0 {
		t.Fatalf("fts: %v %v", fts, err)
	}
	if err := s.DeleteSession(ctx, "sess"); err != nil {
		t.Fatal(err)
	}
}

func TestMarker(t *testing.T) {
	m := Marker("spill-abc", false)
	if !strings.Contains(m, "spill-abc") || !strings.Contains(m, "expand_output") {
		t.Fatal(m)
	}
}
