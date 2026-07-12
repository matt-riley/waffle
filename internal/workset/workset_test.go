package workset

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/store"
)

func TestAddCapAndRender(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Store{DB: st.DB, MaxEntries: 2, MaxBytes: 200}
	if _, err := s.Add(ctx, "s1", KindGoal, "ship phase 4", SourceUser, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(ctx, "s1", KindConstraint, "no host bash", SourceSystem, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(ctx, "s1", KindFact, "overflow", SourceUser, false); err == nil {
		t.Fatal("expected full error")
	}
	list, err := s.List(ctx, "s1")
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v %v", list, err)
	}
	if list[0].Kind != KindGoal || !list[0].Pinned {
		t.Fatalf("pinned first: %+v", list[0])
	}
	r := Render(list)
	if !strings.Contains(r, "<working_set>") || !strings.Contains(r, "ship phase 4") {
		t.Fatalf("render = %q", r)
	}
	if Render(nil) != "" {
		t.Fatal("empty render")
	}
}

func TestReplaceDropClear(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Store{DB: st.DB}
	e, err := s.Add(ctx, "s1", KindDecision, "use SQLite", SourceUser, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(ctx, "s1", e.ID, "use SQLite with FTS", SourceUser); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx, "s1")
	if len(list) != 1 || !strings.Contains(list[0].Body, "FTS") {
		t.Fatalf("%+v", list)
	}
	if err := s.Drop(ctx, "s1", list[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProposal(t *testing.T) {
	if err := ValidateProposal(Proposal{Op: "add", Kind: KindFact, Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProposal(Proposal{Op: "add", Kind: "nope", Body: "x"}); err == nil {
		t.Fatal("expected kind error")
	}
	if err := ValidateProposal(Proposal{Op: "drop"}); err == nil {
		t.Fatal("expected id error")
	}
}
