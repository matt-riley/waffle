package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/textcut"
	"github.com/matt-riley/waffle/internal/workset"
)

const (
	MemorySourceNote    = "note"
	MemorySourceSummary = "summary"
	MemorySourceTurn    = "turn"

	MemorySearchLimit       = 20
	MemoryQueryMaxBytes     = 1024
	MemoryExcerptMaxBytes   = 512
	MemoryForgetPreviewTTL  = 60 * time.Second
	MemoryForgetOperation   = "memory-forget"
	MemoryAttachedEvent     = "memory.attached"
	MemoryForgottenEvent    = "memory.forgotten"
	MemoryForgetScope       = "Affects Waffle-owned memory only."
	memorySourceLabelMaxLen = 256
)

var (
	ErrMemoryInvalidQuery      = errors.New("memory query is invalid")
	ErrMemoryInvalidSource     = errors.New("memory source is invalid")
	ErrMemoryHitNotFound       = errors.New("memory result was not found")
	ErrMemorySessionNotFound   = errors.New("memory target session was not found")
	ErrMemoryWorksetConflict   = errors.New("memory attachment exceeds the working set limits")
	ErrMemoryConfirmation      = errors.New("memory confirmation is invalid")
	ErrMemoryUnavailable       = errors.New("memory service is unavailable")
	ErrMemoryForgetUnavailable = errors.New("memory note could not be forgotten")
)

// MemoryHit is a bounded, attributed search result safe for the Desk API.
type MemoryHit struct {
	Source     string    `json:"source"`
	SourceID   string    `json:"source_id"`
	Excerpt    string    `json:"excerpt"`
	Timestamp  time.Time `json:"timestamp,omitempty"`
	Archived   bool      `json:"archived"`
	Provenance string    `json:"provenance,omitempty"`
}

// MemoryAttachRequest identifies an existing source and an explicit persisted
// session. Query is retained so the source can be resolved again server-side.
type MemoryAttachRequest struct {
	SessionID string `json:"session_id"`
	Query     string `json:"query"`
	Source    string `json:"source"`
	SourceID  string `json:"source_id"`
}

type MemoryForgetPreview struct {
	Note         MemoryHit `json:"note"`
	Scope        string    `json:"scope"`
	Excludes     []string  `json:"excludes"`
	PreviewToken string    `json:"preview_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type MemoryForgetResult struct {
	NoteID   string `json:"note_id"`
	Result   string `json:"result"`
	Archived bool   `json:"archived"`
}

type memoryNoteForgetter interface {
	ForgetNote(string) error
}

// MemoryService is the HTTP-free adapter over the existing indexed memory,
// session, working-set, preview, and event primitives.
type MemoryService struct {
	operations *Operations
	forgetter  memoryNoteForgetter
	events     *EventHub
}

func NewMemoryService(operations *Operations, workspace memory.Workspace) *MemoryService {
	service := &MemoryService{
		operations: operations,
		forgetter:  workspace,
	}
	if operations != nil {
		service.events = operations.Events
	}
	return service
}

// Search merges only turns, summaries, and curated notes. Every dependency is
// queried with the same bounded overfetch before one deterministic total sort.
func (s *MemoryService) Search(ctx context.Context, query string, limit int) ([]MemoryHit, error) {
	query, err := normalizeMemoryQuery(query)
	if err != nil {
		return nil, err
	}
	if s == nil || s.operations == nil || s.operations.Sessions == nil || s.operations.Notes == nil {
		return nil, ErrMemoryUnavailable
	}

	turns, err := s.operations.Sessions.Search(ctx, query, MemorySearchLimit)
	if err != nil {
		return nil, fmt.Errorf("%w: turns", ErrMemoryUnavailable)
	}
	summaries, err := s.operations.Sessions.SearchSummaries(ctx, query, MemorySearchLimit)
	if err != nil {
		return nil, fmt.Errorf("%w: summaries", ErrMemoryUnavailable)
	}
	notes, err := s.operations.Notes.Search(ctx, query, MemorySearchLimit)
	if err != nil {
		return nil, fmt.Errorf("%w: notes", ErrMemoryUnavailable)
	}

	hits := make([]MemoryHit, 0, len(turns)+len(summaries)+len(notes))
	for _, hit := range notes {
		if mapped, ok := mapMemoryNote(hit); ok {
			hits = append(hits, mapped)
		}
	}
	for _, hit := range summaries {
		if mapped, ok := mapMemorySummary(hit); ok {
			hits = append(hits, mapped)
		}
	}
	for _, hit := range turns {
		if mapped, ok := mapMemoryTurn(hit); ok {
			hits = append(hits, mapped)
		}
	}

	sort.SliceStable(hits, func(i, j int) bool {
		left, right := hits[i], hits[j]
		if left.Timestamp.IsZero() != right.Timestamp.IsZero() {
			return !left.Timestamp.IsZero()
		}
		if !left.Timestamp.Equal(right.Timestamp) {
			return left.Timestamp.After(right.Timestamp)
		}
		if memorySourceOrder(left.Source) != memorySourceOrder(right.Source) {
			return memorySourceOrder(left.Source) < memorySourceOrder(right.Source)
		}
		return left.SourceID < right.SourceID
	})

	if limit <= 0 || limit > MemorySearchLimit {
		limit = MemorySearchLimit
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	if hits == nil {
		hits = make([]MemoryHit, 0)
	}
	return hits, nil
}

// Attach resolves the source again, verifies the explicit persisted session,
// and creates one bounded pinned user fact without evicting existing entries.
func (s *MemoryService) Attach(ctx context.Context, request MemoryAttachRequest) (*workset.Entry, error) {
	if s == nil || s.operations == nil || s.operations.Sessions == nil || s.operations.Workset == nil {
		return nil, ErrMemoryUnavailable
	}
	sessionID := safeMemoryLabel(request.SessionID)
	if sessionID == "" {
		return nil, ErrMemorySessionNotFound
	}
	persisted, err := s.operations.Sessions.Get(ctx, sessionID)
	if err != nil || persisted == nil || safeMemoryLabel(persisted.ID) != sessionID {
		if err != nil && !errors.Is(err, session.ErrNotFound) {
			return nil, ErrMemoryUnavailable
		}
		return nil, ErrMemorySessionNotFound
	}
	hit, err := s.resolve(ctx, request.Query, request.Source, request.SourceID)
	if err != nil {
		return nil, err
	}

	prefix := strings.ToValidUTF8(
		fmt.Sprintf("Memory [%s:%s]: ", hit.Source, hit.SourceID),
		"\uFFFD",
	)
	if len(prefix) >= workset.MaxEntryBytes {
		return nil, ErrMemoryHitNotFound
	}
	excerpt := strings.ToValidUTF8(memory.OneLine(hit.Excerpt), "\uFFFD")
	body := strings.ToValidUTF8(
		prefix+textcut.Cut(excerpt, workset.MaxEntryBytes-len(prefix)),
		"\uFFFD",
	)
	entry, err := s.operations.Workset.Add(
		ctx,
		sessionID,
		workset.KindFact,
		body,
		workset.SourceUser,
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMemoryWorksetConflict, err)
	}
	s.publish(MemoryAttachedEvent, hit.Source, hit.SourceID, struct {
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
		EntryID   string `json:"entry_id"`
	}{Result: "attached", SessionID: sessionID, EntryID: safeMemoryLabel(entry.ID)})
	return entry, nil
}

// PreviewForget issues a 60-second token only for one currently live indexed
// Waffle-owned note. Other source kinds and archived notes are ineligible.
func (s *MemoryService) PreviewForget(ctx context.Context, noteID, query string) (MemoryForgetPreview, error) {
	if s == nil || s.operations == nil || s.operations.Previews == nil {
		return MemoryForgetPreview{}, ErrMemoryUnavailable
	}
	hit, err := s.resolve(ctx, query, MemorySourceNote, noteID)
	if err != nil {
		return MemoryForgetPreview{}, err
	}
	if hit.Archived {
		return MemoryForgetPreview{}, ErrMemoryHitNotFound
	}
	token := s.operations.Previews.Issue(MemoryForgetOperation, hit.SourceID, MemoryForgetPreviewTTL)
	now := time.Now()
	if s.operations.Now != nil {
		now = s.operations.Now()
	}
	return MemoryForgetPreview{
		Note:         hit,
		Scope:        MemoryForgetScope,
		Excludes:     []string{"provider logs", "delivered messages", "backups"},
		PreviewToken: token,
		ExpiresAt:    now.Add(MemoryForgetPreviewTTL),
	}, nil
}

// Forget atomically spends the exact resource-bound preview before invoking
// the canonical Workspace.ForgetNote transition.
func (s *MemoryService) Forget(_ context.Context, noteID, previewToken string) (MemoryForgetResult, error) {
	noteID = safeMemoryLabel(noteID)
	previewToken = strings.TrimSpace(previewToken)
	if s == nil || s.operations == nil || s.operations.Previews == nil || s.forgetter == nil {
		return MemoryForgetResult{}, ErrMemoryUnavailable
	}
	if noteID == "" || previewToken == "" {
		return MemoryForgetResult{}, ErrMemoryConfirmation
	}
	if err := s.operations.Previews.Consume(previewToken, MemoryForgetOperation, noteID); err != nil {
		return MemoryForgetResult{}, fmt.Errorf("%w: %v", ErrMemoryConfirmation, err)
	}
	if err := s.forgetter.ForgetNote(noteID); err != nil {
		return MemoryForgetResult{}, ErrMemoryForgetUnavailable
	}
	result := MemoryForgetResult{NoteID: noteID, Result: "forgotten", Archived: true}
	s.publish(MemoryForgottenEvent, MemorySourceNote, noteID, result)
	return result, nil
}

func (s *MemoryService) resolve(ctx context.Context, query, source, sourceID string) (MemoryHit, error) {
	query, err := normalizeMemoryQuery(query)
	if err != nil {
		return MemoryHit{}, err
	}
	source = strings.TrimSpace(source)
	sourceID = safeMemoryLabel(sourceID)
	if sourceID == "" {
		return MemoryHit{}, ErrMemoryHitNotFound
	}
	if s == nil || s.operations == nil {
		return MemoryHit{}, ErrMemoryUnavailable
	}

	switch source {
	case MemorySourceNote:
		if s.operations.Notes == nil {
			return MemoryHit{}, ErrMemoryUnavailable
		}
		hits, err := s.operations.Notes.Search(ctx, query, MemorySearchLimit)
		if err != nil {
			return MemoryHit{}, ErrMemoryUnavailable
		}
		for _, candidate := range hits {
			if mapped, ok := mapMemoryNote(candidate); ok && mapped.SourceID == sourceID {
				return mapped, nil
			}
		}
	case MemorySourceSummary:
		if s.operations.Sessions == nil {
			return MemoryHit{}, ErrMemoryUnavailable
		}
		hits, err := s.operations.Sessions.SearchSummaries(ctx, query, MemorySearchLimit)
		if err != nil {
			return MemoryHit{}, ErrMemoryUnavailable
		}
		for _, candidate := range hits {
			if mapped, ok := mapMemorySummary(candidate); ok && mapped.SourceID == sourceID {
				return mapped, nil
			}
		}
	case MemorySourceTurn:
		if s.operations.Sessions == nil {
			return MemoryHit{}, ErrMemoryUnavailable
		}
		hits, err := s.operations.Sessions.Search(ctx, query, MemorySearchLimit)
		if err != nil {
			return MemoryHit{}, ErrMemoryUnavailable
		}
		for _, candidate := range hits {
			if mapped, ok := mapMemoryTurn(candidate); ok && mapped.SourceID == sourceID {
				return mapped, nil
			}
		}
	default:
		return MemoryHit{}, ErrMemoryInvalidSource
	}
	return MemoryHit{}, ErrMemoryHitNotFound
}

func mapMemoryTurn(hit session.Hit) (MemoryHit, bool) {
	sourceID := safeMemoryLabel(strconv.FormatInt(hit.TurnID, 10))
	excerpt := safeMemoryExcerpt(hit.Snippet)
	if sourceID == "" || excerpt == "" || hit.TurnID <= 0 {
		return MemoryHit{}, false
	}
	return MemoryHit{
		Source:     MemorySourceTurn,
		SourceID:   sourceID,
		Excerpt:    excerpt,
		Timestamp:  hit.CreatedAt,
		Provenance: memorySessionProvenance(hit.SessionID),
	}, true
}

func mapMemorySummary(hit session.Hit) (MemoryHit, bool) {
	sourceID := safeMemoryLabel(hit.SessionID)
	excerpt := safeMemoryExcerpt(hit.Snippet)
	if excerpt == "" {
		excerpt = safeMemoryExcerpt(hit.Summary)
	}
	if sourceID == "" || excerpt == "" {
		return MemoryHit{}, false
	}
	return MemoryHit{
		Source:     MemorySourceSummary,
		SourceID:   sourceID,
		Excerpt:    excerpt,
		Timestamp:  hit.CreatedAt,
		Provenance: memorySessionProvenance(hit.SessionID),
	}, true
}

func mapMemoryNote(hit memory.NoteHit) (MemoryHit, bool) {
	sourceID := safeMemoryLabel(hit.ID)
	excerpt := hit.Snippet
	if excerpt == "" || (hit.RawLine != "" && excerpt == hit.RawLine) {
		excerpt = hit.Body
		if excerpt == "" {
			excerpt = memoryRawLineBody(hit.RawLine)
		}
	}
	excerpt = safeMemoryExcerpt(excerpt)
	if sourceID == "" || excerpt == "" {
		return MemoryHit{}, false
	}
	provenance := "MEMORY.md"
	if hit.Archived {
		provenance = "MEMORY.archive.md"
	}
	return MemoryHit{
		Source:     MemorySourceNote,
		SourceID:   sourceID,
		Excerpt:    excerpt,
		Timestamp:  hit.NoteDate,
		Archived:   hit.Archived,
		Provenance: provenance,
	}, true
}

func memoryRawLineBody(raw string) string {
	raw = strings.TrimSpace(raw)
	if index := strings.LastIndex(raw, "]: "); index >= 0 {
		return strings.TrimSpace(raw[index+3:])
	}
	// A metadata-shaped line that cannot be parsed is omitted rather than
	// risking provenance leakage.
	if strings.Contains(raw, "[trust=") || strings.Contains(raw, " session=") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(raw, "-"))
}

func normalizeMemoryQuery(query string) (string, error) {
	query = memory.OneLine(query)
	if query == "" || len(query) > MemoryQueryMaxBytes {
		return "", ErrMemoryInvalidQuery
	}
	return query, nil
}

func safeMemoryExcerpt(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	return textcut.Cut(memory.OneLine(sanitizeDashboardString(value)), MemoryExcerptMaxBytes)
}

func safeMemoryLabel(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	return textcut.Cut(memory.OneLine(sanitizeDashboardString(value)), memorySourceLabelMaxLen)
}

func memorySessionProvenance(sessionID string) string {
	sessionID = safeMemoryLabel(sessionID)
	if sessionID == "" {
		return ""
	}
	return "session:" + sessionID
}

func memorySourceOrder(source string) int {
	switch source {
	case MemorySourceNote:
		return 0
	case MemorySourceSummary:
		return 1
	case MemorySourceTurn:
		return 2
	default:
		return 3
	}
}

func (s *MemoryService) publish(eventType, resource, resourceID string, value any) {
	if s == nil || s.events == nil {
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	s.events.Publish(Event{
		Type:       eventType,
		Resource:   resource,
		ResourceID: safeMemoryLabel(resourceID),
		Data:       data,
	})
}
