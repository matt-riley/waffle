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
		"actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0 # v7",
		"actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6",
		"build-linux-artifact:",
		"if: github.event_name == 'push' && github.ref == 'refs/heads/main'",
	} {
		if !strings.Contains(workflow, want) {
			t.Fatalf("workflow missing %q:\n%s", want, workflow)
		}
	}
}

func TestCIWorkflowRequestsInfraDeployWithImmutableArtifactOnly(t *testing.T) {
	workflow := readRepoFile(t, ".github/workflows/ci.yml")
	jobStart := strings.Index(workflow, "  request-infra-deploy:\n")
	if jobStart < 0 {
		t.Fatalf("workflow lacks request-infra-deploy job:\n%s", workflow)
	}
	job := workflow[jobStart:]

	for _, want := range []string{
		"needs: build-linux-artifact",
		"if: github.event_name == 'push' && github.ref == 'refs/heads/main' && vars.APP_ID != ''",
		"uses: matt-riley/matt-riley-ci/.github/workflows/request-infra-deploy.yml@v2",
		"artifact-run-id: ${{ github.run_id }}",
		"artifact-name: waffle-linux-amd64",
		"artifact-digest: ${{ needs.build-linux-artifact.outputs.artifact_digest }}",
	} {
		if !strings.Contains(job, want) {
			t.Fatalf("deploy request job missing %q:\n%s", want, job)
		}
	}

	for _, forbidden := range []string{
		"api-key",
		"provider-key",
		"router",
		"postgres",
		"database-password",
		"anthropic",
		"openai",
	} {
		if strings.Contains(strings.ToLower(job), forbidden) {
			t.Fatalf("deploy request job forwards provider or infrastructure detail %q:\n%s", forbidden, job)
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
