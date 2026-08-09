package spill

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

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

	// Exactly OutputLimit runes → no spill.
	short := strings.Repeat("a", tool.OutputLimit)
	if utf8.RuneCountInString(short) != tool.OutputLimit {
		t.Fatal("setup")
	}
	id, partial, err := s.Save(ctx, "sess", "bash", short)
	if err != nil || id != "" || partial {
		t.Fatalf("at limit: id=%q partial=%v err=%v", id, partial, err)
	}
	// One past OutputLimit → spill.
	over := short + "Z"
	id, partial, err = s.Save(ctx, "sess-boundary", "bash", over)
	if err != nil || id == "" || partial {
		t.Fatalf("over limit: id=%q partial=%v err=%v", id, partial, err)
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
	// No match pattern
	nomatch, err := s.Expand(ctx, id, 0, 0, "NEVER_IN_SPILL_QQQ")
	if err != nil || !strings.Contains(nomatch, "no matches") {
		t.Fatalf("no match: %q %v", nomatch, err)
	}
	// Unknown id
	if _, err := s.Expand(ctx, "missing", 0, 10, ""); err == nil {
		t.Fatal("expected unknown id")
	}
	// Offset out of range
	if _, err := s.Expand(ctx, id, len(big)+100, 10, ""); err == nil {
		t.Fatal("expected OOR offset")
	}
	// FTS middle-of-output findable
	fts, err := s.SearchFTS(ctx, "UNIQUE_SPILL_TOKEN_XYZ", 5)
	if err != nil || len(fts) == 0 {
		t.Fatalf("fts: %v %v", fts, err)
	}
	if err := s.DeleteSession(ctx, "sess"); err != nil {
		t.Fatal(err)
	}
	// After delete, expand fails
	if _, err := s.Expand(ctx, id, 0, 10, ""); err == nil {
		t.Fatal("expand after DeleteSession should fail")
	}
}

func TestSpillCapPartialMarker(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Store{DB: st.DB}
	// Content larger than SpillCap.
	huge := strings.Repeat("H", SpillCap+1000)
	// Must also exceed OutputLimit (SpillCap is larger).
	id, partial, err := s.Save(ctx, "sess", "bash", huge)
	if err != nil || id == "" {
		t.Fatalf("save: %q %v %v", id, partial, err)
	}
	if !partial {
		t.Fatal("expected partial=true when over SpillCap")
	}
	m := Marker(id, true)
	if !strings.Contains(m, "partial spill") || !strings.Contains(m, id) {
		t.Fatal(m)
	}
	content, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != SpillCap {
		t.Fatalf("stored len=%d want SpillCap=%d", len(content), SpillCap)
	}
}

func TestExpandToolUnknownAndOOR(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tl := ExpandTool{Store: &Store{DB: st.DB}}
	if _, err := tl.Run(ctx, json.RawMessage(`{"id":"nope"}`)); err == nil {
		t.Fatal("unknown id")
	}
}

func TestMarker(t *testing.T) {
	m := Marker("spill-abc", false)
	if !strings.Contains(m, "spill-abc") || !strings.Contains(m, "expand_output") {
		t.Fatal(m)
	}
	mp := Marker("spill-abc", true)
	if !strings.Contains(mp, "partial") {
		t.Fatal(mp)
	}
}

func TestSpillUtf8BoundariesStayValid(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Store{DB: st.DB}

	// Multi-byte runes: "é" (2 bytes), "日本語" (3 bytes each). Exceed
	// SpillCap with a mix so the cap cut and window slices must land mid-rune.
	content := strings.Repeat("é日本語x", 300000) // ~6 bytes × 300k = 1.8MB > SpillCap
	id, partial, err := s.Save(ctx, "sess-utf8", "bash", content)
	if err != nil || id == "" {
		t.Fatalf("save: id=%q partial=%v err=%v", id, partial, err)
	}
	if !partial {
		t.Fatal("expected partial spill for oversized content")
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(got) {
		t.Fatal("stored spill content is invalid UTF-8")
	}
	if len(got) > SpillCap {
		t.Fatalf("stored spill len=%d exceeds SpillCap", len(got))
	}

	// Single-line grep window: offsets land mid-rune.
	hits, err := s.Expand(ctx, id, 0, 0, "日本語x")
	if err != nil || !utf8.ValidString(hits) {
		t.Fatalf("grep hits invalid UTF-8: %v %q", err, hits)
	}

	// Raw byte-range expansion with mid-rune offset/end.
	out, err := s.Expand(ctx, id, 7, 3, "") // byte 7 is a continuation byte
	if err != nil || !utf8.ValidString(out) {
		t.Fatalf("range expand invalid UTF-8: %v %q", err, out)
	}
}
