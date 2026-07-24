package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/workset"
)

func TestMemorySearchKeepsSourceAndStableID(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	sessions := &memorySessionStore{
		turns: []session.Hit{
			{TurnID: 7, SessionID: "session-turn", Snippet: "security\n  finding", CreatedAt: now.Add(-time.Hour)},
		},
		summaries: []session.Hit{
			{SessionID: "session-summary", Snippet: "security summary", CreatedAt: now},
		},
	}
	notes := &memoryNotesStore{hits: []memory.NoteHit{
		{ID: "note-live", Snippet: "security note", NoteDate: now},
		{
			ID:       "note-archive",
			RawLine:  "- [id=note-archive] 2026-07-23 [trust=owner_stated source=secret-source session=secret-session channel=telegram untrusted=false]: archived security note",
			Archived: true,
			NoteDate: now.Add(-24 * time.Hour),
		},
	}}
	service := NewMemoryService(&Operations{Sessions: sessions, Notes: notes}, memory.Workspace{})

	first, err := service.Search(context.Background(), "security", 20)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Search(context.Background(), "security", 20)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Fatalf("merge is not deterministic:\n%#v\n%#v", first, second)
	}
	if len(first) != 4 {
		t.Fatalf("hits = %d, want 4", len(first))
	}
	if first[0].Source != MemorySourceNote || first[0].SourceID != "note-live" {
		t.Fatalf("tie precedence hit = %#v, want live note first", first[0])
	}
	for _, hit := range first {
		if hit.Source == "" || hit.SourceID == "" || hit.Excerpt == "" {
			t.Fatalf("incomplete hit: %#v", hit)
		}
	}
	if first[1].Source != MemorySourceSummary || first[1].SourceID != "session-summary" ||
		first[1].Provenance != "session:session-summary" {
		t.Fatalf("summary mapping = %#v", first[1])
	}
	if first[2].Source != MemorySourceTurn || first[2].SourceID != "7" ||
		first[2].Provenance != "session:session-turn" ||
		first[2].Excerpt != "security finding" {
		t.Fatalf("turn mapping = %#v", first[2])
	}
	archived := first[3]
	if !archived.Archived || archived.Provenance != "MEMORY.archive.md" ||
		archived.Excerpt != "archived security note" {
		t.Fatalf("archived mapping = %#v", archived)
	}
	for _, leaked := range []string{"secret-source", "secret-session", "channel=telegram"} {
		if containsMemoryHit(first, leaked) {
			t.Fatalf("search leaked raw note metadata %q: %#v", leaked, first)
		}
	}
}

func TestMemorySearchCapsMergedResultsAtTwenty(t *testing.T) {
	turns := make([]session.Hit, 0, 30)
	for index := 0; index < 30; index++ {
		turns = append(turns, session.Hit{
			TurnID:    int64(index + 1),
			SessionID: fmt.Sprintf("session-%02d", index),
			Snippet:   "bounded result",
			CreatedAt: time.Date(2026, 7, 24, 0, index, 0, 0, time.UTC),
		})
	}
	service := NewMemoryService(&Operations{
		Sessions: &memorySessionStore{turns: turns},
		Notes:    &memoryNotesStore{},
	}, memory.Workspace{})

	hits, err := service.Search(context.Background(), "bounded", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != MemorySearchLimit {
		t.Fatalf("hits = %d, want cap %d", len(hits), MemorySearchLimit)
	}
	if hits[0].SourceID != "30" || hits[len(hits)-1].SourceID != "11" {
		t.Fatalf("bounded ordering = first %q last %q", hits[0].SourceID, hits[len(hits)-1].SourceID)
	}
}

func TestMemoryAttachResolvesHitAndPreservesWorksetBounds(t *testing.T) {
	longExcerpt := bytes.Repeat([]byte("🧇"), 400)
	sessions := &memorySessionStore{
		get: map[string]*session.Session{"session-live": {ID: "session-live"}},
	}
	notes := &memoryNotesStore{hits: []memory.NoteHit{{
		ID:      "note-live",
		Snippet: string(longExcerpt),
	}}}
	worksets := &recordingMemoryWorkset{}
	service := NewMemoryService(&Operations{
		Sessions: sessions,
		Notes:    notes,
		Workset:  worksets,
	}, memory.Workspace{})

	entry, err := service.Attach(context.Background(), MemoryAttachRequest{
		SessionID: "session-live",
		Query:     "waffle",
		Source:    MemorySourceNote,
		SourceID:  "note-live",
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || entry.Kind != workset.KindFact || entry.Source != workset.SourceUser || !entry.Pinned {
		t.Fatalf("entry = %#v", entry)
	}
	if len(entry.Body) > workset.MaxEntryBytes || !utf8.ValidString(entry.Body) {
		t.Fatalf("entry body bytes=%d valid=%t", len(entry.Body), utf8.ValidString(entry.Body))
	}
	if want := "Memory [note:note-live]: "; len(entry.Body) < len(want) || entry.Body[:len(want)] != want {
		t.Fatalf("entry body = %q", entry.Body)
	}

	for _, request := range []MemoryAttachRequest{
		{SessionID: "missing", Query: "waffle", Source: MemorySourceNote, SourceID: "note-live"},
		{SessionID: "session-live", Query: "waffle", Source: MemorySourceNote, SourceID: "missing"},
		{SessionID: "session-live", Query: "waffle", Source: "provider-log", SourceID: "note-live"},
	} {
		if _, err := service.Attach(context.Background(), request); err == nil {
			t.Fatalf("Attach(%+v) unexpectedly succeeded", request)
		}
	}
	if worksets.calls != 1 {
		t.Fatalf("workset add calls = %d, want only the valid attach", worksets.calls)
	}

	worksets.err = errors.New("working set full with db=/secret")
	if _, err := service.Attach(context.Background(), MemoryAttachRequest{
		SessionID: "session-live", Query: "waffle",
		Source: MemorySourceNote, SourceID: "note-live",
	}); !errors.Is(err, ErrMemoryWorksetConflict) {
		t.Fatalf("full workset error = %v", err)
	}
}

func TestMemoryAttachNormalizesInvalidUTF8BeforeBoundingWorksetBody(t *testing.T) {
	invalidID := string([]byte{'n', 0xff, '1'})
	invalidExcerpt := string([]byte{0xff}) + strings.Repeat("x", 508) + "🧇tail"
	sessions := &memorySessionStore{
		get: map[string]*session.Session{"session-live": {ID: "session-live"}},
	}
	notes := &memoryNotesStore{hits: []memory.NoteHit{{
		ID:      invalidID,
		Snippet: invalidExcerpt,
	}}}
	worksets := &recordingMemoryWorkset{}
	service := NewMemoryService(&Operations{
		Sessions: sessions,
		Notes:    notes,
		Workset:  worksets,
	}, memory.Workspace{})

	hits, err := service.Search(context.Background(), "waffle", MemorySearchLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %#v", hits)
	}
	if !utf8.ValidString(hits[0].SourceID) || !utf8.ValidString(hits[0].Excerpt) {
		t.Fatalf("search hit retained invalid UTF-8: %#v", hits[0])
	}
	if len(hits[0].Excerpt) > MemoryExcerptMaxBytes {
		t.Fatalf("excerpt bytes = %d, want <= %d", len(hits[0].Excerpt), MemoryExcerptMaxBytes)
	}

	entry, err := service.Attach(context.Background(), MemoryAttachRequest{
		SessionID: "session-live",
		Query:     "waffle",
		Source:    MemorySourceNote,
		SourceID:  invalidID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(entry.Body) || len(entry.Body) > workset.MaxEntryBytes {
		t.Fatalf("workset body bytes=%d valid=%t: %q", len(entry.Body), utf8.ValidString(entry.Body), entry.Body)
	}
	if !strings.HasPrefix(entry.Body, "Memory [note:n\uFFFD1]: \uFFFD") {
		t.Fatalf("normalized workset prefix/body = %q", entry.Body)
	}
}

func TestMemorySearchUsesPersistedSummaryUpdatedAtForNewestFirst(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	sessions := session.New(opened)
	clock := time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)
	sessions.Now = func() time.Time { return clock }
	first, err := sessions.Create(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessions.Create(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	olderID, newerID := first.ID, second.ID
	if olderID > newerID {
		olderID, newerID = newerID, olderID
	}
	older := clock.Add(time.Hour)
	newer := clock.Add(2 * time.Hour)
	clock = older
	if err := sessions.SetSummary(ctx, olderID, "security older"); err != nil {
		t.Fatal(err)
	}
	clock = newer
	if err := sessions.SetSummary(ctx, newerID, "security newer"); err != nil {
		t.Fatal(err)
	}
	service := NewMemoryService(&Operations{
		Sessions: sessions,
		Notes:    &memoryNotesStore{},
	}, memory.Workspace{})

	hits, err := service.Search(ctx, "security", MemorySearchLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %#v", hits)
	}
	if hits[0].SourceID != newerID || !hits[0].Timestamp.Equal(newer) {
		t.Fatalf("newest summary = %#v, want id=%q timestamp=%s", hits[0], newerID, newer)
	}
}

func TestMemoryForgetPreviewIsNoteOnlyAndStatesExactExclusions(t *testing.T) {
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	previews := NewPreviewStore(func() time.Time { return clock }, &countingEntropy{})
	service := NewMemoryService(&Operations{
		Notes: &memoryNotesStore{hits: []memory.NoteHit{
			{ID: "live", Snippet: "live note"},
			{ID: "archived", Snippet: "archived note", Archived: true},
		}},
		Previews: previews,
		Now:      func() time.Time { return clock },
	}, memory.Workspace{})

	preview, err := service.PreviewForget(context.Background(), "live", "note")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Note.Source != MemorySourceNote || preview.Note.SourceID != "live" ||
		preview.PreviewToken == "" || !preview.ExpiresAt.Equal(clock.Add(MemoryForgetPreviewTTL)) {
		t.Fatalf("preview = %#v", preview)
	}
	if preview.Scope != MemoryForgetScope {
		t.Fatalf("scope = %q", preview.Scope)
	}
	for _, exclusion := range []string{"provider logs", "delivered messages", "backups"} {
		if !containsString(preview.Excludes, exclusion) {
			t.Fatalf("preview exclusions %q do not include %q", preview.Excludes, exclusion)
		}
	}
	for _, noteID := range []string{"archived", "turn-7", "unknown"} {
		if got, err := service.PreviewForget(context.Background(), noteID, "note"); err == nil || got.PreviewToken != "" {
			t.Fatalf("preview %q = %#v, %v", noteID, got, err)
		}
	}
}

func TestMemoryForgetConfirmConsumesBeforeMutation(t *testing.T) {
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	previews := NewPreviewStore(func() time.Time { return clock }, &countingEntropy{})
	forgetter := &recordingMemoryForgetter{}
	service := NewMemoryService(&Operations{
		Notes:    &memoryNotesStore{hits: []memory.NoteHit{{ID: "live", Snippet: "live note"}}},
		Previews: previews,
		Now:      func() time.Time { return clock },
	}, memory.Workspace{})
	service.forgetter = forgetter

	wrongBound, err := service.PreviewForget(context.Background(), "live", "note")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Forget(context.Background(), "other", wrongBound.PreviewToken); err == nil {
		t.Fatal("wrong note binding succeeded")
	}
	if _, err := service.Forget(context.Background(), "live", wrongBound.PreviewToken); err == nil {
		t.Fatal("burned mismatched token replay succeeded")
	}
	if forgetter.calls.Load() != 0 {
		t.Fatalf("forget calls after mismatch = %d", forgetter.calls.Load())
	}

	expiring, err := service.PreviewForget(context.Background(), "live", "note")
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(MemoryForgetPreviewTTL)
	if _, err := service.Forget(context.Background(), "live", expiring.PreviewToken); err == nil {
		t.Fatal("token valid at exact expiry")
	}
	if forgetter.calls.Load() != 0 {
		t.Fatalf("forget calls after expiry = %d", forgetter.calls.Load())
	}

	valid, err := service.PreviewForget(context.Background(), "live", "note")
	if err != nil {
		t.Fatal(err)
	}
	var successes int
	var mu sync.Mutex
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := service.Forget(context.Background(), "live", valid.PreviewToken); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	if successes != 1 || forgetter.calls.Load() != 1 {
		t.Fatalf("concurrent confirms successes=%d calls=%d", successes, forgetter.calls.Load())
	}
}

func containsMemoryHit(hits []MemoryHit, needle string) bool {
	for _, hit := range hits {
		if bytes.Contains([]byte(fmt.Sprintf("%+v", hit)), []byte(needle)) {
			return true
		}
	}
	return false
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

type memorySessionStore struct {
	turns     []session.Hit
	summaries []session.Hit
	get       map[string]*session.Session
	err       error
}

func (s *memorySessionStore) Get(_ context.Context, id string) (*session.Session, error) {
	if s.err != nil {
		return nil, s.err
	}
	if found := s.get[id]; found != nil {
		copy := *found
		return &copy, nil
	}
	return nil, session.ErrNotFound
}

func (s *memorySessionStore) Search(context.Context, string, int) ([]session.Hit, error) {
	return append([]session.Hit(nil), s.turns...), s.err
}

func (s *memorySessionStore) SearchSummaries(context.Context, string, int) ([]session.Hit, error) {
	return append([]session.Hit(nil), s.summaries...), s.err
}

type memoryNotesStore struct {
	hits []memory.NoteHit
	err  error
}

func (s *memoryNotesStore) Search(context.Context, string, int) ([]memory.NoteHit, error) {
	return append([]memory.NoteHit(nil), s.hits...), s.err
}

type recordingMemoryWorkset struct {
	calls int
	err   error
}

func (s *recordingMemoryWorkset) Add(_ context.Context, _ string, kind, body, source string, pinned bool) (*workset.Entry, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &workset.Entry{ID: "entry-1", Kind: kind, Body: body, Source: source, Pinned: pinned}, nil
}

type recordingMemoryForgetter struct {
	calls atomic.Int32
	err   error
}

func (f *recordingMemoryForgetter) ForgetNote(string) error {
	f.calls.Add(1)
	return f.err
}

type countingEntropy struct {
	mu   sync.Mutex
	next byte
}

func (r *countingEntropy) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	for index := range p {
		p[index] = r.next
	}
	return len(p), nil
}

var _ io.Reader = (*countingEntropy)(nil)
