package repopolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/tool"
)

func TestParseAndPromptMarker(t *testing.T) {
	raw := `---
tools.deny: bash, remember
hooks.after_create: go mod download
hooks.timeout: 2m
idle_timeout: 15m
egress: none
---

Follow the repo conventions.
`
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Tools.Deny) != 2 || p.Tools.Deny[0] != "bash" {
		t.Fatalf("deny = %#v", p.Tools.Deny)
	}
	if p.Hooks.AfterCreate != "go mod download" || p.Hooks.Timeout != "2m" {
		t.Fatalf("hooks = %#v", p.Hooks)
	}
	block := p.PromptBlock()
	if !strings.Contains(block, "untrusted repo-provenance") {
		t.Fatalf("missing provenance marker: %q", block)
	}
	if !strings.Contains(block, "Follow the repo conventions.") {
		t.Fatalf("missing body: %q", block)
	}
}

func TestUnparsableSurfacesError(t *testing.T) {
	if _, err := Parse("---\nbad line without colon\n---\nbody\n"); err == nil {
		t.Fatal("expected parse error")
	}
	if _, err := Parse("---\ntools.deny: bash\n"); err == nil {
		t.Fatal("expected unclosed front matter error")
	}
}

func TestLoadMissingIsNil(t *testing.T) {
	p, err := Load(t.TempDir())
	if err != nil || p != nil {
		t.Fatalf("got %#v %v", p, err)
	}
}

func TestLoadPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WAFFLE.md")
	if err := os.WriteFile(path, []byte("---\ntools.deny: bash\n---\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Path != path || p.Body != "hello" {
		t.Fatalf("policy = %#v", p)
	}
}

func TestLoadUnparsableErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("---\nnope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error")
	}
}

func TestTightenToolsDenyAndCannotAdd(t *testing.T) {
	host := tool.Policy{Allow: []string{"bash", "read", "write"}, Deny: []string{"remember"}}
	// Repo tries to deny bash (ok) and re-allow remember via allow-list (must not grant remember).
	got := TightenTools(host, ToolFilter{Deny: []string{"bash"}, Allow: []string{"bash", "read", "remember"}})
	if got.Permits("bash") {
		t.Error("repo deny of bash should remove it")
	}
	if got.Permits("remember") {
		t.Error("repo must not re-enable host-denied remember")
	}
	if !got.Permits("read") {
		t.Error("read should remain after intersection")
	}
	if got.Permits("write") {
		t.Error("write not in repo allow-list; should be gone")
	}
}

func TestTightenEgressAndIdle(t *testing.T) {
	if g := TightenEgress("full", "none"); g != "none" {
		t.Fatalf("egress = %q", g)
	}
	if g := TightenEgress("none", "full"); g != "none" {
		t.Fatalf("repo cannot widen egress: %q", g)
	}
	host := 30 * time.Minute
	if g := TightenIdle(host, 10*time.Minute); g != 10*time.Minute {
		t.Fatalf("idle = %v", g)
	}
	if g := TightenIdle(host, time.Hour); g != host {
		t.Fatalf("repo cannot lengthen idle: %v", g)
	}
}

func TestMtimeReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WAFFLE.md")
	if err := os.WriteFile(path, []byte("---\ntools.deny: bash\n---\nv1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p1, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("---\ntools.deny: bash,fetch\n---\nv2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.ModTime.After(p1.ModTime) && p2.Body == p1.Body {
		t.Fatal("expected mtime/body change between loads")
	}
	if p2.Body != "v2" || len(p2.Tools.Deny) != 2 {
		t.Fatalf("p2 = %#v", p2)
	}
}

// TestCacheGetReloadsOnMtime covers serve-without-restart policy refresh (#53).
func TestCacheGetReloadsOnMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WAFFLE.md")
	if err := os.WriteFile(path, []byte("---\ntools.deny: bash\n---\nsession-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := NewCache(dir)
	p1, err := cache.Get()
	if err != nil || p1 == nil || p1.Body != "session-one" {
		t.Fatalf("p1 = %#v err=%v", p1, err)
	}
	// Same mtime → cached pointer.
	p1b, err := cache.Get()
	if err != nil || p1b != p1 {
		t.Fatalf("expected cache hit, got %#v err=%v", p1b, err)
	}
	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(path, []byte("---\ntools.deny: bash,remember\n---\nsession-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, err := cache.Get()
	if err != nil || p2 == nil {
		t.Fatalf("p2 err=%v", err)
	}
	if p2.Body != "session-two" || len(p2.Tools.Deny) != 2 {
		t.Fatalf("p2 not reloaded: %#v", p2)
	}
	if p2 == p1 {
		t.Fatal("expected new policy pointer after mtime advance")
	}
}

func TestTightenToolsCannotWidenHostDeny(t *testing.T) {
	// Both directions: repo deny tightens; repo cannot re-enable host deny.
	host := tool.Policy{Allow: []string{"bash", "read", "write", "fetch"}, Deny: []string{"remember"}}
	got := TightenTools(host, ToolFilter{Deny: []string{"write"}, Allow: []string{"bash", "read", "write", "fetch", "remember"}})
	if got.Permits("write") {
		t.Error("repo deny of write should stick")
	}
	if got.Permits("remember") {
		t.Error("repo allow must not re-enable host-denied remember")
	}
	if !got.Permits("bash") || !got.Permits("read") {
		t.Error("untouched allows should remain")
	}
}

func TestAbsentPolicyNoChange(t *testing.T) {
	host := tool.Policy{Allow: []string{"bash"}, Deny: []string{"remember"}}
	got := TightenTools(host, ToolFilter{})
	if !got.Permits("bash") || got.Permits("remember") {
		t.Fatalf("absent filter changed policy: %#v", got)
	}
	if g := TightenEgress("full", ""); g != "full" {
		t.Fatalf("egress = %q", g)
	}
	hostIdle := 30 * time.Minute
	if g := TightenIdle(hostIdle, 0); g != hostIdle {
		t.Fatalf("idle = %v", g)
	}
	if p, err := Load(t.TempDir()); err != nil || p != nil {
		t.Fatalf("missing file: %#v %v", p, err)
	}
}

func TestFilterCodeIntelCaps(t *testing.T) {
	approved := func(id string) bool {
		return id == "code_find_symbol" || id == "code_blast_radius"
	}
	got := FilterCodeIntelCaps([]string{"code_find_symbol", "/bin/evil", "code_blast_radius", "nope"}, approved)
	if len(got) != 2 || got[0] != "code_find_symbol" {
		t.Fatalf("%v", got)
	}
}
