package modelcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var cacheTestNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

func TestStoreFreshHitMakesNoRefreshRequest(t *testing.T) {
	store := newTestStore(t)
	connection := testConnection("primary")
	wantModels := []Model{{ID: "z-model"}, {ID: "a-model"}}
	if err := store.Save(connection, wantModels, cacheTestNow.Add(-time.Hour)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var calls atomic.Int32
	result, err := store.GetOrRefresh(t.Context(), Connection{
		Name:    connection.Name,
		Type:    connection.Type,
		BaseURL: "https://api.example.com",
		ScopeID: connection.ScopeID,
	}, false, func(context.Context) ([]Model, error) {
		calls.Add(1)
		return nil, errors.New("unexpected refresh")
	})
	if err != nil {
		t.Fatalf("GetOrRefresh() error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("refresh calls = %d, want 0", calls.Load())
	}
	if result.Age != time.Hour || result.Stale {
		t.Fatalf("result age/stale = %v/%t, want %v/false", result.Age, result.Stale, time.Hour)
	}
	if got := modelIDs(result.Models); !equalStrings(got, []string{"a-model", "z-model"}) {
		t.Fatalf("model IDs = %v, want normalized order", got)
	}
}

func TestStoreExpiredAndForcedRefreshMakeOneRequest(t *testing.T) {
	tests := []struct {
		name      string
		fetchedAt time.Time
		force     bool
	}{
		{name: "expired", fetchedAt: cacheTestNow.Add(-DefaultTTL), force: false},
		{name: "forced", fetchedAt: cacheTestNow.Add(-time.Hour), force: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			connection := testConnection("primary")
			if err := store.Save(connection, []Model{{ID: "old"}}, tt.fetchedAt); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			var calls atomic.Int32
			result, err := store.GetOrRefresh(t.Context(), connection, tt.force, func(context.Context) ([]Model, error) {
				calls.Add(1)
				return []Model{{ID: "new"}}, nil
			})
			if err != nil {
				t.Fatalf("GetOrRefresh() error = %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("refresh calls = %d, want 1", calls.Load())
			}
			if result.FetchedAt != cacheTestNow || result.Age != 0 || result.Stale {
				t.Fatalf("refreshed result = %+v, want fresh record at fixed clock", result)
			}
			if got := modelIDs(result.Models); !equalStrings(got, []string{"new"}) {
				t.Fatalf("model IDs = %v, want [new]", got)
			}
		})
	}
}

func TestStoreConcurrentRefreshMakesOneRequest(t *testing.T) {
	home := t.TempDir()
	stores := []*Store{NewStore(home), NewStore(home)}
	for _, store := range stores {
		store.Now = func() time.Time { return cacheTestNow }
	}
	connection := testConnection("primary")
	if err := stores[0].Save(connection, []Model{{ID: "old"}}, cacheTestNow.Add(-time.Hour)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	fetch := func(ctx context.Context) ([]Model, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return []Model{{ID: "new"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	start := make(chan struct{})
	results := make(chan Result, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			<-start
			result, err := store.GetOrRefresh(t.Context(), connection, true, fetch)
			results <- result
			errs <- err
		}(store)
	}
	close(start)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	// Give the second store time to read the old record and contend on the lock.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("GetOrRefresh() error = %v", err)
		}
	}
	for result := range results {
		if got := modelIDs(result.Models); !equalStrings(got, []string{"new"}) {
			t.Fatalf("model IDs = %v, want [new]", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", calls.Load())
	}
}

func TestStoreRefreshFailureReturnsStaleRecord(t *testing.T) {
	store := newTestStore(t)
	connection := testConnection("primary")
	if err := store.Save(connection, []Model{{ID: "cached"}}, cacheTestNow.Add(-25*time.Hour)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	result, err := store.GetOrRefresh(t.Context(), connection, false, func(context.Context) ([]Model, error) {
		return nil, errors.New("credential=super-secret\nupstream failed")
	})
	if err != nil {
		t.Fatalf("GetOrRefresh() error = %v, want stale fallback", err)
	}
	if result.Age != 25*time.Hour || !result.Stale {
		t.Fatalf("result age/stale = %v/%t, want %v/true", result.Age, result.Stale, 25*time.Hour)
	}
	if result.Warning == "" {
		t.Fatal("warning is empty")
	}
	if strings.Contains(result.Warning, "super-secret") || strings.ContainsAny(result.Warning, "\r\n") {
		t.Fatalf("warning is not sanitized: %q", result.Warning)
	}
	if got := modelIDs(result.Models); !equalStrings(got, []string{"cached"}) {
		t.Fatalf("model IDs = %v, want [cached]", got)
	}
}

func TestStoreRejectsMismatchedCorruptAndSymlinkRecords(t *testing.T) {
	t.Run("mismatched connection", func(t *testing.T) {
		store := newTestStore(t)
		connection := testConnection("primary")
		if err := store.Save(connection, []Model{{ID: "cached"}}, cacheTestNow); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		mismatches := []Connection{
			{Name: connection.Name, Type: "anthropic", BaseURL: connection.BaseURL, ScopeID: connection.ScopeID},
			{Name: connection.Name, Type: connection.Type, BaseURL: "https://other.example.com", ScopeID: connection.ScopeID},
			{Name: connection.Name, Type: connection.Type, BaseURL: connection.BaseURL, ScopeID: "other-account"},
		}
		for _, mismatch := range mismatches {
			if _, err := store.Load(mismatch); err == nil {
				t.Fatalf("Load(%+v) error = nil, want mismatch rejection", mismatch)
			}
		}
	})

	t.Run("schema mismatch", func(t *testing.T) {
		store := newTestStore(t)
		connection := testConnection("primary")
		writeRawRecord(t, store, connection.Name, `{"schema_version":2,"connection":{"name":"primary","type":"openai","base_url":"https://api.example.com","scope_id":"account-123"},"fetched_at":"2026-07-19T12:00:00Z","models":[]}`)
		if _, err := store.Load(connection); err == nil {
			t.Fatal("Load() error = nil, want schema rejection")
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		store := newTestStore(t)
		connection := testConnection("primary")
		writeRawRecord(t, store, connection.Name, `{not-json`)
		if _, err := store.Load(connection); err == nil {
			t.Fatal("Load() error = nil, want corrupt record rejection")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		store := newTestStore(t)
		connection := testConnection("primary")
		if err := os.MkdirAll(store.Root, 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		target := filepath.Join(t.TempDir(), "target.json")
		if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if err := os.Symlink(target, filepath.Join(store.Root, connection.Name+".json")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := store.Load(connection); err == nil {
			t.Fatal("Load() error = nil, want symlink rejection")
		}
	})
}

func TestStoreWritesPrivateModesAtomically(t *testing.T) {
	store := newTestStore(t)
	connection := testConnection("primary")
	if err := store.Save(connection, []Model{{ID: "model"}}, cacheTestNow); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	dirInfo, err := os.Stat(store.Root)
	if err != nil {
		t.Fatalf("Stat(root) error = %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("root mode = %04o, want 0700", got)
	}
	recordPath := filepath.Join(store.Root, connection.Name+".json")
	recordInfo, err := os.Lstat(recordPath)
	if err != nil {
		t.Fatalf("Lstat(record) error = %v", err)
	}
	if !recordInfo.Mode().IsRegular() || recordInfo.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %v, want regular 0600", recordInfo.Mode())
	}
	entries, err := os.ReadDir(store.Root)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != connection.Name+".json" {
		t.Fatalf("cache entries = %v, want only committed record", entryNames(entries))
	}
	b, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if bytes.Contains(b, []byte(`"warning"`)) {
		t.Fatalf("stored record serialized a warning: %s", b)
	}
	var record Record
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatalf("stored JSON is invalid: %v", err)
	}
	if record.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", record.SchemaVersion, SchemaVersion)
	}

	withoutScope := connection
	withoutScope.ScopeID = ""
	if err := store.Save(withoutScope, []Model{{ID: "model"}}, cacheTestNow); err == nil {
		t.Fatal("Save() error = nil, want empty scope rejection")
	}
}

func TestStoreFailedRefreshPreservesGoodBytes(t *testing.T) {
	store := newTestStore(t)
	connection := testConnection("primary")
	if err := store.Save(connection, []Model{{ID: "cached"}}, cacheTestNow.Add(-25*time.Hour)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	path := filepath.Join(store.Root, connection.Name+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}

	result, err := store.GetOrRefresh(t.Context(), connection, false, func(context.Context) ([]Model, error) {
		return nil, errors.New("refresh failed")
	})
	if err != nil || !result.Stale {
		t.Fatalf("GetOrRefresh() result/error = %+v/%v, want stale fallback", result, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("cache bytes changed after failed refresh\nbefore: %s\nafter: %s", before, after)
	}
}

func TestStoreInvalidateRemovesOnlyNamedConnection(t *testing.T) {
	store := newTestStore(t)
	primary := testConnection("primary")
	secondary := testConnection("secondary")
	if err := store.Save(primary, []Model{{ID: "primary-model"}}, cacheTestNow); err != nil {
		t.Fatalf("Save(primary) error = %v", err)
	}
	if err := store.Save(secondary, []Model{{ID: "secondary-model"}}, cacheTestNow); err != nil {
		t.Fatalf("Save(secondary) error = %v", err)
	}

	if err := store.Invalidate(primary.Name); err != nil {
		t.Fatalf("Invalidate() error = %v", err)
	}
	if _, err := store.Load(primary); err == nil {
		t.Fatal("Load(primary) error = nil after invalidation")
	}
	result, err := store.Load(secondary)
	if err != nil {
		t.Fatalf("Load(secondary) error = %v", err)
	}
	if got := modelIDs(result.Models); !equalStrings(got, []string{"secondary-model"}) {
		t.Fatalf("secondary model IDs = %v", got)
	}
	if err := store.Invalidate("../secondary"); err == nil {
		t.Fatal("Invalidate(path traversal) error = nil")
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store := NewStore(t.TempDir())
	store.Now = func() time.Time { return cacheTestNow }
	return store
}

func testConnection(name string) Connection {
	return Connection{
		Name:    name,
		Type:    "openai",
		BaseURL: "HTTPS://API.EXAMPLE.COM///",
		ScopeID: "account-123",
	}
}

func writeRawRecord(t *testing.T, store *Store, connection, contents string) {
	t.Helper()
	if err := os.MkdirAll(store.Root, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.Root, connection+".json"), []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func modelIDs(models []Model) []string {
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
	}
	return ids
}

func equalStrings(left, right []string) bool {
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}
