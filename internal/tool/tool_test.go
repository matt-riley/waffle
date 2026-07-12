package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, tl Tool, input string) (string, error) {
	t.Helper()
	return tl.Run(context.Background(), json.RawMessage(input))
}

func TestBash(t *testing.T) {
	out, err := run(t, Bash{}, `{"command":"echo hello; echo err >&2"}`)
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "err") {
		t.Errorf("combined output missing streams: %q", out)
	}

	_, err = run(t, Bash{}, `{"command":"exit 3"}`)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("want exit status error, got %v", err)
	}

	_, err = run(t, Bash{}, `{"command":"sleep 5","timeout_seconds":1}`)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("want timeout error, got %v", err)
	}
}

func TestFileTools(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "f.txt")

	if _, err := run(t, WriteFile{}, fmt.Sprintf(`{"path":%q,"content":"one two one"}`, path)); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := run(t, ReadFile{}, fmt.Sprintf(`{"path":%q}`, path))
	if err != nil || out != "one two one" {
		t.Fatalf("read = %q, %v", out, err)
	}

	// Ambiguous edit must fail without replace_all.
	if _, err := run(t, EditFile{}, fmt.Sprintf(`{"path":%q,"old_string":"one","new_string":"1"}`, path)); err == nil {
		t.Error("ambiguous edit accepted")
	}
	if _, err := run(t, EditFile{}, fmt.Sprintf(`{"path":%q,"old_string":"one","new_string":"1","replace_all":true}`, path)); err != nil {
		t.Fatalf("edit: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "1 two 1" {
		t.Errorf("after edit: %q", b)
	}
	if _, err := run(t, EditFile{}, fmt.Sprintf(`{"path":%q,"old_string":"missing","new_string":"x"}`, path)); err == nil {
		t.Error("edit of missing string accepted")
	}
}

func TestWriteFilePermissions(t *testing.T) {
	dir := t.TempDir()

	// New files default to 0o600 (filtered by umask), so group/other bits
	// must never be set.
	path := filepath.Join(dir, "new.txt")
	if _, err := run(t, WriteFile{}, fmt.Sprintf(`{"path":%q,"content":"secret"}`, path)); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("new file perm = %o, want no group/other bits", perm)
	}

	// Overwriting keeps the existing mode.
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	want, err := os.Stat(existing)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if _, err := run(t, WriteFile{}, fmt.Sprintf(`{"path":%q,"content":"new"}`, existing)); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err := os.Stat(existing)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got.Mode().Perm() != want.Mode().Perm() {
		t.Errorf("overwrite changed perm from %o to %o", want.Mode().Perm(), got.Mode().Perm())
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fetched body")
	}))
	defer srv.Close()

	out, err := run(t, Fetch{AllowPrivate: []string{"127.0.0.0/8"}}, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if err != nil || out != "fetched body" {
		t.Fatalf("fetch = %q, %v", out, err)
	}
	if _, err := run(t, Fetch{}, `{"url":"file:///etc/passwd"}`); err == nil {
		t.Error("non-http URL accepted")
	}
}

func TestFetchBlocksPrivateAndAllowsHostPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	if _, err := run(t, Fetch{}, fmt.Sprintf(`{"url":%q}`, srv.URL)); err == nil ||
		!strings.Contains(err.Error(), "private/link-local range") {
		t.Fatalf("private fetch error = %v", err)
	}
	hostport := strings.TrimPrefix(srv.URL, "http://")
	out, err := run(t, Fetch{AllowPrivate: []string{hostport}}, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if err != nil || out != "ok" {
		t.Fatalf("host:port allowlist fetch = %q, %v", out, err)
	}
}

func TestFetchAddressClasses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "::1", "169.254.1.1", "fe80::1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "fc00::1", "0.0.0.0", "::"} {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatal(err)
		}

		if !blockedFetchAddr(addr) {
			t.Errorf("%s was not blocked", raw)
		}

	}
}

func TestFetchRedirectToPrivateIsRefused(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should not reach")
	}))
	defer target.Close()
	source := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer source.Close()
	allowSource := strings.TrimPrefix(source.URL, "http://")
	_, err := run(t, Fetch{AllowPrivate: []string{allowSource}}, fmt.Sprintf(`{"url":%q}`, source.URL))
	if err == nil || !strings.Contains(err.Error(), "private/link-local range") {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestSearch(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package test\nfunc Target() {}\nTarget\n")
	write("notes.txt", "Target\n")
	write(".git/config", "Target\n")
	write("binary.bin", "\x00Target\n")

	out, err := run(t, Search{}, fmt.Sprintf(`{"pattern":"Target","path":%q,"glob":"*.go"}`, dir))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out, "a.go:2: func Target() {}") || !strings.Contains(out, "a.go:3: Target") {
		t.Errorf("search output = %q", out)
	}
	if strings.Contains(out, "notes.txt") || strings.Contains(out, ".git") || strings.Contains(out, "binary.bin") {
		t.Errorf("search included excluded file: %q", out)
	}

	out, err = run(t, Search{}, fmt.Sprintf(`{"pattern":"Target","path":%q,"glob":"a.go","max_results":2}`, dir))
	if err != nil || strings.Contains(out, "results capped") {
		t.Errorf("exact result cap output = %q, %v", out, err)
	}
	write("more.go", "Target\n")
	out, err = run(t, Search{}, fmt.Sprintf(`{"pattern":"Target","path":%q,"glob":"*.go","max_results":2}`, dir))
	if err != nil || !strings.Contains(out, "results capped at 2") {
		t.Errorf("capped result output = %q, %v", out, err)
	}

	if _, err := run(t, Search{}, fmt.Sprintf(`{"pattern":"[","path":%q}`, dir)); err == nil || !strings.Contains(err.Error(), "invalid pattern") {
		t.Errorf("invalid regexp error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Search{}).Run(ctx, json.RawMessage(fmt.Sprintf(`{"pattern":"Target","path":%q}`, dir))); err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled search error = %v", err)
	}
}

func TestRegistry(t *testing.T) {
	r := Builtins()
	defs := r.Defs()
	if len(defs) != 6 {
		t.Fatalf("builtins = %d", len(defs))
	}
	if defs[5].Name != "search" {
		t.Errorf("last builtin = %q, want search", defs[5].Name)
	}
	for _, d := range defs {
		var schema map[string]any
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			t.Errorf("tool %s: bad schema: %v", d.Name, err)
		}
	}
	if _, err := r.Run(context.Background(), "nope", nil); err == nil {
		t.Error("unknown tool did not error")
	}
}

func TestTruncateKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("a", 100) + strings.Repeat("z", 100)
	got := Truncate(s, 50)
	if !strings.HasPrefix(got, "aaa") || !strings.HasSuffix(got, "zzz") || !strings.Contains(got, "truncated") {
		t.Errorf("truncate = %q", got)
	}
	if Truncate("short", 50) != "short" {
		t.Error("short string modified")
	}
	if len(got) > 50 {
		t.Fatalf("truncate produced %d bytes, want <= 50", len(got))
	}
}
