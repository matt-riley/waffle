package providerconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	tomltree "github.com/pelletier/go-toml"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/instance"
	"github.com/matt-riley/waffle/internal/secret"
)

type snapshot struct {
	configBytes   []byte
	secretBytes   []byte
	configMode    fs.FileMode
	secretMode    fs.FileMode
	configExist   bool
	secretExist   bool
	cfg           config.Config
	readyBytes    []byte
	readyMode     fs.FileMode
	readyExist    bool
	serviceActive bool
}

func (m *Manager) capture(ctx context.Context) (snapshot, error) {
	configBytes, configMode, configExist, err := readSnapshot(m.ConfigPath)
	if err != nil {
		return snapshot{}, err
	}
	secretBytes, secretMode, secretExist, err := readSnapshot(m.SecretsPath)
	if err != nil {
		return snapshot{}, err
	}
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		return snapshot{}, err
	}
	readyBytes, readyMode, readyExist, err := readSnapshot(m.readyPath())
	if err != nil {
		return snapshot{}, err
	}
	if m.ServiceActive == nil {
		return snapshot{}, errors.New("service active-state callback is not configured")
	}
	active, err := m.ServiceActive(ctx)
	if err != nil {
		return snapshot{}, fmt.Errorf("capture service active state: %w", err)
	}
	return snapshot{
		configBytes: configBytes, secretBytes: secretBytes,
		configMode: configMode, secretMode: secretMode,
		configExist: configExist, secretExist: secretExist, cfg: cfg,
		readyBytes: readyBytes, readyMode: readyMode, readyExist: readyExist,
		serviceActive: active,
	}, nil
}

func (m *Manager) acquire(ctx context.Context) (*instance.Lease, error) {
	lease, err := instance.Default(m.LockPath).Acquire(ctx)
	if errors.Is(err, instance.ErrHeld) {
		return nil, fmt.Errorf("%w: %s", ErrLocked, m.LockPath)
	}
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func (m *Manager) stageConfig(original []byte, mutate func(*tomlDocument, config.Config) error) (string, config.Config, error) {
	// Parse into a TOML syntax tree before mutation. The document editor below
	// changes only managed table/key spans so comments, ordering, and unrelated
	// settings retain their original bytes.
	_, err := tomltree.LoadBytes(original)
	if err != nil {
		return "", config.Config{}, fmt.Errorf("parse config syntax tree: %w", err)
	}
	base, err := config.Load(m.ConfigPath)
	if err != nil {
		return "", config.Config{}, err
	}
	if err := ensureCanonicalManagedSource(original, base); err != nil {
		return "", config.Config{}, err
	}
	doc := newTOMLDocument(original)
	if err := mutate(doc, base); err != nil {
		return "", config.Config{}, err
	}
	stage, err := writeStage(m.ConfigPath, doc.bytes(), 0o600)
	if err != nil {
		return "", config.Config{}, err
	}
	candidate, err := config.Load(stage)
	if err != nil {
		_ = os.Remove(stage)
		return "", config.Config{}, fmt.Errorf("validate staged config: %w", err)
	}
	return stage, candidate, nil
}

func (m *Manager) stageSecrets(original []byte, mutate func(secret.Store) error) (string, error) {
	if m.Identity == nil {
		return "", errors.New("secret-store identity is required")
	}
	stage, err := writeStage(m.SecretsPath, original, 0o600)
	if err != nil {
		return "", err
	}
	store := secret.OpenFile(stage, m.Identity)
	if len(original) == 0 {
		// An identity may legitimately exist before secrets.age. Seed and delete
		// a private sentinel so the staged resource is a valid encrypted empty
		// object even for an auth-free provider.
		if err := os.Remove(stage); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := store.Set("providerconfig/stage", "initialise"); err != nil {
			return "", err
		}
		if err := store.Delete("providerconfig/stage"); err != nil {
			return "", err
		}
	}
	if err := mutate(store); err != nil {
		_ = os.Remove(stage)
		return "", err
	}
	if err := syncFile(stage); err != nil {
		_ = os.Remove(stage)
		return "", err
	}
	return stage, nil
}

func (m *Manager) commit(ctx context.Context, before snapshot, configStage, secretStage string, candidate config.Config, key string) (err error) {
	return m.commitWithExpectedRevision(ctx, before, configStage, secretStage, candidate, key, "")
}

func (m *Manager) commitWithExpectedRevision(ctx context.Context, before snapshot, configStage, secretStage string, candidate config.Config, key, expectedRevision string) (err error) {
	state := commitState(ctx)
	if expectedRevision != "" {
		current, readErr := os.ReadFile(m.ConfigPath)
		if readErr != nil {
			return readErr
		}
		if transactionIDForBytes(current) != expectedRevision {
			return ErrRevisionMismatch
		}
	}
	transactionID, err := transactionIDForStage(configStage)
	if err != nil {
		return err
	}
	sessionAliasChanges, _ := ctx.Value(sessionAliasChangesContextKey{}).([]SessionAliasChange)
	if len(sessionAliasChanges) > 0 && (m.SessionApply == nil || m.SessionRecovery == nil) {
		return errors.New("session transaction handlers are not configured")
	}
	for _, change := range sessionAliasChanges {
		if change.FromVersion == 0 || change.ToVersion != change.FromVersion+1 || change.FromUpdatedAt == "" || change.ToUpdatedAt == "" {
			return errors.New("exact session alias transition version is required")
		}
	}
	journal := transactionJournal{
		Phase: "prepared", ConfigExisted: before.configExist, SecretExisted: before.secretExist,
		ReadyExisted: before.readyExist, ConfigMode: uint32(before.configMode),
		SecretMode: uint32(before.secretMode), ReadyMode: uint32(before.readyMode),
		ServiceActive: before.serviceActive, TransactionID: transactionID,
	}
	journal.SessionAliasChanges = append([]SessionAliasChange(nil), sessionAliasChanges...)
	if err := writeBackups(m, before); err != nil {
		return err
	}
	if err := m.writeJournal(journal); err != nil {
		return err
	}
	defer func() {
		if err == nil || errors.Is(err, ErrSimulatedCrash) {
			return
		}
		err = errors.Join(redactError(err, key), redactError(m.recoverLocked(ctx), key))
	}()
	if len(sessionAliasChanges) > 0 {
		if err = m.SessionApply(ctx, sessionAliasChanges); err != nil {
			return fmt.Errorf("apply session aliases: %w", err)
		}
		if err = m.crashPoint("session_applied"); err != nil {
			return err
		}
	}

	if secretStage != "" {
		if err = commitStage(secretStage, m.SecretsPath, 0o600); err != nil {
			return err
		}
	}
	if m.AfterCommit != nil {
		if err = m.AfterCommit("secret"); err != nil {
			return err
		}
	}
	if err = m.advanceJournal(&journal, "secret_committed"); err != nil {
		return err
	}
	if err = m.crashPoint("secret_committed"); err != nil {
		return err
	}
	if err = commitStage(configStage, m.ConfigPath, 0o600); err != nil {
		return err
	}
	if m.AfterCommit != nil {
		if err = m.AfterCommit("config"); err != nil {
			return err
		}
	}
	if err = m.advanceJournal(&journal, "config_committed"); err != nil {
		return err
	}
	if err = m.crashPoint("config_committed"); err != nil {
		return err
	}
	if state.mode == CommitForRestart {
		if err = m.advanceJournal(&journal, "awaiting_restart"); err != nil {
			return err
		}
		state.result = MutationResult{
			RestartRequired: true,
			TransactionID:   transactionID,
		}
		return nil
	}

	if candidate.Agent.DefaultModel == "" {
		if before.serviceActive {
			if m.Stop == nil {
				return errors.New("service stop callback is not configured")
			}
			if err = m.Stop(ctx); err != nil {
				return fmt.Errorf("stop waffle service: %w", err)
			}
		}
		if err = removeIfExists(m.readyPath()); err != nil {
			return err
		}
	} else {
		if m.Restart == nil || m.Health == nil {
			return errors.New("service activation callbacks are not configured")
		}
		if err = m.Restart(ctx); err != nil {
			return fmt.Errorf("restart waffle service: %w", err)
		}
	}
	if err = m.advanceJournal(&journal, "activated"); err != nil {
		return err
	}
	if err = m.crashPoint("activated"); err != nil {
		return err
	}
	if candidate.Agent.DefaultModel != "" {
		if err = m.Health(ctx); err != nil {
			return fmt.Errorf("waffle health check: %w", err)
		}
		configBytes, readErr := os.ReadFile(m.ConfigPath)
		if readErr != nil {
			return readErr
		}
		if err = writeDurable(m.readyPath(), generationBytes(configBytes), 0o600); err != nil {
			return err
		}
	}
	if err = m.advanceJournal(&journal, "healthy"); err != nil {
		return err
	}
	if err = m.crashPoint("healthy"); err != nil {
		return err
	}
	return m.finalizeTransaction()
}

func transactionIDForStage(configStage string) (string, error) {
	contents, err := os.ReadFile(configStage)
	if err != nil {
		return "", err
	}
	return transactionIDForBytes(contents), nil
}

func transactionIDForBytes(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:16])
}

func readSnapshot(path string) ([]byte, fs.FileMode, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, false, err
	}
	return b, info.Mode().Perm(), true, nil
}

func writeStage(destination string, data []byte, mode fs.FileMode) (string, error) {
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".provider-stage-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	remove := true
	defer func() {
		_ = f.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	remove = false
	return path, nil
}

func writeBackups(m *Manager, before snapshot) error {
	for _, path := range []string{m.ConfigPath + ".bak", m.SecretsPath + ".bak", m.readyPath() + ".bak"} {
		if err := removeIfExists(path); err != nil {
			return err
		}
	}
	if before.configExist {
		if err := writeDurable(m.ConfigPath+".bak", before.configBytes, before.configMode); err != nil {
			return err
		}
	}
	if before.secretExist {
		if err := writeDurable(m.SecretsPath+".bak", before.secretBytes, before.secretMode); err != nil {
			return err
		}
	}
	if before.readyExist {
		if err := writeDurable(m.readyPath()+".bak", before.readyBytes, before.readyMode); err != nil {
			return err
		}
	}
	return nil
}

type transactionJournal struct {
	Phase               string               `json:"phase"`
	TransactionID       string               `json:"transaction_id,omitempty"`
	ConfigExisted       bool                 `json:"config_existed"`
	SecretExisted       bool                 `json:"secret_existed"`
	ReadyExisted        bool                 `json:"ready_existed"`
	ConfigMode          uint32               `json:"config_mode"`
	SecretMode          uint32               `json:"secret_mode"`
	ReadyMode           uint32               `json:"ready_mode"`
	ServiceActive       bool                 `json:"service_active"`
	SessionAliasChanges []SessionAliasChange `json:"session_alias_changes,omitempty"`
}

func (m *Manager) journalPath() string { return m.LockPath + ".transaction.json" }
func (m *Manager) readyPath() string   { return m.LockPath + ".ready-generation" }

func generationBytes(configBytes []byte) []byte {
	sum := sha256.Sum256(configBytes)
	return []byte(fmt.Sprintf("%x\n", sum[:]))
}

func (m *Manager) writeJournal(j transactionJournal) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeDurable(m.journalPath(), append(b, '\n'), 0o600)
}

func (m *Manager) advanceJournal(j *transactionJournal, phase string) error {
	j.Phase = phase
	return m.writeJournal(*j)
}

func (m *Manager) crashPoint(phase string) error {
	if m.CrashAfterPhase == nil {
		return nil
	}
	return m.CrashAfterPhase(phase)
}

func (m *Manager) recoverLocked(ctx context.Context) error {
	j, present, err := m.readJournal()
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if j.Phase == "awaiting_restart" {
		return ErrDeferredRestartPending
	}
	if j.Phase == "healthy" || j.Phase == "rolled_back" {
		return m.finalizeTransaction()
	}
	return m.rollbackJournal(ctx, &j)
}

func (m *Manager) readJournal() (transactionJournal, bool, error) {
	b, err := os.ReadFile(m.journalPath())
	if errors.Is(err, os.ErrNotExist) {
		return transactionJournal{}, false, nil
	}
	if err != nil {
		return transactionJournal{}, false, err
	}
	var j transactionJournal
	if err := json.Unmarshal(b, &j); err != nil {
		return transactionJournal{}, false, fmt.Errorf("parse provider transaction journal: %w", err)
	}
	return j, true, nil
}

func (m *Manager) rollbackJournal(ctx context.Context, j *transactionJournal) error {
	if j == nil {
		return errors.New("provider transaction journal is required")
	}
	if m.RestoreService == nil {
		return errors.New("cannot recover provider transaction: service restore callback is not configured")
	}
	restore := errors.Join(
		restoreFromBackup(m.SecretsPath, m.SecretsPath+".bak", fs.FileMode(j.SecretMode), j.SecretExisted),
		restoreFromBackup(m.ConfigPath, m.ConfigPath+".bak", fs.FileMode(j.ConfigMode), j.ConfigExisted),
		restoreFromBackup(m.readyPath(), m.readyPath()+".bak", fs.FileMode(j.ReadyMode), j.ReadyExisted),
	)
	if restore != nil {
		return restore
	}
	if err := m.RestoreService(ctx, j.ServiceActive); err != nil {
		return fmt.Errorf("restore previous service state: %w", err)
	}
	if len(j.SessionAliasChanges) > 0 {
		if m.SessionRecovery == nil {
			return errors.New("cannot recover provider transaction: session recovery handler is not configured")
		}
		if err := m.SessionRecovery(ctx, j.SessionAliasChanges); err != nil {
			return fmt.Errorf("restore session aliases: %w", err)
		}
	}
	if err := m.advanceJournal(j, "rolled_back"); err != nil {
		return err
	}
	return m.finalizeTransaction()
}

// VerifyReadiness re-proves readiness for the config on disk when a default
// model is configured but the ready generation is missing or stale, and reports
// whether it refreshed the marker.
//
// The marker records that the health probe passed for one exact config. Only
// provider transactions wrote it, so any out-of-band edit to config.toml left a
// healthy host reporting Installed with no route back except an unrelated
// provider mutation — and consumers of that state read Installed as "shut the
// service down". Proving readiness here is the same assertion the transaction
// path makes, at the one other moment the service is known to be running the
// config in question.
//
// An unproven outcome is not an error: a failing probe, an unresolvable default
// model, or a missing probe callback all leave the marker untouched so the state
// stays Installed.
// The lock is deliberately not held across the probe. Status reads such as
// Snapshot take the same lock, so holding it around a call that may wait out its
// own timeout would stall every Desk provider read for that long. The probe runs
// unlocked and the generation is only committed under the lock once the config it
// describes is confirmed unchanged.
func (m *Manager) VerifyReadiness(ctx context.Context) (bool, error) {
	configBytes, generation, needed, err := m.readinessProbeNeeded(ctx)
	if err != nil || !needed {
		return false, err
	}
	if m.Health == nil {
		return false, nil
	}
	if err := m.Health(ctx); err != nil {
		return false, nil
	}
	return m.commitProvenReadiness(ctx, configBytes, generation)
}

// readinessProbeNeeded reports whether a probe is worth running, returning the
// config it examined so the caller can confirm nothing changed underneath.
func (m *Manager) readinessProbeNeeded(ctx context.Context) (
	configBytes []byte, generation []byte, needed bool, err error,
) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()

	configBytes, err = os.ReadFile(m.ConfigPath)
	if err != nil {
		return nil, nil, false, err
	}
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		return nil, nil, false, err
	}
	if cfg.Agent.DefaultModel == "" {
		return nil, nil, false, nil
	}
	if _, resolveErr := cfg.ResolveModel(cfg.Agent.DefaultModel); resolveErr != nil {
		return nil, nil, false, nil
	}
	generation = generationBytes(configBytes)
	existing, readErr := os.ReadFile(m.readyPath())
	if readErr == nil && string(existing) == string(generation) {
		return nil, nil, false, nil
	}
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, nil, false, readErr
	}
	return configBytes, generation, true, nil
}

// commitProvenReadiness records a passed probe, but only while the config still
// hashes to what was probed. A concurrent edit or transaction between the probe
// and here means the result describes a configuration that is no longer on disk.
func (m *Manager) commitProvenReadiness(ctx context.Context, probed, generation []byte) (refreshed bool, err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()

	current, err := os.ReadFile(m.ConfigPath)
	if err != nil {
		return false, err
	}
	if string(current) != string(probed) {
		return false, nil
	}
	if err := writeDurable(m.readyPath(), generation, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// FinalizeDeferred confirms that a newly started process is healthy before
// removing the transaction journal and backups. A failed confirmation restores
// the exact previous files and service state.
func (m *Manager) FinalizeDeferred(ctx context.Context) (err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()

	journal, present, err := m.readJournal()
	if err != nil || !present {
		return err
	}
	switch journal.Phase {
	case "healthy", "rolled_back":
		return m.finalizeTransaction()
	case "awaiting_restart":
	default:
		return m.rollbackJournal(ctx, &journal)
	}

	if _, err := m.deferredConfigBytes(journal); err != nil {
		rollbackErr := m.rollbackJournal(ctx, &journal)
		return errors.Join(err, rollbackErr)
	}
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		rollbackErr := m.rollbackJournal(ctx, &journal)
		return errors.Join(err, rollbackErr)
	}
	if _, err := m.deferredConfigBytes(journal); err != nil {
		rollbackErr := m.rollbackJournal(ctx, &journal)
		return errors.Join(err, rollbackErr)
	}
	if cfg.Agent.DefaultModel != "" {
		if m.Health == nil || m.Health(ctx) != nil {
			rollbackErr := m.rollbackJournal(ctx, &journal)
			return errors.Join(ErrDeferredHealth, rollbackErr)
		}
		configBytes, err := m.deferredConfigBytes(journal)
		if err != nil {
			rollbackErr := m.rollbackJournal(ctx, &journal)
			return errors.Join(err, rollbackErr)
		}
		if err := writeDurable(m.readyPath(), generationBytes(configBytes), 0o600); err != nil {
			rollbackErr := m.rollbackJournal(ctx, &journal)
			return errors.Join(err, rollbackErr)
		}
	} else if err := removeIfExists(m.readyPath()); err != nil {
		rollbackErr := m.rollbackJournal(ctx, &journal)
		return errors.Join(err, rollbackErr)
	}
	if err := m.advanceJournal(&journal, "healthy"); err != nil {
		rollbackErr := m.rollbackJournal(ctx, &journal)
		return errors.Join(err, rollbackErr)
	}
	return m.finalizeTransaction()
}

func (m *Manager) deferredConfigBytes(journal transactionJournal) ([]byte, error) {
	configBytes, err := os.ReadFile(m.ConfigPath)
	if err != nil {
		return nil, errors.Join(ErrDeferredIntegrity, err)
	}
	if journal.TransactionID == "" || journal.TransactionID != transactionIDForBytes(configBytes) {
		return nil, ErrDeferredIntegrity
	}
	return configBytes, nil
}

func restoreFromBackup(destination, backup string, mode fs.FileMode, existed bool) error {
	if !existed {
		return removeIfExists(destination)
	}
	b, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("recover %s from backup: %w", destination, err)
	}
	return writeDurable(destination, b, mode)
}

func (m *Manager) finalizeTransaction() error {
	return errors.Join(
		removeIfExists(m.ConfigPath+".bak"),
		removeIfExists(m.SecretsPath+".bak"),
		removeIfExists(m.readyPath()+".bak"),
		removeIfExists(m.journalPath()),
	)
}

func writeDurable(path string, data []byte, mode fs.FileMode) error {
	stage, err := writeStage(path, data, mode)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(stage) }()
	return commitStage(stage, path, mode)
}

func commitStage(stage, destination string, mode fs.FileMode) error {
	if err := os.Chmod(stage, mode); err != nil {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	return syncDir(filepath.Dir(destination))
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		return syncDir(filepath.Dir(path))
	}
	return err
}
