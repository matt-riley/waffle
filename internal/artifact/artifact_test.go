package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/matt-riley/waffle/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// The artifacts table references sessions; seed the sessions the tests
	// write against.
	for _, id := range []string{"session-1", "session-2"} {
		if _, err := st.DB.Exec(`INSERT INTO sessions (id, created_at, updated_at) VALUES (?, '', '')`, id); err != nil {
			t.Fatalf("seed session %s: %v", id, err)
		}
	}
	return New(st.DB)
}

func TestWriteGetListRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	payload := []byte("# Report\n\nFindings.")
	a, err := s.Write(ctx, "session-1", "write_artifact", "report.md", "text/markdown", payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if a.ID == "" || a.Size != int64(len(payload)) {
		t.Fatalf("artifact = %+v", a)
	}
	sum := sha256.Sum256(payload)
	if a.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %q, want sha256 of payload", a.Digest)
	}
	got, err := s.Get(ctx, "session-1", a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Payload) != string(payload) {
		t.Fatalf("payload round-trip failed")
	}
	list, err := s.List(ctx, "session-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func TestGetEnforcesSessionOwnership(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, err := s.Write(ctx, "session-1", "write_artifact", "a.txt", "text/plain", []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "session-2", a.ID); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("cross-session Get = %v, want ErrNotOwned", err)
	}
	if _, err := s.Get(ctx, "", a.ID); err != nil {
		t.Fatalf("ownerless Get: %v", err)
	}
}

func TestWriteRejectsInvalidNamesAndOversize(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, name := range []string{"", "../escape.md", "a/b.md", "bad\x01name"} {
		if _, err := s.Write(ctx, "session-1", "write_artifact", name, "text/plain", []byte("x")); err == nil {
			t.Fatalf("Write with name %q should fail", name)
		}
	}
	if _, err := s.Write(ctx, "session-1", "write_artifact", "big.bin", "application/octet-stream", make([]byte, MaxBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize Write = %v, want ErrTooLarge", err)
	}
	if _, err := s.Write(ctx, "session-1", "write_artifact", "empty.txt", "text/plain", nil); err == nil {
		t.Fatal("empty payload should fail")
	}
}

func TestVerifyDigestMarksStale(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, err := s.Write(ctx, "session-1", "write_artifact", "a.txt", "text/plain", []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyDigest(ctx, a.ID); err != nil {
		t.Fatalf("VerifyDigest on intact payload: %v", err)
	}
	// Tamper with the payload behind the store's back.
	if _, err := s.DB.ExecContext(ctx, `UPDATE artifacts SET payload = ? WHERE id = ?`, []byte("tampered"), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyDigest(ctx, a.ID); err == nil {
		t.Fatal("VerifyDigest on tampered payload must fail")
	}
	got, err := s.Get(ctx, "session-1", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateStale {
		t.Fatalf("state = %q, want stale", got.State)
	}
}

func TestWriteHonorsSessionLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	s.MaxPerSession = 2
	for i := 0; i < 2; i++ {
		if _, err := s.Write(ctx, "session-1", "write_artifact", "a.txt", "text/plain", []byte("x")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := s.Write(ctx, "session-1", "write_artifact", "b.txt", "text/plain", []byte("y")); err == nil {
		t.Fatal("third write should exceed the session limit")
	}
}
