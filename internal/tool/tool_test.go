package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fetched body")
	}))
	defer srv.Close()

	out, err := run(t, Fetch{}, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if err != nil || out != "fetched body" {
		t.Fatalf("fetch = %q, %v", out, err)
	}
	if _, err := run(t, Fetch{}, `{"url":"file:///etc/passwd"}`); err == nil {
		t.Error("non-http URL accepted")
	}
}

func TestRegistry(t *testing.T) {
	r := Builtins()
	defs := r.Defs()
	if len(defs) != 5 {
		t.Fatalf("builtins = %d", len(defs))
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
	got := truncate(s, 50)
	if !strings.HasPrefix(got, "aaa") || !strings.HasSuffix(got, "zzz") || !strings.Contains(got, "truncated") {
		t.Errorf("truncate = %q", got)
	}
	if truncate("short", 50) != "short" {
		t.Error("short string modified")
	}
	if len(got) > 50 {
		t.Fatalf("truncate produced %d bytes, want <= 50", len(got))
	}
}
