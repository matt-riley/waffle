package eval

import (
	"os"
	"os/exec"
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

	assertWorkflowActionPins(t, workflow, "actions/checkout", map[string]string{
		"9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0": "v7",
		"3d3c42e5aac5ba805825da76410c181273ba90b1": "v7",
	})
	// #579 moved every job from actions/setup-go to the mise-pinned toolchain.
	assertWorkflowActionPins(t, workflow, "jdx/mise-action", map[string]string{
		"3c2e0cf82a5b2e5249f0d3635a4d83d0ae861518": "v4",
	})

	for _, want := range []string{
		"linux-artifact-repro:",
		"bash scripts/check-linux-artifact-repro.sh",
		"build-linux-artifact:",
		"if: github.event_name == 'push' && github.ref == 'refs/heads/main'",
		// #335: the artifact build runs in parallel with the repro check (the
		// repro gate moves to the deploy dispatch, asserted by
		// TestCIWorkflowRequestsInfraDeployWithImmutableArtifactOnly). ci-aux
		// carries the build/vet/dashboard/eval work that used to run serially
		// inside the old universal-ci job, so it gates the artifact build too.
		"needs: [deterministic-eval, dashboard-browser, ci, ci-aux, lint, security]",
	} {
		if !strings.Contains(workflow, want) {
			t.Fatalf("workflow missing %q:\n%s", want, workflow)
		}
	}
}

func assertWorkflowActionPins(t *testing.T, workflow, action string, allowed map[string]string) {
	t.Helper()
	prefix := "- uses: " + action + "@"
	found := false
	for _, line := range strings.Split(workflow, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		found = true
		parts := strings.SplitN(strings.TrimPrefix(line, prefix), " # ", 2)
		if len(parts) != 2 {
			t.Fatalf("workflow action %q is not annotated with a version: %q", action, line)
		}
		pin := strings.TrimSpace(parts[0])
		version := strings.TrimSpace(parts[1])
		if allowedVersion, ok := allowed[pin]; !ok || version != allowedVersion {
			t.Fatalf("workflow action %q has unreviewed pin %q %q", action, pin, version)
		}
	}
	if !found {
		t.Fatalf("workflow does not use %q", action)
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
		// #335: the dispatch waits for both the artifact build and the repro
		// check, so no deploy request leaves a run whose artifact is not
		// proven reproducible.
		"needs: [build-linux-artifact, linux-artifact-repro, dispatch-config]",
		"if: github.event_name == 'push' && github.ref == 'refs/heads/main' && needs.dispatch-config.outputs.configured == 'true'",
		"uses: matt-riley/matt-riley-ci/.github/workflows/request-app-deploy.yml@2aedbf6107ff792f9dd41b9c9074dc4801b5888c",
		"app: waffle",
		"artifact-run-id: ${{ github.run_id }}",
		"dispatch-app-id: ${{ vars.INFRA_DISPATCH_APP_ID }}",
		"INFRA_DISPATCH_PRIVATE_KEY: ${{ secrets.INFRA_DISPATCH_PRIVATE_KEY }}",
		"artifact-name: waffle-linux-amd64",
		"artifact-digest: ${{ needs.build-linux-artifact.outputs.artifact_digest }}",
	} {
		if !strings.Contains(job, want) {
			t.Fatalf("deploy request job missing %q:\n%s", want, job)
		}
	}

	for _, forbidden := range []string{
		"vars.APP_ID",
		"secrets.PRIVATE_KEY",
		"api-key",
		"provider-key",
		"router",
		"postgres",
		"database-password",
		"anthropic",
		"openai",
	} {
		if strings.Contains(strings.ToLower(job), strings.ToLower(forbidden)) {
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

func TestCIWorkflowOptionalDispatchRequiresCredentialPair(t *testing.T) {
	workflow := readRepoFile(t, ".github/workflows/ci.yml")
	_, after, found := strings.Cut(workflow, "  dispatch-config:\n")
	if !found {
		t.Fatal("missing dispatch configuration check")
	}
	job, _, found := strings.Cut(after, "  request-infra-deploy:\n")
	if !found {
		t.Fatal("missing downstream deployment request")
	}
	for _, required := range []string{
		"if: github.event_name == 'push' && github.ref == 'refs/heads/main' && vars.INFRA_DISPATCH_APP_ID != ''",
		"configured: ${{ steps.check.outputs.configured }}",
		"DISPATCH_APP_ID: ${{ vars.INFRA_DISPATCH_APP_ID }}",
		"DISPATCH_PRIVATE_KEY: ${{ secrets.INFRA_DISPATCH_PRIVATE_KEY }}",
	} {
		if !strings.Contains(job, required) {
			t.Fatalf("configuration job missing %q", required)
		}
	}
	_, script, found := strings.Cut(job, "        run: |\n")
	if !found {
		t.Fatal("missing executable configuration check")
	}
	for _, tc := range []struct{ name, id, key, want string }{
		{"missing both", "", "", "false"},
		{"missing key", "123", "", "false"},
		{"missing id", "", "test-placeholder", "false"},
		{"present pair", "123", "test-placeholder", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "outputs")
			command := exec.Command("bash", "-c", script)
			command.Env = append(os.Environ(), "DISPATCH_APP_ID="+tc.id, "DISPATCH_PRIVATE_KEY="+tc.key, "GITHUB_OUTPUT="+output)
			logs, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("configuration check failed: %v: %s", err, logs)
			}
			if len(logs) != 0 {
				t.Fatalf("configuration check must not log credential data: %s", logs)
			}
			actual, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != "configured="+tc.want+"\n" {
				t.Fatalf("unexpected output %q", actual)
			}
		})
	}
}
