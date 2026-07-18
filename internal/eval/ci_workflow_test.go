package eval

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPullRequestWorkflowRunsDeterministicEval(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller path unavailable")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "pull_request:") || !strings.Contains(body, "go run ./cmd/waffle eval") {
		t.Fatalf("PR workflow lacks deterministic eval job:\n%s", body)
	}
}

func TestLinuxArtifactWorkflowPinsReviewedActionsAndRunsReproCheck(t *testing.T) {
	workflow := readRepoFile(t, ".github/workflows/ci.yml")

	for _, want := range []string{
		"linux-artifact-repro:",
		"bash scripts/check-linux-artifact-repro.sh",
		"actions/checkout@08c6903cd8c0fde910a37f88322edcfb5dd907a8 # v5.0.0",
		"actions/setup-go@d35c59abb061a4a6fb18e82ac0862c26744d6ab5 # v5.5.0",
		"build-linux-artifact:",
		"if: github.event_name == 'push' && github.ref == 'refs/heads/main'",
	} {
		if !strings.Contains(workflow, want) {
			t.Fatalf("workflow missing %q:\n%s", want, workflow)
		}
	}
}

func TestCheckLinuxArtifactReproScriptIsExecutableAndPortable(t *testing.T) {
	scriptPath := readRepoPath(t, "scripts/check-linux-artifact-repro.sh")
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("expected executable mode 755, got %o", info.Mode().Perm())
	}

	body := readRepoFile(t, "scripts/check-linux-artifact-repro.sh")
	for _, want := range []string{
		"RUNNER_TEMP",
		"TMPDIR",
		"/tmp",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("script missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/private/tmp") {
		t.Fatalf("script still hardcodes /private/tmp:\n%s", body)
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	path := readRepoPath(t, rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func readRepoPath(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller path unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", rel)
}
