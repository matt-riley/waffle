package workset

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestWorkingSetPersistsAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "w.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	ws := &Store{DB: st.DB}
	e, err := ws.Add(ctx, "resumed", KindConstraint, "keep exact state", SourceUser, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	entries, err := (&Store{DB: st.DB}).List(ctx, "resumed")
	if err != nil || len(entries) != 1 || entries[0].ID != e.ID || entries[0].Body != e.Body || entries[0].Source != SourceUser {
		t.Fatalf("reopened entries=%+v err=%v", entries, err)
	}
}

func TestMultibyteEntryEnforcesByteBoundary(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ws := &Store{DB: st.DB}
	if _, err := ws.Add(ctx, "s", KindFact, strings.Repeat("é", MaxEntryBytes/2), SourceUser, false); err != nil {
		t.Fatalf("exact byte boundary rejected: %v", err)
	}
	if _, err := ws.Add(ctx, "overflow", KindFact, strings.Repeat("é", MaxEntryBytes/2+1), SourceUser, false); err == nil {
		t.Fatal("multibyte entry beyond byte limit accepted")
	}
}

// Concurrent Adds must not overshoot MaxEntries (TOCTOU on list→insert).
func TestConcurrentAddRespectsMaxEntries(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const maxEntries = 5
	const workers = 20
	s := &Store{DB: st.DB, MaxEntries: maxEntries, MaxBytes: 64 * 1024}

	var (
		wg       sync.WaitGroup
		okCount  atomic.Int64
		errCount atomic.Int64
	)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			body := fmt.Sprintf("entry-%d", i)
			_, err := s.Add(ctx, "s1", KindFact, body, SourceUser, false)
			if err != nil {
				errCount.Add(1)
				if !strings.Contains(err.Error(), "full") && !strings.Contains(err.Error(), "byte budget") {
					t.Errorf("unexpected add error: %v", err)
				}
				return
			}
			okCount.Add(1)
		}()
	}
	wg.Wait()

	list, err := s.List(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) > maxEntries {
		t.Fatalf("cap overshot: list len=%d max=%d", len(list), maxEntries)
	}
	if len(list) != int(okCount.Load()) {
		t.Fatalf("list len=%d != successful adds=%d", len(list), okCount.Load())
	}
	if okCount.Load() != maxEntries {
		t.Fatalf("successful adds=%d want %d", okCount.Load(), maxEntries)
	}
	if errCount.Load() == 0 {
		t.Fatal("expected some Adds to fail with full/budget errors")
	}
	ids := map[string]struct{}{}
	for _, e := range list {
		if _, dup := ids[e.ID]; dup {
			t.Fatalf("duplicate id %q", e.ID)
		}
		ids[e.ID] = struct{}{}
	}
}

// Concurrent Adds must not overshoot MaxBytes.
func TestConcurrentAddRespectsMaxBytes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Each body is 40 bytes; budget fits at most 2 entries.
	const bodyLen = 40
	const maxBytes = 90
	const workers = 20
	s := &Store{DB: st.DB, MaxEntries: 100, MaxBytes: maxBytes}
	bodyBase := strings.Repeat("x", bodyLen)

	var (
		wg       sync.WaitGroup
		okCount  atomic.Int64
		errCount atomic.Int64
	)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			// Distinct bodies, same byte length.
			body := bodyBase[:bodyLen-2] + fmt.Sprintf("%02d", i)
			_, err := s.Add(ctx, "s1", KindFact, body, SourceUser, false)
			if err != nil {
				errCount.Add(1)
				if !strings.Contains(err.Error(), "full") && !strings.Contains(err.Error(), "byte budget") {
					t.Errorf("unexpected add error: %v", err)
				}
				return
			}
			okCount.Add(1)
		}()
	}
	wg.Wait()

	list, err := s.List(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, e := range list {
		total += len(e.Body)
	}
	if total > maxBytes {
		t.Fatalf("byte budget overshot: total=%d max=%d (n=%d)", total, maxBytes, len(list))
	}
	if len(list) != int(okCount.Load()) {
		t.Fatalf("list len=%d != successful adds=%d", len(list), okCount.Load())
	}
	if errCount.Load() == 0 {
		t.Fatal("expected some Adds to fail with full/budget errors")
	}
	// 90 / 40 = 2 full entries max.
	if okCount.Load() > 2 {
		t.Fatalf("successful adds=%d want <= 2", okCount.Load())
	}
}

// Concurrent Replace (and Replace+Add) must hold caps and keep unique IDs.
func TestConcurrentReplaceRespectsCaps(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const maxEntries = 4
	const maxBytes = 200
	s := &Store{DB: st.DB, MaxEntries: maxEntries, MaxBytes: maxBytes}

	// Seed three small entries.
	var seeds []*Entry
	for i := 0; i < 3; i++ {
		e, err := s.Add(ctx, "s1", KindFact, fmt.Sprintf("seed-%d", i), SourceUser, false)
		if err != nil {
			t.Fatal(err)
		}
		seeds = append(seeds, e)
	}

	var (
		wg       sync.WaitGroup
		errCount atomic.Int64
	)
	// Concurrent Replace of the same entry with larger bodies (some may exceed budget).
	const replacers = 10
	wg.Add(replacers)
	for i := 0; i < replacers; i++ {
		i := i
		go func() {
			defer wg.Done()
			// Bodies of increasing size; half stay small, half push the budget.
			n := 20
			if i%2 == 1 {
				n = 80
			}
			body := fmt.Sprintf("repl-%02d-", i) + strings.Repeat("y", n)
			_, err := s.Replace(ctx, "s1", seeds[0].ID, body, SourceUser)
			if err != nil {
				errCount.Add(1)
				// Entry may already have been replaced (new id) or byte budget.
				if !strings.Contains(err.Error(), "not found") &&
					!strings.Contains(err.Error(), "byte budget") &&
					!strings.Contains(err.Error(), "full") {
					t.Errorf("unexpected replace error: %v", err)
				}
			}
		}()
	}

	// Concurrent Adds racing with Replace — must not exceed MaxEntries/MaxBytes.
	const adders = 12
	wg.Add(adders)
	for i := 0; i < adders; i++ {
		i := i
		go func() {
			defer wg.Done()
			body := fmt.Sprintf("add-%02d-", i) + strings.Repeat("z", 30)
			_, err := s.Add(ctx, "s1", KindConstraint, body, SourceUser, false)
			if err != nil {
				errCount.Add(1)
				if !strings.Contains(err.Error(), "full") && !strings.Contains(err.Error(), "byte budget") {
					t.Errorf("unexpected add error: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	list, err := s.List(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) > maxEntries {
		t.Fatalf("entry cap overshot: len=%d max=%d", len(list), maxEntries)
	}
	total := 0
	ids := map[string]struct{}{}
	for _, e := range list {
		total += len(e.Body)
		if _, dup := ids[e.ID]; dup {
			t.Fatalf("duplicate id %q", e.ID)
		}
		ids[e.ID] = struct{}{}
	}
	if total > maxBytes {
		t.Fatalf("byte budget overshot: total=%d max=%d", total, maxBytes)
	}
	// At least the two untouched seeds (or their descendants) plus structure should remain sensible.
	if len(list) == 0 {
		t.Fatal("expected some entries to remain after concurrent replace/add")
	}
}

func TestReplaceByteBudgetExcludesOldBody(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Budget 50: one 30-byte entry can be replaced with another 30-byte body
	// (exclude old), but not with a 40-byte body when another 20-byte entry exists.
	s := &Store{DB: st.DB, MaxEntries: 10, MaxBytes: 50}
	a, err := s.Add(ctx, "s1", KindFact, strings.Repeat("a", 30), SourceUser, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(ctx, "s1", KindFact, strings.Repeat("b", 20), SourceUser, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(ctx, "s1", a.ID, strings.Repeat("c", 30), SourceUser); err != nil {
		t.Fatalf("replace within budget: %v", err)
	}
	list, err := s.List(ctx, "s1")
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	// Find the replaced entry (kind fact, body of c's).
	var replacedID string
	for _, e := range list {
		if strings.HasPrefix(e.Body, "c") {
			replacedID = e.ID
		}
	}
	if replacedID == "" {
		t.Fatalf("replaced body missing: %+v", list)
	}
	if _, err := s.Replace(ctx, "s1", replacedID, strings.Repeat("d", 40), SourceUser); err == nil {
		t.Fatal("expected byte budget error on oversized replace")
	}
	// Failed replace must leave the set intact.
	list2, err := s.List(ctx, "s1")
	if err != nil || len(list2) != 2 {
		t.Fatalf("after failed replace list=%+v err=%v", list2, err)
	}
	total := 0
	for _, e := range list2 {
		total += len(e.Body)
	}
	if total != 50 {
		t.Fatalf("total bytes=%d want 50", total)
	}
}
