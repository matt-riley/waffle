package providerconfig

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
