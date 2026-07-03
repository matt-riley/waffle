package secret

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return OpenFile(filepath.Join(t.TempDir(), "secrets.age"), id)
}

func TestFileStoreRoundTrip(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("anthropic/api-key", "sk-ant-test-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("telegram/bot-token", "12345:abcdef"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	v, err := s.Get("anthropic/api-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "sk-ant-test-123" {
		t.Errorf("Get = %q", v)
	}

	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 || names[0] != "anthropic/api-key" || names[1] != "telegram/bot-token" {
		t.Errorf("List = %v", names)
	}

	if err := s.Delete("telegram/bot-token"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("telegram/bot-token"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: %v, want ErrNotFound", err)
	}
	if err := s.Delete("telegram/bot-token"); !errors.Is(err, ErrNotFound) {
		t.Errorf("double Delete: %v, want ErrNotFound", err)
	}
}

func TestFileStoreRejectsBadNames(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"", "UPPER", "sp ace", "../escape", "/lead", "trail/"} {
		if err := s.Set(name, "value"); err == nil {
			t.Errorf("Set(%q) accepted, want error", name)
		}
	}
}

func TestFileStoreWrongIdentityFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.age")
	id1, _ := age.GenerateX25519Identity()
	id2, _ := age.GenerateX25519Identity()

	if err := OpenFile(path, id1).Set("a/b", "value-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := OpenFile(path, id2).Get("a/b"); err == nil {
		t.Fatal("Get with wrong identity succeeded, want error")
	}
}

func TestFileIsEncryptedAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.age")
	id, _ := age.GenerateX25519Identity()
	if err := OpenFile(path, id).Set("a/b", "super-secret-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-value") || strings.Contains(string(raw), "a/b") {
		t.Fatal("plaintext found in secrets file")
	}
}

func TestResolve(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("anthropic/api-key", "sk-ant-xyz"); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve(s, "secret://anthropic/api-key")
	if err != nil {
		t.Fatalf("Resolve ref: %v", err)
	}
	if got != "sk-ant-xyz" {
		t.Errorf("Resolve = %q", got)
	}

	plain, err := Resolve(s, "not-a-ref")
	if err != nil || plain != "not-a-ref" {
		t.Errorf("Resolve plain = %q, %v; want passthrough", plain, err)
	}

	if _, err := Resolve(s, "secret://missing/key"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve missing = %v, want ErrNotFound", err)
	}
}

func TestRedactor(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("github/pat", "ghp_supersecrettoken"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("tiny/one", "abc"); err != nil { // below minRedactLen
		t.Fatal(err)
	}

	r, err := NewRedactor(s)
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	got := r.Redact("token is ghp_supersecrettoken and abc stays")
	want := "token is [redacted:github/pat] and abc stays"
	if got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}

func TestRedactorWithRuntimeSecrets(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("github/pat", "ghp_supersecrettoken"); err != nil {
		t.Fatal(err)
	}

	r, err := NewRedactorWith(s, NamedValue{
		Name:  "anthropic/api-key",
		Value: "sk-ant-runtime-secret",
	})
	if err != nil {
		t.Fatalf("NewRedactorWith: %v", err)
	}
	got := r.Redact("ghp_supersecrettoken and sk-ant-runtime-secret")
	want := "[redacted:github/pat] and [redacted:anthropic/api-key]"
	if got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}

func TestResolveRef(t *testing.T) {
	// literal value
	if v, err := ResolveRef("literal-value", "ENV"); err != nil || v != "literal-value" {
		t.Errorf("literal: got %q %v", v, err)
	}

	// empty -> env fallback
	t.Setenv("TEST_REF_ENV", "env-fallback")
	if v, err := ResolveRef("", "TEST_REF_ENV"); err != nil || v != "env-fallback" {
		t.Errorf("empty ref: got %q %v", v, err)
	}

	// When no store (TryOpen returns nil in this test env), ref falls back to env or empty
	if v, err := ResolveRef("secret://anything", "TEST_REF_ENV"); err != nil || v != "env-fallback" {
		t.Errorf("ref no-store with env: got %q %v", v, err)
	}
	if v, err := ResolveRef("secret://anything", "NONEXISTENT"); err != nil || v != "" {
		t.Errorf("ref no-store no-env: got %q %v", v, err)
	}
}

func TestRedactorFor(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("k", "secretval"); err != nil {
		t.Fatal(err)
	}

	r, err := RedactorFor(s, "k", "secretval")
	if err != nil || r == nil {
		t.Fatalf("RedactorFor: err=%v (r==nil)", err)
	}
	if got := r("before secretval after"); got != "before [redacted:k] after" {
		t.Errorf("redact: %q", got)
	}

	// nil cases
	if r, err := RedactorFor(nil, "", ""); err != nil || r != nil {
		t.Errorf("empty nil: got err=%v r=%T", err, r)
	}
}
