package providerconfig

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/instance"
	"github.com/matt-riley/waffle/internal/secret"
)

const providerTestKey = "test-provider-key-must-never-leak"

func TestManagerAddProbeFailureLeavesFilesUnchanged(t *testing.T) {
	m := newTestManager(t)
	beforeConfig := readMaybe(t, m.ConfigPath)
	beforeSecrets := readMaybe(t, m.SecretsPath)
	m.Probe = func(context.Context, config.ResolvedModel, string) error {
		return errors.New("credentials rejected")
	}

	err := m.Add(context.Background(), validAddRequest())
	if err == nil || !strings.Contains(err.Error(), "probe") {
		t.Fatalf("Add error = %v, want probe failure", err)
	}
	assertBytesEqual(t, m.ConfigPath, beforeConfig)
	assertBytesEqual(t, m.SecretsPath, beforeSecrets)
	if strings.Contains(err.Error(), providerTestKey) {
		t.Fatalf("error leaked API key: %v", err)
	}
}

func TestManagerProspectiveTestUsesEnteredProviderWithoutMutatingState(t *testing.T) {
	m := newTestManager(t)
	beforeConfig := readMaybe(t, m.ConfigPath)
	beforeSecrets := readMaybe(t, m.SecretsPath)
	const apiKey = "prospective-provider-key"
	var gotTarget config.ResolvedModel
	var gotKey string
	m.Probe = func(_ context.Context, target config.ResolvedModel, key string) error {
		gotTarget = target
		gotKey = key
		return nil
	}

	req := ProspectiveProbeRequest{
		ConnectionName: "new-provider",
		Connection: config.ProviderConnection{
			Type:      "openai",
			BaseURL:   "https://gateway.example/v1",
			MaxTokens: 321,
		},
		Model:  "vendor/model",
		APIKey: apiKey,
	}
	if err := m.TestProspective(context.Background(), req); err != nil {
		t.Fatalf("TestProspective: %v", err)
	}
	if gotTarget.ConnectionName != req.ConnectionName || gotTarget.Connection != req.Connection ||
		gotTarget.UpstreamModel != req.Model || gotTarget.MaxTokens != req.Connection.MaxTokens {
		t.Fatalf("probe target = %#v, want entered provider/model = %#v", gotTarget, req)
	}
	if gotKey != apiKey {
		t.Fatalf("probe key = %q, want entered key", gotKey)
	}
	assertBytesEqual(t, m.ConfigPath, beforeConfig)
	assertBytesEqual(t, m.SecretsPath, beforeSecrets)
}

func TestManagerAddRejectsActiveKeyInEveryDurableRequestString(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AddRequest)
	}{
		{name: "connection name", mutate: func(req *AddRequest) { req.ConnectionName = providerTestKey }},
		{name: "connection type", mutate: func(req *AddRequest) { req.Connection.Type = "openai-" + providerTestKey }},
		{name: "connection API key field", mutate: func(req *AddRequest) { req.Connection.APIKey = "secret://" + providerTestKey }},
		{name: "base URL", mutate: func(req *AddRequest) { req.Connection.BaseURL = "https://example.invalid/" + providerTestKey }},
		{name: "model alias", mutate: func(req *AddRequest) {
			req.Models = map[string]config.ModelTarget{providerTestKey: {Model: "gpt-test"}}
		}},
		{name: "model provider", mutate: func(req *AddRequest) {
			req.Models["gpt"] = config.ModelTarget{Provider: "prefix-" + providerTestKey, Model: "gpt-test"}
		}},
		{name: "upstream model", mutate: func(req *AddRequest) {
			req.Models["gpt"] = config.ModelTarget{Model: "vendor/" + providerTestKey}
		}},
		{name: "default alias", mutate: func(req *AddRequest) { req.DefaultModel = providerTestKey }},
		{name: "utility alias", mutate: func(req *AddRequest) { req.UtilityModel = providerTestKey }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager(t)
			before := captureManagerState(t, m)
			req := validAddRequest()
			tt.mutate(&req)

			err := m.Add(t.Context(), req)
			if err == nil || !strings.Contains(err.Error(), "durable provider configuration contains the active API key") {
				t.Fatalf("Add() error = %v, want active-key persistence rejection", err)
			}
			assertErrorTreeRedacted(t, err)
			assertManagerState(t, m, before)
			assertNoProviderStageFiles(t, filepath.Dir(m.ConfigPath))
		})
	}
}

func TestManagerAddCommitsSecretBeforeConfigAndClearsBackupsAfterReady(t *testing.T) {
	m := newTestManager(t)
	var events []string
	restarted := false
	m.Restart = func(context.Context) error {
		restarted = true
		return nil
	}
	m.ServiceActive = func(context.Context) (bool, error) { return restarted, nil }
	m.Health = func(context.Context) error {
		if !restarted {
			t.Fatal("health called before restart")
		}
		return nil
	}
	m.AfterCommit = func(resource string) error {
		events = append(events, resource)
		switch resource {
		case "secret":
			cfgBytes := readMaybe(t, m.ConfigPath)
			if bytes.Contains(cfgBytes, []byte("[providers.openai]")) {
				t.Fatal("config reference committed before secret")
			}
			assertStoredSecret(t, m, "provider/openai/api-key", providerTestKey)
		case "config":
			if _, err := os.Stat(m.ConfigPath + ".bak"); err != nil {
				t.Fatalf("config backup absent during activation: %v", err)
			}
			if _, err := os.Stat(m.SecretsPath + ".bak"); err != nil {
				t.Fatalf("secret backup absent during activation: %v", err)
			}
		}
		return nil
	}

	if err := m.Add(context.Background(), validAddRequest()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if strings.Join(events, ",") != "secret,config" {
		t.Fatalf("commit events = %v, want secret, config", events)
	}
	for _, path := range []string{m.ConfigPath + ".bak", m.SecretsPath + ".bak"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("backup %s remains after success: %v", path, err)
		}
	}
	cfgBytes := readMaybe(t, m.ConfigPath)
	for _, want := range []string{"# keep this comment", "level = \"debug\"", "[providers.openai]", "secret://provider/openai/api-key"} {
		if !bytes.Contains(cfgBytes, []byte(want)) {
			t.Errorf("config lost %q:\n%s", want, cfgBytes)
		}
	}
	if bytes.Contains(cfgBytes, []byte(providerTestKey)) {
		t.Fatal("config contains raw API key")
	}
	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" || status.DefaultModel != "gpt" {
		t.Fatalf("Status = %#v, want ready/gpt", status)
	}
}

func TestManagerAddRollsBackFilesOnLifecycleFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart func(context.Context) error
		health  func(context.Context) error
	}{
		{name: "restart", restart: func(context.Context) error { return errors.New("restart failed") }},
		{name: "health", restart: func(context.Context) error { return nil }, health: func(context.Context) error { return errors.New("unhealthy") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			beforeConfig := readMaybe(t, m.ConfigPath)
			beforeSecrets := readMaybe(t, m.SecretsPath)
			m.Restart = tc.restart
			m.Health = tc.health
			err := m.Add(context.Background(), validAddRequest())
			if err == nil {
				t.Fatal("Add succeeded, want lifecycle failure")
			}
			assertBytesEqual(t, m.ConfigPath, beforeConfig)
			assertBytesEqual(t, m.SecretsPath, beforeSecrets)
			if strings.Contains(err.Error(), providerTestKey) {
				t.Fatalf("error leaked key: %v", err)
			}
		})
	}
}

func TestManagerLockRejectsConcurrentMutation(t *testing.T) {
	m := newTestManager(t)
	lease, err := instance.Default(m.LockPath).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	err = m.Add(context.Background(), validAddRequest())
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("Add error = %v, want ErrLocked", err)
	}
}

func TestManagerRemoveRejectsReferencedConnectionPrecisely(t *testing.T) {
	m := newTestManager(t)
	if err := m.Add(context.Background(), validAddRequest()); err != nil {
		t.Fatal(err)
	}
	err := m.Remove(context.Background(), "openai")
	if !errors.Is(err, ErrReferenced) || !strings.Contains(err.Error(), "gpt") {
		t.Fatalf("Remove error = %v, want referenced alias gpt", err)
	}
	if strings.Contains(err.Error(), providerTestKey) {
		t.Fatalf("error leaked key: %v", err)
	}
}

func TestManagerPreviewRemovalBindsCurrentProviderState(t *testing.T) {
	m := newTestManager(t)
	req := validAddRequest()
	req.Models["small"] = config.ModelTarget{Model: "gpt-small"}
	if err := m.Add(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	model, err := m.PreviewModelRemoval(context.Background(), "gpt")
	if err != nil {
		t.Fatal(err)
	}
	if model.Alias != "gpt" || model.Provider != "openai" || !model.Default || !model.Utility || model.Revision == "" {
		t.Fatalf("model preview = %#v", model)
	}
	if len(model.Profiles) != 0 {
		t.Fatalf("model profiles = %v, want empty", model.Profiles)
	}
	connection, err := m.PreviewProviderRemoval(context.Background(), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if connection.Name != "openai" || connection.Revision == "" || !slices.Equal(connection.ModelAliases, []string{"gpt", "small"}) {
		t.Fatalf("connection preview = %#v", connection)
	}
}

func TestManagerModeAwareRemovalReturnsDeferredMutation(t *testing.T) {
	m := newTestManager(t)
	enrollConnectionWithoutRoles(t, m, false)
	result, err := m.RemoveModelWithMode(context.Background(), "gpt", "", CommitForRestart)
	if err != nil {
		t.Fatal(err)
	}
	if !result.RestartRequired || result.TransactionID == "" {
		t.Fatalf("model removal result = %#v", result)
	}
	provider := newTestManager(t)
	enrollConnectionWithoutRoles(t, provider, false)
	if err := provider.RemoveModel(context.Background(), "gpt", ""); err != nil {
		t.Fatal(err)
	}
	providerResult, err := provider.RemoveWithMode(context.Background(), "openai", CommitForRestart)
	if err != nil {
		t.Fatal(err)
	}
	if !providerResult.RestartRequired || providerResult.TransactionID == "" {
		t.Fatalf("provider removal result = %#v", providerResult)
	}
	if _, err := os.Stat(provider.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if cfg, err := config.Load(provider.ConfigPath); err != nil {
		t.Fatal(err)
	} else if len(cfg.Models) != 0 || len(cfg.Providers) != 0 {
		t.Fatalf("removed config = %#v", cfg)
	}
	store := secret.OpenFile(provider.SecretsPath, provider.Identity)
	for _, name := range []string{"provider/openai/api-key", "provider/openai/catalog-scope"} {
		if _, err := store.Get(name); !errors.Is(err, secret.ErrNotFound) {
			t.Fatalf("secret %q error = %v, want ErrNotFound", name, err)
		}
	}
}

func TestManagerInstalledWithoutDefaultAndTestConnection(t *testing.T) {
	m := newTestManager(t)
	req := validAddRequest()
	req.DefaultModel = ""
	if err := m.Add(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "installed" || status.DefaultModel != "" {
		t.Fatalf("Status = %#v, want installed", status)
	}
	var gotKey string
	m.Probe = func(_ context.Context, target config.ResolvedModel, key string) error {
		gotKey = key
		if target.Alias != "gpt" || target.ConnectionName != "openai" {
			t.Fatalf("target = %#v", target)
		}
		return nil
	}
	if err := m.Test(context.Background(), "openai"); err != nil {
		t.Fatal(err)
	}
	if gotKey != providerTestKey {
		t.Fatal("Test did not resolve encrypted API key")
	}
}

func TestManagerNeverReturnsSecretInListingJSON(t *testing.T) {
	m := newTestManager(t)
	if err := m.Add(context.Background(), validAddRequest()); err != nil {
		t.Fatal(err)
	}
	listing, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(listing, []byte(providerTestKey)) {
		t.Fatalf("listing leaked API key: %s", listing)
	}
}

func TestManagerFirstEnrollmentCreatesEncryptedStoreWithPrivateModes(t *testing.T) {
	m := newTestManager(t)
	if err := os.Remove(m.SecretsPath); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(context.Background(), validAddRequest()); err != nil {
		t.Fatal(err)
	}
	assertStoredSecret(t, m, "provider/openai/api-key", providerTestKey)
	for _, path := range []string{m.ConfigPath, m.SecretsPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", path, got)
		}
	}
}

func TestManagerRejectsConnectionAndAliasCollisionsWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*AddRequest)
		want   string
	}{
		{name: "connection", mutate: func(*AddRequest) {}, want: "connection \"openai\" already exists"},
		{name: "alias", mutate: func(req *AddRequest) {
			req.ConnectionName = "second"
		}, want: "model alias \"gpt\" already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			if err := m.Add(context.Background(), validAddRequest()); err != nil {
				t.Fatal(err)
			}
			beforeConfig := readMaybe(t, m.ConfigPath)
			beforeSecrets := readMaybe(t, m.SecretsPath)
			req := validAddRequest()
			tc.mutate(&req)
			err := m.Add(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Add error = %v, want %q", err, tc.want)
			}
			assertBytesEqual(t, m.ConfigPath, beforeConfig)
			assertBytesEqual(t, m.SecretsPath, beforeSecrets)
		})
	}
}

func TestManagerRollsBackWhenCommitBoundaryHookFails(t *testing.T) {
	for _, boundary := range []string{"secret", "config"} {
		t.Run(boundary, func(t *testing.T) {
			m := newTestManager(t)
			beforeConfig := readMaybe(t, m.ConfigPath)
			beforeSecrets := readMaybe(t, m.SecretsPath)
			m.AfterCommit = func(resource string) error {
				if resource == boundary {
					return errors.New("injected commit failure")
				}
				return nil
			}
			if err := m.Add(context.Background(), validAddRequest()); err == nil {
				t.Fatal("Add succeeded, want injected failure")
			}
			assertBytesEqual(t, m.ConfigPath, beforeConfig)
			assertBytesEqual(t, m.SecretsPath, beforeSecrets)
		})
	}
}

func TestManagerFirstWriteMigratesLegacyProviderWithoutCredentialInConfig(t *testing.T) {
	m := newTestManager(t)
	legacyKey := "legacy-key-must-move"
	legacy := "# legacy operator comment\n[provider]\nname = \"anthropic\"\nmodel = \"claude-old\"\napi_key = \"" + legacyKey + "\"\n\n[[mcp]]\nname = \"keep-me\"\ncommand = \"true\"\n\n[log]\nlevel = \"debug\"\n"
	if err := os.WriteFile(m.ConfigPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(context.Background(), validAddRequest()); err != nil {
		t.Fatal(err)
	}
	got := readMaybe(t, m.ConfigPath)
	for _, want := range []string{"# legacy operator comment", "[providers.default]", "secret://provider/default/api-key", "claude-old", "[providers.openai]", "[[mcp]]", "keep-me"} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("migrated config lost %q:\n%s", want, got)
		}
	}
	if bytes.Contains(got, []byte(legacyKey)) || bytes.Contains(got, []byte("[provider]")) {
		t.Fatalf("legacy config retained credential/table:\n%s", got)
	}
	assertStoredSecret(t, m, "provider/default/api-key", legacyKey)
}

func TestManagerRejectsQuotedAndInlineManagedTOMLBeforeSecretMutation(t *testing.T) {
	for _, body := range []string{
		"[providers.\"openai\"]\ntype = \"openai\"\n",
		"providers = { openai = { type = \"openai\" } }\n",
		"['provider']\nname = \"anthropic\"\nmodel = \"old\"\napi_key = \"legacy\"\n",
	} {
		t.Run(strings.ReplaceAll(body[:min(len(body), 12)], "\n", "_"), func(t *testing.T) {
			m := newTestManager(t)
			if err := os.WriteFile(m.ConfigPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			beforeSecrets := readMaybe(t, m.SecretsPath)
			err := m.Add(context.Background(), validAddRequest())
			if err == nil || !strings.Contains(err.Error(), "canonical") {
				t.Fatalf("Add error = %v, want canonical-source refusal", err)
			}
			assertBytesEqual(t, m.SecretsPath, beforeSecrets)
		})
	}
}

func TestManagerLegacyEmptyCredentialFailsClosed(t *testing.T) {
	m := newTestManager(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	legacy := "[provider]\nname = \"anthropic\"\nmodel = \"claude-old\"\n"
	if err := os.WriteFile(m.ConfigPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeConfig := readMaybe(t, m.ConfigPath)
	beforeSecrets := readMaybe(t, m.SecretsPath)
	err := m.Add(context.Background(), validAddRequest())
	if err == nil || !strings.Contains(err.Error(), "legacy provider credential") {
		t.Fatalf("Add error = %v, want fail-closed credential guidance", err)
	}
	assertBytesEqual(t, m.ConfigPath, beforeConfig)
	assertBytesEqual(t, m.SecretsPath, beforeSecrets)
}

func TestManagerActivateAndRemoveModelAvoidInstalledDeadEnd(t *testing.T) {
	m := newTestManager(t)
	req := validAddRequest()
	req.DefaultModel = ""
	req.UtilityModel = ""
	if err := m.Add(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateModel(context.Background(), "gpt"); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background())
	if err != nil || status.State != "ready" || status.DefaultModel != "gpt" {
		t.Fatalf("activated status = %#v err=%v", status, err)
	}
	if err := m.RemoveModel(context.Background(), "gpt", ""); err != nil {
		t.Fatal(err)
	}
	status, err = m.Status(context.Background())
	if err != nil || status.State != "installed" || status.DefaultModel != "" {
		t.Fatalf("removed status = %#v err=%v", status, err)
	}
	if err := m.Remove(context.Background(), "openai"); err != nil {
		t.Fatalf("remove unreferenced provider: %v", err)
	}
}

func TestManagerRemoveModelReassignsDefaultAndUtility(t *testing.T) {
	m := newTestManager(t)
	req := validAddRequest()
	req.Models["small"] = config.ModelTarget{Model: "gpt-small"}
	if err := m.Add(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveModel(context.Background(), "gpt", "small"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.DefaultModel != "small" || cfg.Agent.UtilityModel != "small" {
		t.Fatalf("agent aliases = default:%q utility:%q", cfg.Agent.DefaultModel, cfg.Agent.UtilityModel)
	}
}

func TestDeploymentDocsDescribeModelAndProviderRemovalSemantics(t *testing.T) {
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "deploy.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Join(strings.Fields(string(docs)), " ")
	for _, want := range []string{
		"`--replace-with` reassigns default, utility, and agent-profile references",
		"Without it, default and utility references are cleared",
		"can move a Ready installation back to Installed",
		"Agent-profile references remain blocking without a replacement",
		"Provider removal remains blocked while any model alias references the connection",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("docs/deploy.md missing model-removal guidance %q", want)
		}
	}
}

func TestManagerRecoversDurableJournalAfterSimulatedProcessDeath(t *testing.T) {
	for _, phase := range []string{"secret_committed", "config_committed", "activated", "healthy"} {
		t.Run(phase, func(t *testing.T) {
			m := newTestManager(t)
			beforeConfig := readMaybe(t, m.ConfigPath)
			beforeSecrets := readMaybe(t, m.SecretsPath)
			m.CrashAfterPhase = func(got string) error {
				if got == phase {
					return ErrSimulatedCrash
				}
				return nil
			}
			err := m.Add(context.Background(), validAddRequest())
			if !errors.Is(err, ErrSimulatedCrash) {
				t.Fatalf("Add error = %v, want simulated crash after %s", err, phase)
			}
			m.CrashAfterPhase = nil
			status, recoverErr := m.Status(context.Background())
			if recoverErr != nil {
				t.Fatal(recoverErr)
			}
			if phase == "healthy" {
				if status.State != "ready" {
					t.Fatalf("healthy-generation recovery = %#v", status)
				}
				return
			}
			if status.State != "installed" {
				t.Fatalf("rollback recovery = %#v", status)
			}
			assertBytesEqual(t, m.ConfigPath, beforeConfig)
			assertBytesEqual(t, m.SecretsPath, beforeSecrets)
		})
	}
}

func TestManagerRollbackRestoresActualActiveServiceState(t *testing.T) {
	m := newTestManager(t)
	active := true
	m.ServiceActive = func(context.Context) (bool, error) { return active, nil }
	m.RestoreService = func(_ context.Context, wasActive bool) error {
		active = wasActive
		return nil
	}
	m.Restart = func(context.Context) error {
		active = false
		return errors.New("restart failed")
	}
	if err := m.Add(context.Background(), validAddRequest()); err == nil {
		t.Fatal("Add succeeded, want restart failure")
	}
	if !active {
		t.Fatal("rollback did not restore previously active service")
	}
}

func TestManagerCrashRecoveryRestoresOriginallyAbsentFiles(t *testing.T) {
	m := newTestManager(t)
	if err := os.Remove(m.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.SecretsPath); err != nil {
		t.Fatal(err)
	}
	m.CrashAfterPhase = func(phase string) error {
		if phase == "config_committed" {
			return ErrSimulatedCrash
		}
		return nil
	}
	if err := m.Add(context.Background(), validAddRequest()); !errors.Is(err, ErrSimulatedCrash) {
		t.Fatalf("Add error = %v", err)
	}
	m.CrashAfterPhase = nil
	if _, err := m.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{m.ConfigPath, m.SecretsPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("originally absent %s was not removed: %v", path, err)
		}
	}
}

func TestManagerReadyRequiresActiveCommittedGeneration(t *testing.T) {
	m := newTestManager(t)
	if err := m.Add(context.Background(), validAddRequest()); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(m.ConfigPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n# generation changed without activation\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "installed" {
		t.Fatalf("stale healthy process reported %#v, want installed", status)
	}
}

func TestManagerListUsesOneLockConsistentSnapshot(t *testing.T) {
	m := newTestManager(t)
	req := validAddRequest()
	req.Models["small"] = config.ModelTarget{Model: "gpt-small"}
	if err := m.Add(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	paused := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m.afterReadStatus = func() {
		once.Do(func() { close(paused) })
		<-release
	}
	listDone := make(chan struct{})
	var listing []byte
	var listErr error
	go func() {
		listing, listErr = m.List(context.Background())
		close(listDone)
	}()
	<-paused
	mutationDone := make(chan error, 1)
	go func() { mutationDone <- m.RemoveModel(context.Background(), "gpt", "small") }()
	mutationReturned := false
	select {
	case err := <-mutationDone:
		mutationReturned = true
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("mutation result while List snapshot was paused = %v, want ErrLocked", err)
		}
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-listDone
	if listErr != nil {
		t.Fatal(listErr)
	}
	if !bytes.Contains(listing, []byte(`"default_model": "gpt"`)) || !bytes.Contains(listing, []byte(`"gpt"`)) {
		t.Fatalf("List mixed snapshots: %s", listing)
	}
	if mutationReturned {
		if err := m.RemoveModel(context.Background(), "gpt", "small"); err != nil {
			t.Fatal(err)
		}
	} else if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
}

func TestManagerTestRecoversEveryCrashLeftJournalPhase(t *testing.T) {
	for _, phase := range []string{"secret_committed", "config_committed", "activated", "healthy"} {
		t.Run(phase, func(t *testing.T) {
			m := newTestManager(t)
			m.CrashAfterPhase = func(got string) error {
				if got == phase {
					return ErrSimulatedCrash
				}
				return nil
			}
			if err := m.Add(context.Background(), validAddRequest()); !errors.Is(err, ErrSimulatedCrash) {
				t.Fatalf("Add error = %v", err)
			}
			m.CrashAfterPhase = nil
			probes := 0
			m.Probe = func(_ context.Context, target config.ResolvedModel, key string) error {
				probes++
				if target.Alias != "gpt" || key != providerTestKey {
					t.Fatalf("probe target=%#v key=%q", target, key)
				}
				return nil
			}
			err := m.Test(context.Background(), "openai")
			if phase == "healthy" {
				if err != nil || probes != 1 {
					t.Fatalf("healthy Test err=%v probes=%d", err, probes)
				}
			} else if err == nil || probes != 0 {
				t.Fatalf("rolled-back Test err=%v probes=%d", err, probes)
			}
			if _, statErr := os.Stat(m.journalPath()); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("journal remains after Test recovery: %v", statErr)
			}
		})
	}
}

func TestManagerAddModelCommitsExactAliasAndRoles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		def     bool
		utility bool
	}{
		{name: "no role"},
		{name: "default only", def: true},
		{name: "utility only", utility: true},
		{name: "both roles", def: true, utility: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			enrollConnectionWithoutRoles(t, m, false)
			beforeCfg, err := config.Load(m.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeSecrets := readMaybe(t, m.SecretsPath)
			probes := 0
			m.Probe = func(_ context.Context, target config.ResolvedModel, key string) error {
				probes++
				if target.Alias != "favourite" || target.ConnectionName != "openai" || target.UpstreamModel != "gpt-upstream-exact" {
					t.Fatalf("probe target = %#v", target)
				}
				if key != providerTestKey {
					t.Fatalf("probe key = %q, want provider test key", key)
				}
				return nil
			}

			err = m.AddModel(context.Background(), AddModelRequest{
				ConnectionName: "openai",
				Alias:          "favourite",
				UpstreamModel:  "gpt-upstream-exact",
				Default:        tc.def,
				Utility:        tc.utility,
			})
			if err != nil {
				t.Fatalf("AddModel: %v", err)
			}
			if probes != 1 {
				t.Fatalf("probe count = %d, want 1", probes)
			}
			got, err := config.Load(m.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Providers, beforeCfg.Providers) {
				t.Fatalf("providers changed:\n got %#v\nwant %#v", got.Providers, beforeCfg.Providers)
			}
			if got.Models["gpt"] != beforeCfg.Models["gpt"] || got.Models["favourite"] != (config.ModelTarget{Provider: "openai", Model: "gpt-upstream-exact"}) || len(got.Models) != len(beforeCfg.Models)+1 {
				t.Fatalf("models = %#v", got.Models)
			}
			wantAgent := beforeCfg.Agent
			if tc.def {
				wantAgent.DefaultModel = "favourite"
			}
			if tc.utility {
				wantAgent.UtilityModel = "favourite"
			}
			if !reflect.DeepEqual(got.Agent, wantAgent) {
				t.Fatalf("agent = %#v, want %#v", got.Agent, wantAgent)
			}
			assertBytesEqual(t, m.SecretsPath, beforeSecrets)
			if body := readMaybe(t, m.ConfigPath); !bytes.Contains(body, []byte("# keep this comment")) || !bytes.Contains(body, []byte("level = \"debug\"")) {
				t.Fatalf("AddModel changed unrelated TOML:\n%s", body)
			}
			status, err := m.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			wantState := "installed"
			if tc.def {
				wantState = "ready"
			}
			if status.State != wantState || status.DefaultModel != wantAgent.DefaultModel {
				t.Fatalf("status = %#v, want state=%q default=%q", status, wantState, wantAgent.DefaultModel)
			}
			active, err := m.ServiceActive(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if active != tc.def {
				t.Fatalf("service active = %v, want %v", active, tc.def)
			}
			ready := readMaybe(t, m.readyPath())
			if tc.def {
				if want := generationBytes(readMaybe(t, m.ConfigPath)); !bytes.Equal(ready, want) {
					t.Fatalf("ready generation = %q, want %q", ready, want)
				}
			} else if ready != nil {
				t.Fatalf("installed state wrote ready generation %q", ready)
			}
		})
	}
}

func TestManagerAddModelProbeFailureRollsBack(t *testing.T) {
	m := newTestManager(t)
	enrollConnectionWithoutRoles(t, m, false)
	before := captureManagerState(t, m)
	m.Probe = func(context.Context, config.ResolvedModel, string) error {
		return errors.New("probe rejected " + providerTestKey)
	}

	err := m.AddModel(context.Background(), AddModelRequest{ConnectionName: "openai", Alias: "favourite", UpstreamModel: "gpt-new", Default: true})
	if err == nil || !strings.Contains(err.Error(), "probe") {
		t.Fatalf("AddModel error = %v, want probe failure", err)
	}
	if strings.Contains(err.Error(), providerTestKey) {
		t.Fatalf("AddModel error leaked credential: %v", err)
	}
	assertManagerState(t, m, before)
}

func TestManagerAddModelRejectsActiveKeyBeforeStaging(t *testing.T) {
	for _, tt := range []struct {
		name string
		req  AddModelRequest
	}{
		{
			name: "alias",
			req: AddModelRequest{
				ConnectionName: "openai",
				Alias:          providerTestKey,
				UpstreamModel:  "gpt-new",
			},
		},
		{
			name: "upstream model",
			req: AddModelRequest{
				ConnectionName: "openai",
				Alias:          "favourite",
				UpstreamModel:  "vendor/" + providerTestKey,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager(t)
			enrollConnectionWithoutRoles(t, m, false)
			before := captureManagerState(t, m)
			probes := 0
			m.Probe = func(context.Context, config.ResolvedModel, string) error {
				probes++
				return nil
			}

			err := m.AddModel(t.Context(), tt.req)
			if err == nil || !strings.Contains(err.Error(), "durable provider configuration contains the active API key") {
				t.Fatalf("AddModel() error = %v, want active-key persistence rejection", err)
			}
			assertErrorTreeRedacted(t, err)
			if probes != 0 {
				t.Fatalf("probe calls = %d, want zero", probes)
			}
			assertManagerState(t, m, before)
			assertNoProviderStageFiles(t, filepath.Dir(m.ConfigPath))
		})
	}
}

func TestManagerAddModelAuthFreeWithoutSecretsPreservesAbsence(t *testing.T) {
	m := newTestManager(t)
	if err := os.Remove(m.SecretsPath); err != nil {
		t.Fatal(err)
	}
	initial := "# auth-free provider\n[providers.local]\ntype = \"openai\"\nbase_url = \"http://127.0.0.1:11434/v1\"\n\n[models.existing]\nprovider = \"local\"\nmodel = \"existing-upstream\"\n"
	if err := os.WriteFile(m.ConfigPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Probe = func(_ context.Context, target config.ResolvedModel, key string) error {
		if target.Alias != "favourite" || target.ConnectionName != "local" || target.UpstreamModel != "new-upstream" {
			t.Fatalf("probe target = %#v", target)
		}
		if key != "" {
			t.Fatalf("auth-free probe key = %q, want empty", key)
		}
		return nil
	}

	if err := m.AddModel(context.Background(), AddModelRequest{ConnectionName: "local", Alias: "favourite", UpstreamModel: "new-upstream"}); err != nil {
		t.Fatalf("AddModel: %v", err)
	}
	if _, err := os.Stat(m.SecretsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op secret stage created secrets file: %v", err)
	}
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models["existing"] != (config.ModelTarget{Provider: "local", Model: "existing-upstream"}) || cfg.Models["favourite"] != (config.ModelTarget{Provider: "local", Model: "new-upstream"}) {
		t.Fatalf("models = %#v", cfg.Models)
	}
}

func TestManagerAddModelLifecycleFailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		restartErr error
		healthErr  error
		restoreErr error
	}{
		{name: "restart", restartErr: errors.New("restart failed " + providerTestKey)},
		{name: "health", healthErr: errors.New("health failed " + providerTestKey)},
		{
			name:       "restart and restore errors",
			restartErr: errors.New("restart failed " + providerTestKey),
			restoreErr: errors.New("restore failed " + providerTestKey),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			enrollConnectionWithoutRoles(t, m, false)
			active := false
			m.ServiceActive = func(context.Context) (bool, error) { return active, nil }
			m.Restart = func(context.Context) error {
				active = true
				return tc.restartErr
			}
			m.Health = func(context.Context) error { return tc.healthErr }
			m.RestoreService = func(_ context.Context, wasActive bool) error {
				active = wasActive
				return tc.restoreErr
			}
			before := captureManagerState(t, m)

			err := m.AddModel(context.Background(), AddModelRequest{ConnectionName: "openai", Alias: "favourite", UpstreamModel: "gpt-new", Default: true})
			if err == nil {
				t.Fatal("AddModel succeeded, want lifecycle failure")
			}
			assertErrorTreeRedacted(t, err)
			assertManagerState(t, m, before)
		})
	}
}

func assertErrorTreeRedacted(t *testing.T, err error) {
	t.Helper()
	var walk func(error)
	walk = func(current error) {
		if current == nil {
			return
		}
		if strings.Contains(current.Error(), providerTestKey) {
			t.Fatalf("error tree leaked credential: %v", current)
		}
		switch unwrapped := current.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range unwrapped.Unwrap() {
				walk(child)
			}
		case interface{ Unwrap() error }:
			walk(unwrapped.Unwrap())
		}
	}
	walk(err)
}

func TestManagerAddModelRejectsUnknownConnectionInvalidAliasAndCollision(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  AddModelRequest
		want string
	}{
		{name: "unknown connection", req: AddModelRequest{ConnectionName: "missing", Alias: "new", UpstreamModel: "gpt-new"}, want: "does not exist"},
		{name: "invalid alias", req: AddModelRequest{ConnectionName: "openai", Alias: "BAD ALIAS", UpstreamModel: "gpt-new"}, want: "invalid model alias"},
		{name: "missing upstream", req: AddModelRequest{ConnectionName: "openai", Alias: "new", UpstreamModel: "  "}, want: "upstream model is required"},
		{name: "alias collision", req: AddModelRequest{ConnectionName: "openai", Alias: "gpt", UpstreamModel: "gpt-new"}, want: "already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			enrollConnectionWithoutRoles(t, m, false)
			before := captureManagerState(t, m)
			probes := 0
			m.Probe = func(context.Context, config.ResolvedModel, string) error { probes++; return nil }

			err := m.AddModel(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AddModel error = %v, want %q", err, tc.want)
			}
			if probes != 0 {
				t.Fatalf("probe count = %d, want 0", probes)
			}
			if strings.Contains(err.Error(), providerTestKey) {
				t.Fatalf("AddModel error leaked credential: %v", err)
			}
			assertManagerState(t, m, before)
		})
	}
}

func TestManagerCatalogSnapshotReturnsConnectionAndDecryptedKey(t *testing.T) {
	m := newTestManager(t)
	m.Random = bytes.NewReader(bytes.Repeat([]byte{0xab}, 32))
	enrollConnectionWithoutRoles(t, m, false)
	before := captureManagerState(t, m)

	snapshot, err := m.CatalogSnapshot(context.Background(), "openai")
	if err != nil {
		t.Fatalf("CatalogSnapshot: %v", err)
	}
	if snapshot.Connection != (config.ProviderConnection{Type: "openai", APIKey: "secret://provider/openai/api-key", BaseURL: "https://api.openai.example/v1"}) {
		t.Fatalf("connection = %#v", snapshot.Connection)
	}
	if snapshot.APIKey != providerTestKey {
		t.Fatalf("APIKey = %q, want decrypted provider key", snapshot.APIKey)
	}
	if snapshot.ScopeID != strings.Repeat("ab", 32) {
		t.Fatalf("ScopeID = %q", snapshot.ScopeID)
	}
	assertStoredSecret(t, m, "provider/openai/catalog-scope", snapshot.ScopeID)
	assertManagerState(t, m, before)
	if bytes.Contains(readMaybe(t, m.ConfigPath), []byte(snapshot.ScopeID)) {
		t.Fatal("config serialized catalogue scope")
	}
}

func TestManagerCatalogSnapshotSupportsAuthFreeAndRejectsUnknownConnection(t *testing.T) {
	m := newTestManager(t)
	m.Random = bytes.NewReader(bytes.Repeat([]byte{0x7c}, 32))
	enrollConnectionWithoutRoles(t, m, true)

	snapshot, err := m.CatalogSnapshot(context.Background(), "openai")
	if err != nil {
		t.Fatalf("CatalogSnapshot auth-free: %v", err)
	}
	if snapshot.APIKey != "" || snapshot.ScopeID != strings.Repeat("7c", 32) || snapshot.Connection.APIKey != "" {
		t.Fatalf("auth-free snapshot = %#v", snapshot)
	}
	before := captureManagerState(t, m)
	_, err = m.CatalogSnapshot(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("CatalogSnapshot error = %v, want unknown connection", err)
	}
	if strings.Contains(err.Error(), providerTestKey) {
		t.Fatalf("CatalogSnapshot error leaked credential: %v", err)
	}
	assertManagerState(t, m, before)
}

func TestManagerCatalogSnapshotRejectsDurableConnectionMetadataContainingActiveKey(t *testing.T) {
	m := newTestManager(t)
	enrollConnectionWithoutRoles(t, m, false)

	configBytes := readMaybe(t, m.ConfigPath)
	configBytes = bytes.ReplaceAll(
		configBytes,
		[]byte("https://api.openai.example/v1"),
		[]byte("https://api.openai.example/v1/"+providerTestKey),
	)
	if err := os.WriteFile(m.ConfigPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := secret.OpenFile(m.SecretsPath, m.Identity).Delete("provider/openai/catalog-scope"); err != nil {
		t.Fatal(err)
	}
	before := captureManagerState(t, m)

	_, err := m.CatalogSnapshot(t.Context(), "openai")
	if err == nil || !strings.Contains(err.Error(), "durable provider configuration contains the active API key") {
		t.Fatalf("CatalogSnapshot() error = %v, want active-key persistence rejection", err)
	}
	assertErrorTreeRedacted(t, err)
	assertManagerState(t, m, before)
}

func TestManagerEnrollmentScopesDifferAcrossRemoveAndReAdd(t *testing.T) {
	m := newTestManager(t)
	m.Random = bytes.NewReader(append(bytes.Repeat([]byte{0x11}, 32), bytes.Repeat([]byte{0x22}, 32)...))
	enrollConnectionWithoutRoles(t, m, false)
	first, err := m.CatalogSnapshot(context.Background(), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveModel(context.Background(), "gpt", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(context.Background(), "openai"); err != nil {
		t.Fatal(err)
	}
	store := secret.OpenFile(m.SecretsPath, m.Identity)
	for _, name := range []string{"provider/openai/api-key", "provider/openai/catalog-scope"} {
		if _, err := store.Get(name); !errors.Is(err, secret.ErrNotFound) {
			t.Fatalf("removed secret %q error = %v, want ErrNotFound", name, err)
		}
	}
	enrollConnectionWithoutRoles(t, m, false)
	second, err := m.CatalogSnapshot(context.Background(), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if first.ScopeID == second.ScopeID || first.ScopeID != strings.Repeat("11", 32) || second.ScopeID != strings.Repeat("22", 32) {
		t.Fatalf("scope IDs first=%q second=%q", first.ScopeID, second.ScopeID)
	}
}

func TestManagerCatalogSnapshotBackfillsLegacyScopeUnderLock(t *testing.T) {
	t.Run("backfill", func(t *testing.T) {
		m := newTestManager(t)
		enrollConnectionWithoutRoles(t, m, false)
		store := secret.OpenFile(m.SecretsPath, m.Identity)
		if err := store.Delete("provider/openai/catalog-scope"); err != nil {
			t.Fatal(err)
		}
		m.Random = bytes.NewReader(bytes.Repeat([]byte{0xcd}, 32))
		before := captureManagerState(t, m)
		lease, err := instance.Default(m.LockPath).Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, lockedErr := m.CatalogSnapshot(context.Background(), "openai")
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(lockedErr, ErrLocked) {
			t.Fatalf("CatalogSnapshot under held lock error = %v, want ErrLocked", lockedErr)
		}
		assertManagerState(t, m, before)

		snapshot, err := m.CatalogSnapshot(context.Background(), "openai")
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.ScopeID != strings.Repeat("cd", 32) {
			t.Fatalf("backfilled scope = %q", snapshot.ScopeID)
		}
		if got := readMaybe(t, m.ConfigPath); !bytes.Equal(got, before.config) {
			t.Fatal("scope backfill modified config")
		}
		if got := readMaybe(t, m.readyPath()); !bytes.Equal(got, before.ready) {
			t.Fatal("scope backfill modified ready generation")
		}
		active, err := m.ServiceActive(context.Background())
		if err != nil || active != before.serviceActive {
			t.Fatalf("scope backfill service active=%v err=%v", active, err)
		}
		assertStoredSecret(t, m, "existing/value", "preserve-me")
		assertStoredSecret(t, m, "provider/openai/api-key", providerTestKey)
		assertStoredSecret(t, m, "provider/openai/catalog-scope", snapshot.ScopeID)
		names, err := store.List()
		if err != nil {
			t.Fatal(err)
		}
		wantNames := []string{"existing/value", "provider/openai/api-key", "provider/openai/catalog-scope"}
		if !reflect.DeepEqual(names, wantNames) {
			t.Fatalf("secret names = %v, want %v", names, wantNames)
		}
	})

	t.Run("generation failure", func(t *testing.T) {
		m := newTestManager(t)
		enrollConnectionWithoutRoles(t, m, false)
		if err := secret.OpenFile(m.SecretsPath, m.Identity).Delete("provider/openai/catalog-scope"); err != nil {
			t.Fatal(err)
		}
		m.Random = strings.NewReader("short")
		before := captureManagerState(t, m)
		_, err := m.CatalogSnapshot(context.Background(), "openai")
		if err == nil || !strings.Contains(err.Error(), "catalogue access") {
			t.Fatalf("CatalogSnapshot error = %v, want catalogue access failure", err)
		}
		if strings.Contains(err.Error(), providerTestKey) {
			t.Fatalf("CatalogSnapshot error leaked credential: %v", err)
		}
		assertManagerState(t, m, before)
	})
}

type managerState struct {
	config        []byte
	secrets       []byte
	ready         []byte
	serviceActive bool
}

func captureManagerState(t *testing.T, m *Manager) managerState {
	t.Helper()
	active, err := m.ServiceActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return managerState{
		config:        readMaybe(t, m.ConfigPath),
		secrets:       readMaybe(t, m.SecretsPath),
		ready:         readMaybe(t, m.readyPath()),
		serviceActive: active,
	}
}

func assertManagerState(t *testing.T, m *Manager, want managerState) {
	t.Helper()
	assertBytesEqual(t, m.ConfigPath, want.config)
	assertBytesEqual(t, m.SecretsPath, want.secrets)
	assertBytesEqual(t, m.readyPath(), want.ready)
	active, err := m.ServiceActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active != want.serviceActive {
		t.Fatalf("service active = %v, want %v", active, want.serviceActive)
	}
}

func assertNoProviderStageFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".provider-stage-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("provider staging files remain: %v", matches)
	}
}

func enrollConnectionWithoutRoles(t *testing.T, m *Manager, authFree bool) {
	t.Helper()
	req := validAddRequest()
	req.DefaultModel = ""
	req.UtilityModel = ""
	if authFree {
		req.APIKey = ""
	}
	if err := m.Add(context.Background(), req); err != nil {
		t.Fatalf("enroll connection: %v", err)
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.toml")
	initial := "# keep this comment\n[gateway]\nlisten = \"127.0.0.1:8420\"\n\n[log]\nlevel = \"debug\"\n"
	if err := os.WriteFile(configPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	secretsPath := filepath.Join(dir, "secrets.age")
	store := secret.OpenFile(secretsPath, id)
	if err := store.Set("existing/value", "preserve-me"); err != nil {
		t.Fatal(err)
	}
	active := false
	return &Manager{
		ConfigPath:     configPath,
		SecretsPath:    secretsPath,
		LockPath:       filepath.Join(dir, "provider-config.lock"),
		Identity:       id,
		Probe:          func(context.Context, config.ResolvedModel, string) error { return nil },
		Restart:        func(context.Context) error { active = true; return nil },
		Health:         func(context.Context) error { return nil },
		ServiceActive:  func(context.Context) (bool, error) { return active, nil },
		Stop:           func(context.Context) error { active = false; return nil },
		RestoreService: func(_ context.Context, wasActive bool) error { active = wasActive; return nil },
	}
}

func validAddRequest() AddRequest {
	return AddRequest{
		ConnectionName: "openai",
		Connection: config.ProviderConnection{
			Type:    "openai",
			BaseURL: "https://api.openai.example/v1",
		},
		Models: map[string]config.ModelTarget{
			"gpt": {Model: "gpt-test"},
		},
		DefaultModel: "gpt",
		UtilityModel: "gpt",
		APIKey:       providerTestKey,
	}
}

func readMaybe(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertBytesEqual(t *testing.T, path string, want []byte) {
	t.Helper()
	if got := readMaybe(t, path); !bytes.Equal(got, want) {
		t.Fatalf("%s changed:\n got %x\nwant %x", path, got, want)
	}
}

func assertStoredSecret(t *testing.T, m *Manager, name, want string) {
	t.Helper()
	got, err := secret.OpenFile(m.SecretsPath, m.Identity).Get(name)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("secret %s = %q, want expected value", name, got)
	}
}
