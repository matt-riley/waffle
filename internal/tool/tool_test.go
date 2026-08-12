package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

type sequenceResolver struct {
	answers [][]netip.Addr
	calls   int
}

func (r *sequenceResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	answer := r.answers[r.calls]
	r.calls++
	return answer, nil
}

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

func TestFileToolsRejectOversizedContent(t *testing.T) {
	dir := t.TempDir()
	tooLarge := strings.Repeat("x", fileContentMaxBytes+1)

	writePath := filepath.Join(dir, "write.txt")
	writeInput, err := json.Marshal(map[string]string{
		"path":    writePath,
		"content": tooLarge,
	})
	if err != nil {
		t.Fatalf("marshal write input: %v", err)
	}
	if _, err := run(t, WriteFile{}, string(writeInput)); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("maximum %d bytes", fileContentMaxBytes)) {
		t.Fatalf("oversized write error = %v", err)
	}
	if _, err := os.Stat(writePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized write created %s: %v", writePath, err)
	}

	editPath := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(editPath, []byte("old content"), 0o600); err != nil {
		t.Fatalf("setup edit file: %v", err)
	}
	editInput, err := json.Marshal(map[string]any{
		"path":       editPath,
		"old_string": "old",
		"new_string": tooLarge,
	})
	if err != nil {
		t.Fatalf("marshal edit input: %v", err)
	}
	if _, err := run(t, EditFile{}, string(editInput)); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("maximum %d bytes", fileContentMaxBytes)) {
		t.Fatalf("oversized edit error = %v", err)
	}
	if got, err := os.ReadFile(editPath); err != nil || string(got) != "old content" {
		t.Fatalf("oversized edit changed file to %q (%v)", got, err)
	}
}

func TestFileToolDefinitionsDescribeContentLimit(t *testing.T) {
	defs := []struct {
		name        string
		description string
		schema      json.RawMessage
	}{
		{name: "write_file", description: WriteFile{}.Def().Description, schema: WriteFile{}.Def().InputSchema},
		{name: "edit_file", description: EditFile{}.Def().Description, schema: EditFile{}.Def().InputSchema},
	}
	for _, tc := range defs {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.description, "2 MiB") {
				t.Errorf("description = %q, want content limit", tc.description)
			}
			if !strings.Contains(string(tc.schema), "maximum 2 MiB") {
				t.Errorf("schema = %s, want content limit", tc.schema)
			}
		})
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

// TestFileWritesAreAtomic pins the write-then-rename contract (#264): the
// target is swapped in whole, never truncated in place, and no staging file is
// left behind.
func TestFileWritesAreAtomic(t *testing.T) {
	tests := []struct {
		name  string
		tool  Tool
		input func(path string) string
		want  string
	}{
		{
			name:  "edit_file",
			tool:  EditFile{},
			input: func(p string) string { return fmt.Sprintf(`{"path":%q,"old_string":"old","new_string":"new"}`, p) },
			want:  "new content",
		},
		{
			name:  "write_file",
			tool:  WriteFile{},
			input: func(p string) string { return fmt.Sprintf(`{"path":%q,"content":"new content"}`, p) },
			want:  "new content",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f.txt")
			if err := os.WriteFile(path, []byte("old content"), 0o640); err != nil {
				t.Fatalf("setup: %v", err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if _, err := run(t, tc.tool, tc.input(path)); err != nil {
				t.Fatalf("run: %v", err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			// A rename swaps in a different inode; an in-place O_TRUNC rewrite
			// would keep the same one, which is the corruption window.
			if os.SameFile(before, after) {
				t.Error("file was rewritten in place, want atomic rename")
			}
			if got, _ := os.ReadFile(path); string(got) != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
			if after.Mode().Perm() != before.Mode().Perm() {
				t.Errorf("perm changed from %o to %o", before.Mode().Perm(), after.Mode().Perm())
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("readdir: %v", err)
			}
			if len(entries) != 1 {
				t.Errorf("staging file left behind: %v", entries)
			}
		})
	}
}

// TestEditFileFollowsSymlink: renaming over a symlink would replace the link
// itself, so the target must be resolved first (#264).
func TestEditFileFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := run(t, EditFile{}, fmt.Sprintf(`{"path":%q,"old_string":"old","new_string":"new"}`, link)); err != nil {
		t.Fatalf("edit: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a regular file")
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Errorf("target = %q, want new", got)
	}
}

// TestEditFileFailureKeepsOriginal: a write that cannot be staged must leave
// the original readable, not a truncated remnant (#264).
func TestEditFileFailureKeepsOriginal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("old content"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := run(t, EditFile{}, fmt.Sprintf(`{"path":%q,"old_string":"old","new_string":"new"}`, path)); err == nil {
		t.Fatal("edit succeeded in an unwritable directory")
	}
	if got, _ := os.ReadFile(path); string(got) != "old content" {
		t.Errorf("original damaged: %q", got)
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fetched body")
	}))
	defer srv.Close()

	out, err := run(t, &Fetch{AllowPrivate: []string{"127.0.0.0/8"}}, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if err != nil || out != "fetched body" {
		t.Fatalf("fetch = %q, %v", out, err)
	}
	if _, err := run(t, &Fetch{}, `{"url":"file:///etc/passwd"}`); err == nil {
		t.Error("non-http URL accepted")
	}
}

func TestFetchBlocksPrivateAndAllowsHostPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	if _, err := run(t, &Fetch{}, fmt.Sprintf(`{"url":%q}`, srv.URL)); err == nil ||
		!strings.Contains(err.Error(), "private/link-local range") {
		t.Fatalf("private fetch error = %v", err)
	}
	hostport := strings.TrimPrefix(srv.URL, "http://")
	out, err := run(t, &Fetch{AllowPrivate: []string{hostport}}, fmt.Sprintf(`{"url":%q}`, srv.URL))
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
	_, err := run(t, &Fetch{AllowPrivate: []string{allowSource}}, fmt.Sprintf(`{"url":%q}`, source.URL))
	if err == nil || !strings.Contains(err.Error(), "private/link-local range") {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestFetchRejectsDNSRebindAtDialTime(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		fmt.Fprint(w, "must not be returned")
	}))
	defer srv.Close()
	port := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[1]
	resolver := &sequenceResolver{answers: [][]netip.Addr{
		{netip.MustParseAddr("93.184.216.34")}, // initial policy/preflight resolution
		{netip.MustParseAddr("127.0.0.1")},     // rebound answer used by DialContext
	}}
	if first, err := resolver.LookupNetIP(context.Background(), "ip", "rebind.test"); err != nil || first[0].IsLoopback() {
		t.Fatalf("preflight answer = %v, %v; want public", first, err)
	}
	_, err := run(t, &Fetch{Resolver: resolver}, fmt.Sprintf(`{"url":"http://rebind.test:%s/secret"}`, port))
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1") || !strings.Contains(err.Error(), "allow_private") {
		t.Fatalf("dial-time rebind error = %v", err)
	}
	if reached {
		t.Fatal("DNS rebound request reached loopback and returned a partial body")
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
	if len(defs) != 7 {
		t.Fatalf("builtins = %d", len(defs))
	}
	if defs[5].Name != "search" {
		t.Errorf("search builtin = %q, want search", defs[5].Name)
	}
	if defs[6].Name != "list_files" {
		t.Errorf("last builtin = %q, want list_files", defs[6].Name)
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

// TestTruncateUTF8Safe ensures head/tail cuts never split multi-byte runes (#107).
func TestTruncateUTF8Safe(t *testing.T) {
	// Mix ASCII with 4-byte emoji so many byte limits land mid-rune.
	s := "hi " + strings.Repeat("🌍", 40) + " mid " + strings.Repeat("🎉", 40) + " end"
	for _, limit := range []int{1, 2, 3, 4, 5, 10, 17, 31, 50, 80, 100, 200} {
		got := Truncate(s, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("Truncate(limit=%d) invalid UTF-8: %q (%v)", limit, got, []byte(got))
		}
		if len(got) > limit {
			t.Fatalf("Truncate(limit=%d) len=%d > limit", limit, len(got))
		}
	}
	// café: é is 2 bytes; force mid-rune cut on a short budget.
	cafe := strings.Repeat("café ", 30)
	got := Truncate(cafe, 25)
	if !utf8.ValidString(got) {
		t.Fatalf("café Truncate invalid: %q", got)
	}
	if len(got) > 25 {
		t.Fatalf("café Truncate len=%d > 25", len(got))
	}
	// Below limit: unchanged.
	if Truncate("café 🌍", 100) != "café 🌍" {
		t.Fatal("short multi-byte string modified")
	}
}

// TestHostBuiltinsReturnPastOutputLimit ensures Bash/ReadFile do not apply
// OutputLimit truncation themselves — Agent.runOne spills then truncates (#69).
func TestHostBuiltinsReturnPastOutputLimit(t *testing.T) {
	// Bash: produce more than OutputLimit bytes of ASCII.
	n := OutputLimit + 500
	out, err := run(t, Bash{}, fmt.Sprintf(`{"command":"python3 -c \"import sys; sys.stdout.write('B'*%d)\""}`, n))
	if err != nil {
		// Fallback when python3 is missing (rare on CI/mac).
		out, err = run(t, Bash{}, fmt.Sprintf(`{"command":"yes B | tr -d '\\n' | head -c %d"}`, n))
		if err != nil {
			t.Fatalf("bash large: %v", err)
		}
	}
	if len(out) <= OutputLimit {
		t.Fatalf("bash returned %d bytes, want > OutputLimit (%d) so spill can run", len(out), OutputLimit)
	}
	if len(out) > HostReturnCap {
		t.Fatalf("bash returned %d bytes, want <= HostReturnCap (%d)", len(out), HostReturnCap)
	}

	// ReadFile: large temp file returns full content past OutputLimit.
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	body := strings.Repeat("R", OutputLimit+800)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := run(t, ReadFile{}, fmt.Sprintf(`{"path":%q}`, path))
	if err != nil {
		t.Fatalf("read_file large: %v", err)
	}
	if len(got) <= OutputLimit {
		t.Fatalf("read_file returned %d bytes, want > OutputLimit", len(got))
	}
	if got != body {
		t.Fatalf("read_file content mismatch: len=%d want %d", len(got), len(body))
	}
}

func TestCapHostReturn(t *testing.T) {
	if CapHostReturn("short") != "short" {
		t.Fatal("short modified")
	}
	huge := strings.Repeat("H", HostReturnCap+100)
	got := CapHostReturn(huge)
	if len(got) != HostReturnCap {
		t.Fatalf("cap len=%d want %d", len(got), HostReturnCap)
	}
}

func TestFetchReusesTransportAcrossRuns(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	f := &Fetch{AllowPrivate: []string{"127.0.0.0/8"}}
	for i := 0; i < 3; i++ {
		if out, err := run(t, f, fmt.Sprintf(`{"url":%q}`, srv.URL)); err != nil || !strings.Contains(out, "ok") {
			t.Fatalf("run %d: %q %v", i, out, err)
		}
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("server hits = %d, want 3", got)
	}
	// The transport must be built once and cached, not recreated per Run.
	if f.cachedTransport == nil {
		t.Fatal("cached transport was not initialized")
	}
	first := f.cachedTransport
	if _, err := run(t, f, fmt.Sprintf(`{"url":%q}`, srv.URL)); err != nil {
		t.Fatal(err)
	}
	if f.cachedTransport != first {
		t.Fatal("transport was rebuilt on a later Run")
	}
}

// TestReadFileOmittingRangesIsByteIdentical pins the #256 backward-compat
// contract: a read_file call without offset or limit must return exactly the
// same bytes as before ranges existed (asserted by the pre-existing tests
// passing unmodified).
func TestReadFileOmittingRangesIsByteIdentical(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "plain", body: "one two one"},
		{name: "CRLF", body: "a\r\nb\r\n"},
		{name: "NUL byte", body: "a\x00b\n"},
		{name: "no trailing newline", body: "line1\nline2"},
		{name: "empty", body: ""},
		{name: "invalid UTF-8", body: "caf\xc3\x28\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.txt")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := run(t, ReadFile{}, fmt.Sprintf(`{"path":%q}`, path))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if out != tc.body {
				t.Fatalf("read = %q, want byte-identical %q", out, tc.body)
			}
		})
	}
}

// TestReadFileRanges covers the #256 range semantics: 1-indexed line numbers,
// offset beyond EOF, negative values, zero limit, limit exceeding remaining
// lines, a single-line file, no trailing newline, CRLF, NUL bytes, and the
// total line count footer.
func TestReadFileRanges(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		offset  int
		limit   int
		want    string
		wantErr string
	}{
		{name: "offset beyond EOF", body: "a\nb\nc\n", offset: 10, want: "[3 total lines; showing 0]"},
		{name: "negative offset", body: "a\n", offset: -1, wantErr: "offset must not be negative"},
		{name: "negative limit", body: "a\n", limit: -1, wantErr: "limit must not be negative"},
		{name: "zero limit reads all remaining", body: "a\nb\nc\n", offset: 2, limit: 0, want: "2: b\n3: c\n[3 total lines; showing 2]"},
		{name: "limit exceeding remaining", body: "a\nb\nc\n", offset: 2, limit: 100, want: "2: b\n3: c\n[3 total lines; showing 2]"},
		{name: "single-line file", body: "only\n", offset: 1, limit: 1, want: "1: only\n[1 total lines; showing 1]"},
		{name: "no trailing newline", body: "a\nb", offset: 2, limit: 1, want: "2: b\n[2 total lines; showing 1]"},
		{name: "CRLF normalized in range", body: "a\r\nb\r\n", offset: 1, limit: 2, want: "1: a\n2: b\n[2 total lines; showing 2]"},
		{name: "NUL byte verbatim", body: "a\x00b\nc\n", offset: 1, limit: 1, want: "1: a\x00b\n[2 total lines; showing 1]"},
		{name: "empty file", body: "", offset: 1, want: "[0 total lines; showing 0]"},
		{name: "offset zero defaults to first line", body: "a\nb\n", offset: 0, limit: 1, want: "1: a\n[2 total lines; showing 1]"},
		{name: "middle range", body: "a\nb\nc\nd\n", offset: 2, limit: 2, want: "2: b\n3: c\n[4 total lines; showing 2]"},
		{name: "range footer states total lines", body: "a\nb\nc\nd\ne\n", offset: 3, limit: 1, want: "3: c\n[5 total lines; showing 1]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.txt")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			input, err := json.Marshal(map[string]any{"path": path, "offset": tc.offset, "limit": tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			out, err := run(t, ReadFile{}, string(input))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if out != tc.want {
				t.Fatalf("read = %q, want %q", out, tc.want)
			}
		})
	}
}

// TestReadFileRangeRespectsHostReturnCap pins the #256 truncation contract: a
// range selecting more than HostReturnCap bytes is cut with an explicit
// marker, the cut lands on a UTF-8 rune boundary (textcut), and the marker
// states the total line count. The fixture is sized so the cumulative
// numbered output lands inside the marker-reserve window, forcing the
// textcut cut path.
func TestReadFileRangeRespectsHostReturnCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	base := strings.Repeat("🌍", 407) // ~1.6 KiB per line of 4-byte runes

	// Build the fixture so the cumulative numbered output lands exactly one
	// byte under the cap after the final emitted line, with one more line
	// after it that cannot fit: the marker-reserve window is only ~70 bytes
	// wide (far smaller than one line), so this forces the textcut cut path.
	k := 0
	emitted := 0
	for {
		lineLen := len(fmt.Sprintf("%d: %s\n", k+1, base))
		if emitted+lineLen > HostReturnCap-2 {
			break
		}
		emitted += lineLen
		k++
	}
	numPrefix := fmt.Sprintf("%d: ", k+1)
	filler := HostReturnCap - 1 - emitted - len(numPrefix) - 1 // minus trailing "\n"
	// End the sized line with emoji so the marker-reserve cut (~69 bytes
	// before the end) lands mid-rune and textcut must retreat.
	lineK1 := strings.Repeat("x", filler-68) + strings.Repeat("🌍", 17)
	if filler < 68 {
		t.Fatalf("fixture filler=%d too small", filler)
	}
	var sb strings.Builder
	for i := 0; i < k; i++ {
		sb.WriteString(base)
		sb.WriteString("\n")
	}
	sb.WriteString(lineK1)
	sb.WriteString("\n")
	sb.WriteString("z\n") // one byte over the cap once the sized line is emitted
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	total, shown := k+2, k+1
	marker := fmt.Sprintf("\n... [output truncated at %d bytes; %d total lines; showing %d]", HostReturnCap, total, shown)
	cumulative := emitted + len(numPrefix) + filler + 1 // one byte under the cap
	if cumulative <= HostReturnCap-len(marker) {
		t.Fatalf("fixture cumulative=%d does not exercise the textcut cut path (window > %d)", cumulative, HostReturnCap-len(marker))
	}

	out, err := run(t, ReadFile{}, fmt.Sprintf(`{"path":%q,"offset":1,"limit":0}`, path))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasSuffix(out, marker) {
		t.Fatalf("output missing truncation marker %q:\n%.200s...", marker, out)
	}
	if len(out) > HostReturnCap {
		t.Fatalf("output len=%d exceeds HostReturnCap %d", len(out), HostReturnCap)
	}
	// The cut branch ran (not the append branch), so the prefix was cut by
	// textcut and must be valid UTF-8 with no split rune.
	if len(out) <= HostReturnCap-len(marker) {
		t.Fatalf("output len=%d, want the textcut cut path (len > %d)", len(out), HostReturnCap-len(marker))
	}
	prefix := out[:len(out)-len(marker)]
	if !utf8.ValidString(prefix) {
		t.Fatalf("truncated prefix is invalid UTF-8: %q", prefix)
	}
}

// TestReadFileRangeInvalidUTF8 pins #256's untrusted-data handling: invalid
// UTF-8 inside the selected range is returned verbatim (exactly as the raw
// path does), and the numbered wrapper never corrupts it.
func TestReadFileRangeInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.txt")
	body := "caf\xc3\x28\nok\n" // \xc3\x28 is an invalid UTF-8 sequence
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, ReadFile{}, fmt.Sprintf(`{"path":%q,"offset":1,"limit":2}`, path))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "1: caf\xc3\x28\n2: ok\n[2 total lines; showing 2]"
	if out != want {
		t.Fatalf("read = %q, want %q", out, want)
	}
}

// TestReadFileRangeLongLineError pins the bounded-memory guarantee: a line
// beyond readMaxLineBytes is refused with a clear error naming the line, and
// the raw (unranged) path still reads such files.
func TestReadFileRangeLongLineError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long.txt")
	body := "short\n" + strings.Repeat("x", readMaxLineBytes+1) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, ReadFile{}, fmt.Sprintf(`{"path":%q,"offset":1}`, path)); err == nil ||
		!strings.Contains(err.Error(), "line 2 exceeds") {
		t.Fatalf("long line error = %v", err)
	}
	// The raw path has no line-length bound and must still read the file
	// (capped at HostReturnCap as before ranges existed).
	out, err := run(t, ReadFile{}, fmt.Sprintf(`{"path":%q}`, path))
	if err != nil {
		t.Fatalf("raw read of long line: %v", err)
	}
	if out != body[:HostReturnCap] {
		t.Fatalf("raw read mismatch: len=%d want %d", len(out), HostReturnCap)
	}
}

// TestListFiles exercises the #256 listing tool against a fixture tree:
// type+size rows, basename glob, VCS-directory skips, deterministic sorting,
// the entry cap, and the distinct non-existent vs not-a-directory errors.
func TestListFiles(t *testing.T) {
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
	write("a.go", "package a\n")
	write("notes.txt", "notes\n")
	write("sub/b.go", "package b\n")
	write("sub/deep/c.txt", "c\n")
	write(".git/config", "ignored\n")
	write(".hg/x", "ignored\n")
	write(".svn/y", "ignored\n")
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(dir, loop); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	row := func(rel, typ string) string {
		t.Helper()
		info, err := os.Lstat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s\t%s\t%d", filepath.Join(dir, rel), typ, info.Size())
	}
	all := []string{
		row("a.go", "file"),
		row("loop", "symlink"),
		row("notes.txt", "file"),
		row("sub", "dir"),
		row(filepath.Join("sub", "b.go"), "file"),
		row(filepath.Join("sub", "deep"), "dir"),
		row(filepath.Join("sub", "deep", "c.txt"), "file"),
	}
	sort.Strings(all)

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{
			name:  "full tree with type and size",
			input: fmt.Sprintf(`{"path":%q}`, dir),
			want:  strings.Join(all, "\n"),
		},
		{
			name:  "basename glob",
			input: fmt.Sprintf(`{"path":%q,"glob":"*.go"}`, dir),
			want:  strings.Join([]string{all[0], all[4]}, "\n"),
		},
		{
			name:  "glob no match",
			input: fmt.Sprintf(`{"path":%q,"glob":"*.rs"}`, dir),
			want:  "(no entries)",
		},
		{
			name:    "invalid glob is a clear error",
			input:   fmt.Sprintf(`{"path":%q,"glob":"["}`, dir),
			wantErr: "invalid glob",
		},
		{
			name:    "non-existent path",
			input:   fmt.Sprintf(`{"path":%q}`, filepath.Join(dir, "missing")),
			wantErr: "does not exist",
		},
		{
			name:    "file is not a directory",
			input:   fmt.Sprintf(`{"path":%q}`, filepath.Join(dir, "a.go")),
			wantErr: "not a directory",
		},
		{
			name:  "capped with marker",
			input: fmt.Sprintf(`{"path":%q,"max_results":2}`, dir),
			want:  strings.Join(all[:2], "\n") + "\n... [listing capped at 2]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run(t, ListFiles{}, tc.input)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if out != tc.want {
				t.Fatalf("list = %q, want %q", out, tc.want)
			}
		})
	}
}

// TestListFilesSkipsVCSEverywhere pins the Search parity: VCS directories are
// skipped at every depth, not only at the listing root.
func TestListFilesSkipsVCSEverywhere(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", ".git", "config"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, ListFiles{}, fmt.Sprintf(`{"path":%q}`, dir))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(out, ".git") || !strings.Contains(out, "keep.txt") {
		t.Fatalf("listing = %q, want .git skipped and keep.txt present", out)
	}
}

// TestListFilesSymlinkLoopTerminates pins the #256 termination guarantee: a
// symlink cycle in the fixture must not cause infinite recursion.
func TestListFilesSymlinkLoopTerminates(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// sub/loop -> dir, and dir/loop2 -> sub: a cycle between two levels.
	if err := os.Symlink(dir, filepath.Join(sub, "loop")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(sub, filepath.Join(dir, "loop2")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	out, err := run(t, ListFiles{}, fmt.Sprintf(`{"path":%q}`, dir))
	if err != nil {
		t.Fatalf("list with symlink loop: %v", err)
	}
	if !strings.Contains(out, "loop2\tsymlink\t") || !strings.Contains(out, "loop\tsymlink\t") {
		t.Fatalf("listing = %q, want symlink entries listed without descending", out)
	}
	if !strings.Contains(out, "sub/f.txt\tfile\t") {
		t.Fatalf("listing = %q, want sub/f.txt present", out)
	}
}

// TestListFilesCancellation pins the Search-parity cancellation contract: a
// cancelled context aborts the walk promptly with context.Canceled.
func TestListFilesCancellation(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 200; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (ListFiles{}).Run(ctx, json.RawMessage(fmt.Sprintf(`{"path":%q}`, dir))); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled list_files error = %v", err)
	}
}

// TestEditFileBatch covers the #256 batched form: edits apply in order
// against the evolving content, and a failure on ANY edit — including the
// last — leaves the file byte-identical to its pre-call state.
func TestEditFileBatch(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		edits   []map[string]any
		extra   map[string]any // additional top-level fields (e.g. old_string)
		want    string         // final content on success
		wantErr string
	}{
		{
			name:  "edits apply in order against evolving content",
			body:  "one two three",
			edits: []map[string]any{{"old_string": "one", "new_string": "1"}, {"old_string": "two", "new_string": "2"}},
			want:  "1 2 three",
		},
		{
			name:  "later edit matches earlier edit's result",
			body:  "ab",
			edits: []map[string]any{{"old_string": "ab", "new_string": "Xbc"}, {"old_string": "Xbc", "new_string": "Y"}},
			want:  "Y",
		},
		{
			name:    "overlapping edits fail deterministically",
			body:    "abc",
			edits:   []map[string]any{{"old_string": "ab", "new_string": "X"}, {"old_string": "bc", "new_string": "Y"}},
			wantErr: "batch aborted, file unchanged",
		},
		{
			name:    "failure on last edit leaves file byte-identical",
			body:    "one two",
			edits:   []map[string]any{{"old_string": "one", "new_string": "1"}, {"old_string": "missing", "new_string": "x"}},
			wantErr: "edit 2 of 2",
		},
		{
			name:    "ambiguity without replace_all aborts batch",
			body:    "a a",
			edits:   []map[string]any{{"old_string": "a", "new_string": "b"}},
			wantErr: "appears 2 times",
		},
		{
			name:  "replace_all inside a batch",
			body:  "a a",
			edits: []map[string]any{{"old_string": "a", "new_string": "b", "replace_all": true}},
			want:  "b b",
		},
		{
			name:    "batch combined with old_string rejected",
			body:    "x",
			edits:   []map[string]any{{"old_string": "x", "new_string": "y"}},
			extra:   map[string]any{"old_string": "x"},
			wantErr: "cannot be combined",
		},
		{
			name:    "empty batch rejected",
			body:    "x",
			edits:   []map[string]any{},
			wantErr: "at least one edit",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f.txt")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			input := map[string]any{"path": path, "edits": tc.edits}
			for k, v := range tc.extra {
				input[k] = v
			}
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			out, err := run(t, EditFile{}, string(raw))
			got, _ := os.ReadFile(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				if string(got) != tc.body {
					t.Fatalf("failed batch changed the file: %q, want byte-identical %q", got, tc.body)
				}
				return
			}
			if err != nil {
				t.Fatalf("batch: %v", err)
			}
			if !strings.Contains(out, "applied 2 edit(s)") && !strings.Contains(out, "applied 1 edit(s)") {
				t.Fatalf("result message = %q", out)
			}
			if string(got) != tc.want {
				t.Fatalf("content = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEditFileBatchBounds pins the batch guards: per-edit content size, the
// batch length cap, and permission preservation via info.Mode().Perm().
func TestEditFileBatchBounds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}

	// Per-edit new_string size cap.
	tooLarge := strings.Repeat("x", fileContentMaxBytes+1)
	raw, err := json.Marshal(map[string]any{"path": path, "edits": []map[string]any{{"old_string": "old", "new_string": tooLarge}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, EditFile{}, string(raw)); err == nil || !strings.Contains(err.Error(), "new_string too large") {
		t.Fatalf("oversized batch edit error = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Fatalf("oversized batch edit changed file to %q", got)
	}

	// Batch length cap.
	edits := make([]map[string]any, 0, editBatchMaxEdits+1)
	for i := 0; i < editBatchMaxEdits+1; i++ {
		edits = append(edits, map[string]any{"old_string": "old", "new_string": "x"})
	}
	raw, err = json.Marshal(map[string]any{"path": path, "edits": edits})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, EditFile{}, string(raw)); err == nil || !strings.Contains(err.Error(), "exceeds maximum 100") {
		t.Fatalf("oversized batch error = %v", err)
	}

	// Evolving-content cap: successive replace_all edits expand the result
	// past the per-edit new_string limit; the batch must fail and leave the
	// file untouched (#256 review).
	growthPath := filepath.Join(dir, "growth.txt")
	if err := os.WriteFile(growthPath, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	growth := make([]map[string]any, 0, 12)
	for range 12 {
		growth = append(growth, map[string]any{"old_string": "x", "new_string": "xxxx", "replace_all": true})
	}
	raw, err = json.Marshal(map[string]any{"path": growthPath, "edits": growth})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, EditFile{}, string(raw)); err == nil || !strings.Contains(err.Error(), "result too large") {
		t.Fatalf("expanding batch error = %v, want result-too-large", err)
	}
	if got, _ := os.ReadFile(growthPath); string(got) != "x" {
		t.Fatalf("expanding batch changed file to %q", got)
	}

	// Oversized source file: the initial read is bounded like write_file.
	bigPath := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(bigPath, []byte(strings.Repeat("y", fileContentMaxBytes+1)), 0o640); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(map[string]any{"path": bigPath, "edits": []map[string]any{{"old_string": "y", "new_string": "z"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, EditFile{}, string(raw)); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("oversized source error = %v, want maximum-bytes refusal", err)
	}

	// Permissions preserved on success.
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(map[string]any{"path": path, "edits": []map[string]any{{"old_string": "old", "new_string": "new"}, {"old_string": "new", "new_string": "NEW"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, EditFile{}, string(raw)); err != nil {
		t.Fatalf("batch: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("batch edit changed perm from %o to %o", before.Mode().Perm(), after.Mode().Perm())
	}
	if os.SameFile(before, after) {
		t.Error("file was rewritten in place, want atomic rename")
	}
	if got, _ := os.ReadFile(path); string(got) != "NEW" {
		t.Fatalf("content = %q, want NEW", got)
	}
}

// TestFileToolsConcurrent pins the tool.Tool contract for the #256 tools:
// read_file (ranged), list_files, and edit_file (batch) must be safe for
// concurrent invocation. Run under -race.
func TestFileToolsConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readInput := fmt.Sprintf(`{"path":%q,"offset":2,"limit":1}`, path)
	listInput := fmt.Sprintf(`{"path":%q}`, dir)

	var wg sync.WaitGroup
	errs := make(chan error, 96)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if out, err := run(t, ReadFile{}, readInput); err != nil || !strings.Contains(out, "2: beta") {
				errs <- fmt.Errorf("ranged read: %q %v", out, err)
				return
			}
			if out, err := run(t, ListFiles{}, listInput); err != nil || !strings.Contains(out, "shared.txt\tfile\t") {
				errs <- fmt.Errorf("list_files: %q %v", out, err)
				return
			}
			// Concurrent edits use distinct files: same-file concurrent writes
			// are user error, but shared state between calls must still be
			// race-free (distinct files exercise that without lost updates).
			editPath := filepath.Join(dir, fmt.Sprintf("edit-%03d.txt", i))
			if err := os.WriteFile(editPath, []byte("a b"), 0o600); err != nil {
				errs <- err
				return
			}
			raw, _ := json.Marshal(map[string]any{"path": editPath, "edits": []map[string]any{{"old_string": "a", "new_string": "1"}, {"old_string": "b", "new_string": "2"}}})
			if out, err := run(t, EditFile{}, string(raw)); err != nil || !strings.Contains(out, "applied 2 edit(s)") {
				errs <- fmt.Errorf("batch edit: %q %v", out, err)
				return
			}
			if got, _ := os.ReadFile(editPath); string(got) != "1 2" {
				errs <- fmt.Errorf("batch edit content = %q", got)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
