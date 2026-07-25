package providerconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
)

func TestManagerSnapshotIsTypedCredentialFreeAndIncludesUtilityRole(t *testing.T) {
	m := newTestManager(t)
	req := validAddRequest()
	req.Models["small"] = config.ModelTarget{Model: "gpt-small"}
	req.UtilityModel = "small"
	if err := m.Add(t.Context(), req); err != nil {
		t.Fatal(err)
	}

	snapshot, err := m.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DefaultModel != "gpt" || snapshot.UtilityModel != "small" {
		t.Fatalf("roles = default:%q utility:%q", snapshot.DefaultModel, snapshot.UtilityModel)
	}
	if snapshot.Providers["openai"].Type != "openai" ||
		snapshot.Models["small"].Model != "gpt-small" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	public, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(public, []byte(providerTestKey)) {
		t.Fatalf("snapshot leaked credential: %s", public)
	}
	listing, err := m.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var decoded Listing
	if err := json.Unmarshal(listing, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, snapshot) {
		t.Fatalf("List decoded = %#v, Snapshot = %#v", decoded, snapshot)
	}
}

func TestManagerActivateUtilityModelChangesOnlyUtilityRole(t *testing.T) {
	m := newTestManager(t)
	req := validAddRequest()
	req.Models["small"] = config.ModelTarget{Model: "gpt-small"}
	if err := m.Add(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	before, err := config.Load(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	probes := 0
	m.Probe = func(_ context.Context, target config.ResolvedModel, key string) error {
		probes++
		if target.Alias != "small" || key != providerTestKey {
			t.Fatalf("probe target=%#v key=%q", target, key)
		}
		return nil
	}

	if err := m.ActivateUtilityModel(t.Context(), "small"); err != nil {
		t.Fatal(err)
	}
	after, err := config.Load(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	wantAgent := before.Agent
	wantAgent.UtilityModel = "small"
	if !reflect.DeepEqual(after.Providers, before.Providers) ||
		!reflect.DeepEqual(after.Models, before.Models) ||
		!reflect.DeepEqual(after.Agent, wantAgent) {
		t.Fatalf("unrelated config changed:\nbefore=%#v\nafter=%#v", before, after)
	}
	if probes != 1 {
		t.Fatalf("probe calls = %d, want 1", probes)
	}
}

func TestManagerDeferredMutationsCommitAwaitingRestartWithoutLifecycle(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, *Manager) (MutationResult, error)
	}{
		{
			name: "provider enrollment",
			mutate: func(ctx context.Context, m *Manager) (MutationResult, error) {
				return m.AddWithMode(ctx, validAddRequest(), CommitForRestart)
			},
		},
		{
			name: "add model",
			mutate: func(ctx context.Context, m *Manager) (MutationResult, error) {
				enrollConnectionWithoutRoles(t, m, false)
				return m.AddModelWithMode(ctx, AddModelRequest{
					ConnectionName: "openai",
					Alias:          "favourite",
					UpstreamModel:  "gpt-favourite",
				}, CommitForRestart)
			},
		},
		{
			name: "default role",
			mutate: func(ctx context.Context, m *Manager) (MutationResult, error) {
				enrollConnectionWithoutRoles(t, m, false)
				return m.ActivateModelWithMode(ctx, "gpt", CommitForRestart)
			},
		},
		{
			name: "utility role",
			mutate: func(ctx context.Context, m *Manager) (MutationResult, error) {
				enrollConnectionWithoutRoles(t, m, false)
				return m.ActivateUtilityModelWithMode(ctx, "gpt", CommitForRestart)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager(t)
			restarts, stops, health := 0, 0, 0
			m.Restart = func(context.Context) error { restarts++; return nil }
			m.Stop = func(context.Context) error { stops++; return nil }
			m.Health = func(context.Context) error { health++; return nil }

			result, err := tt.mutate(t.Context(), m)
			if err != nil {
				t.Fatal(err)
			}
			if !result.RestartRequired || result.TransactionID == "" {
				t.Fatalf("result = %#v", result)
			}
			if restarts != 0 || stops != 0 || health != 0 {
				t.Fatalf("lifecycle calls restart=%d stop=%d health=%d", restarts, stops, health)
			}
			journalBytes, err := os.ReadFile(m.journalPath())
			if err != nil {
				t.Fatal(err)
			}
			var journal transactionJournal
			if err := json.Unmarshal(journalBytes, &journal); err != nil {
				t.Fatal(err)
			}
			if journal.Phase != "awaiting_restart" || journal.TransactionID != result.TransactionID {
				t.Fatalf("journal = %#v, result = %#v", journal, result)
			}
			for _, path := range []string{m.ConfigPath + ".bak", m.SecretsPath + ".bak"} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("deferred backup %q missing: %v", path, err)
				}
			}
		})
	}
}

func TestManagerFinalizeDeferredFinalizesHealthyOrRollsBackFailure(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		m := newTestManager(t)
		before := captureManagerState(t, m)
		result, err := m.AddWithMode(t.Context(), validAddRequest(), CommitForRestart)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.FinalizeDeferred(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(m.journalPath()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("journal remains after finalization: %v", err)
		}
		if got, want := readMaybe(t, m.readyPath()), generationBytes(readMaybe(t, m.ConfigPath)); !bytes.Equal(got, want) {
			t.Fatalf("ready generation = %q, want %q", got, want)
		}
		if result.TransactionID == "" || bytes.Equal(readMaybe(t, m.ConfigPath), before.config) {
			t.Fatal("deferred config was not committed")
		}
		if err := m.FinalizeDeferred(t.Context()); err != nil {
			t.Fatalf("idempotent finalization: %v", err)
		}
	})

	t.Run("failed health rolls back", func(t *testing.T) {
		m := newTestManager(t)
		before := captureManagerState(t, m)
		if _, err := m.AddWithMode(t.Context(), validAddRequest(), CommitForRestart); err != nil {
			t.Fatal(err)
		}
		m.Health = func(context.Context) error { return errors.New("new process unhealthy") }
		err := m.FinalizeDeferred(t.Context())
		if err == nil || !errors.Is(err, ErrDeferredHealth) {
			t.Fatalf("FinalizeDeferred error = %v, want ErrDeferredHealth", err)
		}
		assertManagerState(t, m, before)
		if _, statErr := os.Stat(m.journalPath()); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("journal remains after rollback: %v", statErr)
		}
	})
}

func TestManagerDeferredRecoveryUsesJournalPayloadAfterStartup(t *testing.T) {
	m := newTestManager(t)
	req := validAddRequest()
	req.Models["small"] = config.ModelTarget{Model: "gpt-small"}
	if err := m.Add(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	changes := []SessionAliasChange{{SessionID: "session-1", From: "gpt", To: "small"}}
	m.SetSessionRecovery(func(context.Context, []SessionAliasChange) error { return nil })
	result, err := m.RemoveModelWithModeAtRevision(context.Background(), "gpt", "small", "", changes, CommitForRestart)
	if err != nil {
		t.Fatal(err)
	}
	if result.TransactionID == "" {
		t.Fatal("deferred transaction did not return an ID")
	}
	j, present, err := m.readJournal()
	if err != nil || !present {
		t.Fatalf("read journal: present=%v err=%v", present, err)
	}
	if !reflect.DeepEqual(j.SessionAliasChanges, changes) {
		t.Fatalf("journal session changes = %#v, want %#v", j.SessionAliasChanges, changes)
	}
	var recovered []SessionAliasChange
	m.SetSessionRecovery(func(_ context.Context, got []SessionAliasChange) error {
		recovered = append([]SessionAliasChange(nil), got...)
		return nil
	})
	m.Health = func(context.Context) error { return errors.New("new process unhealthy") }
	if err := m.FinalizeDeferred(context.Background()); !errors.Is(err, ErrDeferredHealth) {
		t.Fatalf("FinalizeDeferred error = %v, want ErrDeferredHealth", err)
	}
	if !reflect.DeepEqual(recovered, changes) {
		t.Fatalf("recovered session changes = %#v, want %#v", recovered, changes)
	}
}

func TestManagerFinalizeDeferredRejectsUnboundOrTamperedTransactions(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*testing.T, *Manager)
	}{
		{
			name: "missing transaction ID",
			tamper: func(t *testing.T, m *Manager) {
				journal, present, err := m.readJournal()
				if err != nil || !present {
					t.Fatalf("read journal: present=%v err=%v", present, err)
				}
				journal.TransactionID = ""
				if err := m.writeJournal(journal); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed transaction ID",
			tamper: func(t *testing.T, m *Manager) {
				journal, present, err := m.readJournal()
				if err != nil || !present {
					t.Fatalf("read journal: present=%v err=%v", present, err)
				}
				journal.TransactionID = "not-a-config-digest"
				if err := m.writeJournal(journal); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "stale transaction ID",
			tamper: func(t *testing.T, m *Manager) {
				journal, present, err := m.readJournal()
				if err != nil || !present {
					t.Fatalf("read journal: present=%v err=%v", present, err)
				}
				journal.TransactionID = strings.Repeat("0", 32)
				if err := m.writeJournal(journal); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "committed config changed after journal",
			tamper: func(t *testing.T, m *Manager) {
				raw, err := os.ReadFile(m.ConfigPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(m.ConfigPath, append(raw, []byte("\n# tampered after commit\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager(t)
			before := captureManagerState(t, m)
			if _, err := m.AddWithMode(t.Context(), validAddRequest(), CommitForRestart); err != nil {
				t.Fatal(err)
			}
			healthCalls := 0
			m.Health = func(context.Context) error {
				healthCalls++
				return nil
			}
			tt.tamper(t, m)

			err := m.FinalizeDeferred(t.Context())

			if err == nil || !strings.Contains(err.Error(), "deferred provider transaction integrity check failed") {
				t.Fatalf("FinalizeDeferred error = %v, want integrity failure", err)
			}
			if healthCalls != 0 {
				t.Fatalf("health calls = %d, want zero before integrity validation", healthCalls)
			}
			assertManagerState(t, m, before)
			for _, path := range []string{
				m.journalPath(),
				m.ConfigPath + ".bak",
				m.SecretsPath + ".bak",
				m.readyPath() + ".bak",
			} {
				if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("transaction evidence %q remains after successful rollback: %v", path, statErr)
				}
			}
		})
	}
}

func TestManagerFinalizeDeferredRetainsRecoveryEvidenceWhenIntegrityRollbackFails(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.AddWithMode(t.Context(), validAddRequest(), CommitForRestart); err != nil {
		t.Fatal(err)
	}
	journal, present, err := m.readJournal()
	if err != nil || !present {
		t.Fatalf("read journal: present=%v err=%v", present, err)
	}
	journal.TransactionID = ""
	if err := m.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.ConfigPath + ".bak"); err != nil {
		t.Fatal(err)
	}

	err = m.FinalizeDeferred(t.Context())

	if err == nil || !strings.Contains(err.Error(), "deferred provider transaction integrity check failed") {
		t.Fatalf("FinalizeDeferred error = %v, want integrity failure", err)
	}
	for _, path := range []string{m.journalPath(), m.SecretsPath + ".bak"} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("recovery evidence %q was discarded: %v", path, statErr)
		}
	}
}
