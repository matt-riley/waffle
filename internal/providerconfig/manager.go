// Package providerconfig manages on-host provider enrollment as one
// transaction across config.toml, the encrypted secret store, and service
// activation.
package providerconfig

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"filippo.io/age"
	tomltree "github.com/pelletier/go-toml"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/instance"
	"github.com/matt-riley/waffle/internal/secret"
)

var (
	// ErrLocked means another provider mutation owns the host lock.
	ErrLocked = errors.New("provider configuration is locked")
	// ErrAliasConflict is wrapped by the stable legacy error text for a
	// requested model alias that already exists.
	ErrAliasConflict = errors.New("model alias")
	// ErrReferenced means a connection cannot be removed while model aliases
	// still point at it.
	ErrReferenced = errors.New("provider connection is referenced")
	// ErrSimulatedCrash is used only by deterministic recovery tests. The
	// transaction is intentionally left on disk exactly as a dead process would.
	ErrSimulatedCrash = errors.New("simulated provider transaction process death")
	// ErrDeferredRestartPending means committed provider state is waiting for a
	// new process to confirm health and finalize its recovery journal.
	ErrDeferredRestartPending = errors.New("provider configuration restart is pending")
	// ErrDeferredHealth means the new process could not confirm the deferred
	// provider transaction and the previous state was restored.
	ErrDeferredHealth = errors.New("deferred provider configuration health check failed")
	// ErrDeferredIntegrity means an awaiting-restart journal is not bound to
	// the exact config generation currently committed on disk.
	ErrDeferredIntegrity = errors.New("deferred provider transaction integrity check failed")
)

// Probe validates one concrete provider/model pair without mutating live state.
type Probe func(context.Context, config.ResolvedModel, string) error

// AddRequest is one atomic connection and model-catalog enrollment.
type AddRequest struct {
	ConnectionName string
	Connection     config.ProviderConnection
	Models         map[string]config.ModelTarget
	DefaultModel   string
	UtilityModel   string
	APIKey         string
	LegacyAuthFree bool
}

// AddModelRequest adds one model alias to an existing provider connection.
type AddModelRequest struct {
	ConnectionName string
	Alias          string
	UpstreamModel  string
	Default        bool
	Utility        bool
}

// CommitMode selects whether a provider transaction reconciles the current
// process immediately or leaves a durable journal for a new process.
type CommitMode int

const (
	CommitAndReconcile CommitMode = iota
	CommitForRestart
)

// MutationResult is the public, credential-free outcome of one provider
// mutation.
type MutationResult struct {
	RestartRequired bool   `json:"restart_required"`
	TransactionID   string `json:"transaction_id,omitempty"`
}

// CatalogSnapshot is the private provider catalogue input for one enrollment.
type CatalogSnapshot struct {
	Connection config.ProviderConnection
	APIKey     string
	ScopeID    string
}

// Preflight establishes identity, path, syntax, and recovery readiness before
// the CLI asks an operator to disclose a provider credential.
func (m *Manager) Preflight(ctx context.Context) (err error) {
	if m.Identity == nil {
		return errors.New("secret-store identity is required")
	}
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.ConfigPath), 0o700); err != nil {
		return err
	}
	raw, _, _, err := readSnapshot(m.ConfigPath)
	if err != nil {
		return err
	}
	if _, err := tomltree.LoadBytes(raw); err != nil {
		return fmt.Errorf("parse config syntax tree: %w", err)
	}
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		return err
	}
	return ensureCanonicalManagedSource(raw, cfg)
}

// Status is the stable deployment lifecycle shape consumed by host tooling.
type Status struct {
	State        string `json:"state"`
	DefaultModel string `json:"default_model,omitempty"`
}

// Listing contains no credential material and is safe to serialize.
type Listing struct {
	State        string                     `json:"state"`
	DefaultModel string                     `json:"default_model,omitempty"`
	UtilityModel string                     `json:"utility_model,omitempty"`
	Providers    map[string]ProviderSummary `json:"providers"`
	Models       map[string]ModelSummary    `json:"models"`
}

// ProviderSummary is the public, credential-free part of a connection.
type ProviderSummary struct {
	Type      string `json:"type"`
	BaseURL   string `json:"base_url,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

// ModelSummary is the stable JSON representation of a model alias.
type ModelSummary struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

// Manager's callbacks keep systemd and HTTP health outside the transaction
// engine, making every failure boundary deterministic in tests.
type Manager struct {
	ConfigPath    string
	SecretsPath   string
	LockPath      string
	Identity      *age.X25519Identity
	Random        io.Reader
	Probe         Probe
	Restart       func(context.Context) error
	Stop          func(context.Context) error
	Health        func(context.Context) error
	ServiceActive func(context.Context) (bool, error)
	// RestoreService restores the independently captured active/inactive state.
	RestoreService func(context.Context, bool) error
	// AfterCommit is an observation/fault-injection hook. It is invoked after
	// the named resource ("secret" then "config") is durably committed.
	AfterCommit     func(resource string) error
	CrashAfterPhase func(phase string) error
	// afterReadStatus is a deterministic concurrency-test seam.
	afterReadStatus func()
}

type commitModeContextKey struct{}

type commitModeState struct {
	mode   CommitMode
	result MutationResult
}

func runWithCommitMode(ctx context.Context, mode CommitMode, mutate func(context.Context) error) (MutationResult, error) {
	if mode != CommitAndReconcile && mode != CommitForRestart {
		return MutationResult{}, errors.New("invalid provider commit mode")
	}
	state := &commitModeState{mode: mode}
	if err := mutate(context.WithValue(ctx, commitModeContextKey{}, state)); err != nil {
		return MutationResult{}, err
	}
	return state.result, nil
}

func commitState(ctx context.Context) *commitModeState {
	state, _ := ctx.Value(commitModeContextKey{}).(*commitModeState)
	if state == nil {
		return &commitModeState{mode: CommitAndReconcile}
	}
	return state
}

// New returns a manager rooted in WAFFLE_HOME using the supplied identity.
func New(identity *age.X25519Identity) (*Manager, error) {
	configPath, err := config.Path()
	if err != nil {
		return nil, err
	}
	secretsPath, err := config.SecretsPath()
	if err != nil {
		return nil, err
	}
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	return &Manager{
		ConfigPath:  configPath,
		SecretsPath: secretsPath,
		LockPath:    filepath.Join(home, "provider-config.lock"),
		Identity:    identity,
		Random:      rand.Reader,
	}, nil
}

// Add validates and enrolls a connection and one or more aliases.
func (m *Manager) Add(ctx context.Context, req AddRequest) error {
	_, err := m.AddWithMode(ctx, req, CommitAndReconcile)
	return err
}

// AddWithMode validates and enrolls a connection using the requested commit
// lifecycle.
func (m *Manager) AddWithMode(ctx context.Context, req AddRequest, mode CommitMode) (MutationResult, error) {
	return runWithCommitMode(ctx, mode, func(modeCtx context.Context) error {
		return m.add(modeCtx, req)
	})
}

func (m *Manager) add(ctx context.Context, req AddRequest) (err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}

	if err := validateAdd(req); err != nil {
		return err
	}
	before, err := m.capture(ctx)
	if err != nil {
		return err
	}
	if err := ensureCanonicalManagedSource(before.configBytes, before.cfg); err != nil {
		return err
	}
	if _, exists := before.cfg.Providers[req.ConnectionName]; exists {
		return fmt.Errorf("provider connection %q already exists", req.ConnectionName)
	}
	for alias := range req.Models {
		if _, exists := before.cfg.Models[alias]; exists {
			return fmt.Errorf("%w %q already exists", ErrAliasConflict, alias)
		}
	}
	scopeID, err := m.newCatalogScope()
	if err != nil {
		return redactError(err, req.APIKey)
	}
	legacyKey, err := m.legacyCredential(before.cfg, req.LegacyAuthFree)
	if err != nil {
		return redactError(err, req.APIKey)
	}

	connection := req.Connection
	if req.APIKey != "" {
		connection.APIKey = secretRef(req.ConnectionName)
	} else {
		connection.APIKey = ""
	}
	configStage, candidate, err := m.stageConfig(before.configBytes, func(doc *tomlDocument, cfg config.Config) error {
		migrateLegacyDocument(doc, cfg, legacyKey != "")
		setConnection(doc, req.ConnectionName, connection)
		for alias, target := range req.Models {
			target.Provider = req.ConnectionName
			setModel(doc, alias, target)
		}
		if req.DefaultModel != "" {
			doc.setValue("agent", "default_model", strconv.Quote(req.DefaultModel))
		}
		if req.UtilityModel != "" {
			doc.setValue("agent", "utility_model", strconv.Quote(req.UtilityModel))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := validateAddCandidate(before.cfg, candidate, req, connection); err != nil {
		_ = os.Remove(configStage)
		return err
	}
	defer func() { _ = os.Remove(configStage) }()

	secretStage, err := m.stageSecrets(before.secretBytes, func(store secret.Store) error {
		if legacyKey != "" {
			if err := store.Set(secretName("default"), legacyKey); err != nil {
				return err
			}
		}
		if req.APIKey != "" {
			if err := store.Set(secretName(req.ConnectionName), req.APIKey); err != nil {
				return err
			}
		}
		return store.Set(catalogScopeName(req.ConnectionName), scopeID)
	})
	if err != nil {
		return redactError(err, req.APIKey)
	}
	defer func() { _ = os.Remove(secretStage) }()

	if m.Probe == nil {
		return errors.New("provider probe is not configured")
	}
	aliases := SortedKeys(req.Models)
	for _, alias := range aliases {
		target, resolveErr := candidate.ResolveModel(alias)
		if resolveErr != nil {
			return resolveErr
		}
		if probeErr := m.Probe(ctx, target, req.APIKey); probeErr != nil {
			return redactError(fmt.Errorf("probe model alias %q: %w", alias, probeErr), req.APIKey)
		}
	}

	return m.commit(ctx, before, configStage, secretStage, candidate, req.APIKey)
}

// Remove deletes an unreferenced connection and its scoped credential.
func (m *Manager) Remove(ctx context.Context, name string) (err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}
	if !config.ValidProviderConnectionName(name) {
		return fmt.Errorf("invalid connection name %q", name)
	}
	before, err := m.capture(ctx)
	if err != nil {
		return err
	}
	if err := ensureCanonicalManagedSource(before.configBytes, before.cfg); err != nil {
		return err
	}
	if _, ok := before.cfg.Providers[name]; !ok {
		return fmt.Errorf("provider connection %q does not exist", name)
	}
	refs := referencedAliases(before.cfg, name)
	if len(refs) > 0 {
		return fmt.Errorf("%w: %q is used by model aliases %s", ErrReferenced, name, strings.Join(refs, ", "))
	}
	configStage, candidate, err := m.stageConfig(before.configBytes, func(doc *tomlDocument, cfg config.Config) error {
		migrateLegacyDocument(doc, cfg, false)
		doc.deleteTable("providers." + name)
		return nil
	})
	if err != nil {
		return err
	}
	if _, stillPresent := candidate.Providers[name]; stillPresent {
		_ = os.Remove(configStage)
		return fmt.Errorf("semantic provider removal failed for %q; refusing secret mutation", name)
	}
	if err := validateRemoveCandidate(before.cfg, candidate, name); err != nil {
		_ = os.Remove(configStage)
		return err
	}
	defer func() { _ = os.Remove(configStage) }()
	secretStage, err := m.stageSecrets(before.secretBytes, func(store secret.Store) error {
		return errors.Join(
			deleteSecretIfPresent(store, secretName(name)),
			deleteSecretIfPresent(store, catalogScopeName(name)),
		)
	})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(secretStage) }()
	return m.commit(ctx, before, configStage, secretStage, candidate, "")
}

// Test probes the first alias for a named connection using its encrypted key.
func (m *Manager) Test(ctx context.Context, name string) error {
	if m.Probe == nil {
		return errors.New("provider probe is not configured")
	}
	target, key, err := m.testSnapshot(ctx, name)
	if err != nil {
		return err
	}
	if err := m.Probe(ctx, target, key); err != nil {
		return redactError(fmt.Errorf("probe provider %q model alias %q: %w", name, target.Alias, err), key)
	}
	return nil
}

// CatalogSnapshot returns the private inputs needed to discover one
// connection's catalogue. Legacy enrollments receive a scope on first access.
func (m *Manager) CatalogSnapshot(ctx context.Context, name string) (snapshot CatalogSnapshot, err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return CatalogSnapshot{}, err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return CatalogSnapshot{}, err
	}
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		return CatalogSnapshot{}, err
	}
	connection, ok := cfg.Providers[name]
	if !ok {
		return CatalogSnapshot{}, fmt.Errorf("provider connection %q does not exist", name)
	}
	key, err := m.connectionKey(connection)
	if err != nil {
		return CatalogSnapshot{}, err
	}
	if err := validateNoActiveKeyInDurableStrings(key, connection.Type, connection.APIKey, connection.BaseURL); err != nil {
		return CatalogSnapshot{}, err
	}
	if m.Identity == nil {
		return CatalogSnapshot{}, errors.New("catalogue access: secret-store identity is required")
	}
	store := secret.OpenFile(m.SecretsPath, m.Identity)
	scopeID, err := store.Get(catalogScopeName(name))
	if errors.Is(err, secret.ErrNotFound) {
		scopeID, err = m.newCatalogScope()
		if err != nil {
			return CatalogSnapshot{}, redactError(err, key)
		}
		if err := store.Set(catalogScopeName(name), scopeID); err != nil {
			return CatalogSnapshot{}, redactError(fmt.Errorf("catalogue access: persist enrollment scope: %w", err), key)
		}
	} else if err != nil {
		return CatalogSnapshot{}, redactError(fmt.Errorf("catalogue access: load enrollment scope: %w", err), key)
	}
	return CatalogSnapshot{Connection: connection, APIKey: key, ScopeID: scopeID}, nil
}

// AddModel adds one favourite alias and optional agent roles transactionally.
func (m *Manager) AddModel(ctx context.Context, req AddModelRequest) error {
	_, err := m.AddModelWithMode(ctx, req, CommitAndReconcile)
	return err
}

// AddModelWithMode adds one alias using the requested commit lifecycle.
func (m *Manager) AddModelWithMode(ctx context.Context, req AddModelRequest, mode CommitMode) (MutationResult, error) {
	return runWithCommitMode(ctx, mode, func(modeCtx context.Context) error {
		return m.addModel(modeCtx, req)
	})
}

func (m *Manager) addModel(ctx context.Context, req AddModelRequest) (err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}
	if !config.ValidProviderConnectionName(req.ConnectionName) {
		return fmt.Errorf("invalid connection name %q", req.ConnectionName)
	}
	before, err := m.capture(ctx)
	if err != nil {
		return err
	}
	if err := ensureCanonicalManagedSource(before.configBytes, before.cfg); err != nil {
		return err
	}
	connection, ok := before.cfg.Providers[req.ConnectionName]
	if !ok {
		return fmt.Errorf("provider connection %q does not exist", req.ConnectionName)
	}
	key, err := m.connectionKey(connection)
	if err != nil {
		return err
	}
	if err := validateNoActiveKeyInDurableStrings(key, req.ConnectionName, req.Alias, req.UpstreamModel); err != nil {
		return err
	}
	if err := validateAddModel(req); err != nil {
		return err
	}
	if _, ok := before.cfg.Models[req.Alias]; ok {
		return fmt.Errorf("%w %q already exists", ErrAliasConflict, req.Alias)
	}

	target := config.ModelTarget{Provider: req.ConnectionName, Model: req.UpstreamModel}
	configStage, candidate, err := m.stageConfig(before.configBytes, func(doc *tomlDocument, _ config.Config) error {
		setModel(doc, req.Alias, target)
		if req.Default {
			doc.setValue("agent", "default_model", strconv.Quote(req.Alias))
		}
		if req.Utility {
			doc.setValue("agent", "utility_model", strconv.Quote(req.Alias))
		}
		return nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(configStage) }()
	if err := validateAddModelCandidate(before.cfg, candidate, req, target); err != nil {
		return err
	}

	secretStage := ""
	if before.secretExist {
		secretStage, err = m.stageSecrets(before.secretBytes, func(secret.Store) error { return nil })
		if err != nil {
			return redactError(err, key)
		}
	}
	defer func() {
		if secretStage != "" {
			_ = os.Remove(secretStage)
		}
	}()
	resolved, err := candidate.ResolveModel(req.Alias)
	if err != nil {
		return err
	}
	if m.Probe == nil {
		return errors.New("provider probe is not configured")
	}
	if err := m.Probe(ctx, resolved, key); err != nil {
		return redactError(fmt.Errorf("probe model alias %q: %w", req.Alias, err), key)
	}
	return m.commit(ctx, before, configStage, secretStage, candidate, key)
}

// ActivateModel validates an existing alias, makes it the default, and moves
// an Installed host to Ready transactionally.
func (m *Manager) ActivateModel(ctx context.Context, alias string) error {
	_, err := m.ActivateModelWithMode(ctx, alias, CommitAndReconcile)
	return err
}

// ActivateModelWithMode sets the default role using the requested commit
// lifecycle.
func (m *Manager) ActivateModelWithMode(ctx context.Context, alias string, mode CommitMode) (MutationResult, error) {
	return runWithCommitMode(ctx, mode, func(modeCtx context.Context) error {
		return m.activateModel(modeCtx, alias, false)
	})
}

// ActivateUtilityModel validates an existing alias and makes it the utility
// model transactionally.
func (m *Manager) ActivateUtilityModel(ctx context.Context, alias string) error {
	_, err := m.ActivateUtilityModelWithMode(ctx, alias, CommitAndReconcile)
	return err
}

// ActivateUtilityModelWithMode sets the utility role using the requested
// commit lifecycle.
func (m *Manager) ActivateUtilityModelWithMode(ctx context.Context, alias string, mode CommitMode) (MutationResult, error) {
	return runWithCommitMode(ctx, mode, func(modeCtx context.Context) error {
		return m.activateModel(modeCtx, alias, true)
	})
}

func (m *Manager) activateModel(ctx context.Context, alias string, utility bool) (err error) {
	if !config.ValidModelAlias(alias) {
		return fmt.Errorf("invalid model alias %q", alias)
	}
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}
	before, err := m.capture(ctx)
	if err != nil {
		return err
	}
	target, err := before.cfg.ResolveModel(alias)
	if err != nil {
		return err
	}
	stage, candidate, err := m.stageConfig(before.configBytes, func(doc *tomlDocument, _ config.Config) error {
		field := "default_model"
		if utility {
			field = "utility_model"
		}
		doc.setValue("agent", field, strconv.Quote(alias))
		return nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(stage) }()
	expectedAgent := before.cfg.Agent
	if utility {
		if candidate.Agent.UtilityModel != alias {
			return errors.New("semantic utility-model activation failed")
		}
		expectedAgent.UtilityModel = alias
	} else {
		if candidate.Agent.DefaultModel != alias {
			return errors.New("semantic default-model activation failed")
		}
		expectedAgent.DefaultModel = alias
	}
	if !reflect.DeepEqual(candidate.Providers, before.cfg.Providers) ||
		!reflect.DeepEqual(candidate.Models, before.cfg.Models) ||
		!reflect.DeepEqual(candidate.Agent, expectedAgent) {
		return errors.New("unrelated configuration changed during model activation")
	}
	secretStage, err := m.stageSecrets(before.secretBytes, func(secret.Store) error { return nil })
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(secretStage) }()
	key, err := m.connectionKey(target.Connection)
	if err != nil {
		return err
	}
	if m.Probe == nil {
		return errors.New("provider probe is not configured")
	}
	if err := m.Probe(ctx, target, key); err != nil {
		return redactError(fmt.Errorf("probe model alias %q: %w", alias, err), key)
	}
	return m.commit(ctx, before, stage, secretStage, candidate, key)
}

// RemoveModel deletes an alias. replacement reassigns default, utility, and
// profile references; without it, default/utility references are cleared and
// profile references fail closed.
func (m *Manager) RemoveModel(ctx context.Context, alias, replacement string) (err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}
	before, err := m.capture(ctx)
	if err != nil {
		return err
	}
	if _, ok := before.cfg.Models[alias]; !ok {
		return fmt.Errorf("model alias %q does not exist", alias)
	}
	if replacement == alias {
		return errors.New("replacement alias must differ from removed alias")
	}
	if replacement != "" {
		if _, ok := before.cfg.Models[replacement]; !ok {
			return fmt.Errorf("replacement model alias %q does not exist", replacement)
		}
	}
	for name, profile := range before.cfg.Agent.Profiles {
		if profile.Model == alias && replacement == "" {
			return fmt.Errorf("model alias %q is referenced by agent profile %q; use --replace-with", alias, name)
		}
	}
	stage, candidate, err := m.stageConfig(before.configBytes, func(doc *tomlDocument, cfg config.Config) error {
		doc.deleteTable("models." + alias)
		if cfg.Agent.DefaultModel == alias {
			if replacement == "" {
				doc.deleteValue("agent", "default_model")
			} else {
				doc.setValue("agent", "default_model", strconv.Quote(replacement))
			}
		}
		if cfg.Agent.UtilityModel == alias {
			if replacement == "" {
				doc.deleteValue("agent", "utility_model")
			} else {
				doc.setValue("agent", "utility_model", strconv.Quote(replacement))
			}
		}
		for name, profile := range cfg.Agent.Profiles {
			if profile.Model != alias {
				continue
			}
			table := "agent.profile." + name
			if _, _, ok := doc.tableSpan(table); !ok {
				return fmt.Errorf("agent profile %q must use canonical [%s] table form before reassignment", name, table)
			}
			doc.setValue(table, "model", strconv.Quote(replacement))
		}
		return nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(stage) }()
	if _, ok := candidate.Models[alias]; ok {
		return errors.New("semantic model removal failed")
	}
	if before.cfg.Agent.DefaultModel == alias && candidate.Agent.DefaultModel != replacement {
		return errors.New("semantic default-model reassignment failed")
	}
	expectedModels := make(map[string]config.ModelTarget, len(before.cfg.Models)-1)
	for name, target := range before.cfg.Models {
		if name != alias {
			expectedModels[name] = target
		}
	}
	expectedAgent := before.cfg.Agent
	if expectedAgent.DefaultModel == alias {
		expectedAgent.DefaultModel = replacement
	}
	if expectedAgent.UtilityModel == alias {
		expectedAgent.UtilityModel = replacement
	}
	if replacement != "" && len(expectedAgent.Profiles) > 0 {
		profiles := make(map[string]config.AgentProfile, len(expectedAgent.Profiles))
		for name, profile := range expectedAgent.Profiles {
			if profile.Model == alias {
				profile.Model = replacement
			}
			profiles[name] = profile
		}
		expectedAgent.Profiles = profiles
	}
	if !equalMaps(candidate.Providers, before.cfg.Providers) ||
		!equalMaps(candidate.Models, expectedModels) ||
		!reflect.DeepEqual(candidate.Agent, expectedAgent) {
		return errors.New("unrelated configuration changed during model removal")
	}
	secretStage, err := m.stageSecrets(before.secretBytes, func(secret.Store) error { return nil })
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(secretStage) }()
	if replacement != "" && candidate.Agent.DefaultModel == replacement {
		target, resolveErr := candidate.ResolveModel(replacement)
		if resolveErr != nil {
			return resolveErr
		}
		key, keyErr := m.connectionKey(target.Connection)
		if keyErr != nil {
			return keyErr
		}
		if m.Probe == nil {
			return errors.New("provider probe is not configured")
		}
		if probeErr := m.Probe(ctx, target, key); probeErr != nil {
			return redactError(probeErr, key)
		}
	}
	return m.commit(ctx, before, stage, secretStage, candidate, "")
}

func equalMaps[V comparable](a, b map[string]V) bool {
	if len(a) != len(b) {
		return false
	}
	for name, value := range a {
		if other, ok := b[name]; !ok || other != value {
			return false
		}
	}
	return true
}

// Status reports Installed unless a configured default alias is healthy.
func (m *Manager) Status(ctx context.Context) (status Status, err error) {
	_, status, err = m.lockedConfigStatus(ctx, false)
	return status, err
}

func (m *Manager) lockedConfigStatus(ctx context.Context, listHook bool) (cfg config.Config, status Status, err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return config.Config{}, Status{}, err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return config.Config{}, Status{}, err
	}
	cfg, err = config.Load(m.ConfigPath)
	if err != nil {
		return config.Config{}, Status{}, err
	}
	status, err = m.statusFromConfig(ctx, cfg)
	if err != nil {
		return config.Config{}, Status{}, err
	}
	if listHook && m.afterReadStatus != nil {
		m.afterReadStatus()
	}
	return cfg, status, nil
}

// Snapshot returns typed, credential-free provider state.
func (m *Manager) Snapshot(ctx context.Context) (Listing, error) {
	cfg, status, err := m.lockedConfigStatus(ctx, true)
	if err != nil {
		return Listing{}, err
	}
	providers := make(map[string]ProviderSummary, len(cfg.Providers))
	for name, connection := range cfg.Providers {
		providers[name] = ProviderSummary{Type: connection.Type, BaseURL: connection.BaseURL, MaxTokens: connection.MaxTokens}
	}
	models := make(map[string]ModelSummary, len(cfg.Models))
	for alias, target := range cfg.Models {
		models[alias] = ModelSummary{Provider: target.Provider, Model: target.Model, MaxTokens: target.MaxTokens}
	}
	return Listing{
		State:        status.State,
		DefaultModel: status.DefaultModel,
		UtilityModel: cfg.Agent.UtilityModel,
		Providers:    providers,
		Models:       models,
	}, nil
}

// List returns a deterministic, credential-free JSON document.
func (m *Manager) List(ctx context.Context) ([]byte, error) {
	listing, err := m.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(listing, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (m *Manager) testSnapshot(ctx context.Context, name string) (target config.ResolvedModel, key string, err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return config.ResolvedModel{}, "", err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return config.ResolvedModel{}, "", err
	}
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		return config.ResolvedModel{}, "", err
	}
	aliases := referencedAliases(cfg, name)
	if len(aliases) == 0 {
		if _, ok := cfg.Providers[name]; !ok {
			return config.ResolvedModel{}, "", fmt.Errorf("provider connection %q does not exist", name)
		}
		return config.ResolvedModel{}, "", fmt.Errorf("provider connection %q has no model aliases to test", name)
	}
	target, err = cfg.ResolveModel(aliases[0])
	if err != nil {
		return config.ResolvedModel{}, "", err
	}
	key, err = m.connectionKey(target.Connection)
	return target, key, err
}

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
	state := commitState(ctx)
	transactionID, err := transactionIDForStage(configStage)
	if err != nil {
		return err
	}
	journal := transactionJournal{
		Phase: "prepared", ConfigExisted: before.configExist, SecretExisted: before.secretExist,
		ReadyExisted: before.readyExist, ConfigMode: uint32(before.configMode),
		SecretMode: uint32(before.secretMode), ReadyMode: uint32(before.readyMode),
		ServiceActive: before.serviceActive, TransactionID: transactionID,
	}
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

func (m *Manager) statusFromConfig(ctx context.Context, cfg config.Config) (Status, error) {
	status := Status{State: "installed", DefaultModel: cfg.Agent.DefaultModel}
	if cfg.Agent.DefaultModel == "" {
		return status, nil
	}
	if _, err := cfg.ResolveModel(cfg.Agent.DefaultModel); err != nil {
		return Status{}, err
	}
	if m.ServiceActive == nil {
		return Status{}, errors.New("service active-state callback is not configured")
	}
	active, err := m.ServiceActive(ctx)
	if err != nil || !active {
		return status, err
	}
	configBytes, err := os.ReadFile(m.ConfigPath)
	if err != nil {
		return Status{}, err
	}
	readyBytes, err := os.ReadFile(m.readyPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status, nil
		}
		return Status{}, err
	}
	if string(readyBytes) == string(generationBytes(configBytes)) && m.Health != nil && m.Health(ctx) == nil {
		status.State = "ready"
	}
	return status, nil
}

func (m *Manager) connectionKey(connection config.ProviderConnection) (string, error) {
	if connection.APIKey == "" {
		return "", nil
	}
	if !strings.HasPrefix(connection.APIKey, "secret://provider/") {
		return "", errors.New("provider connection has an invalid credential reference")
	}
	if m.Identity == nil {
		return "", errors.New("secret-store identity is required")
	}
	return secret.Resolve(secret.OpenFile(m.SecretsPath, m.Identity), connection.APIKey)
}

func validateAdd(req AddRequest) error {
	durable := []string{
		req.ConnectionName,
		req.Connection.Type,
		req.Connection.APIKey,
		req.Connection.BaseURL,
		req.DefaultModel,
		req.UtilityModel,
	}
	for alias, target := range req.Models {
		durable = append(durable, alias, target.Provider, target.Model)
	}
	if err := validateNoActiveKeyInDurableStrings(req.APIKey, durable...); err != nil {
		return err
	}
	if !config.ValidProviderConnectionName(req.ConnectionName) {
		return fmt.Errorf("invalid connection name %q (want slug [a-z0-9-] max %d)", req.ConnectionName, config.ProviderConnectionNameMax)
	}
	if len(req.Models) == 0 {
		return errors.New("at least one model alias is required")
	}
	if req.Connection.APIKey != "" {
		return errors.New("connection api_key must not contain credential material")
	}
	for alias, target := range req.Models {
		if !config.ValidModelAlias(alias) {
			return fmt.Errorf("invalid model alias %q", alias)
		}
		if strings.TrimSpace(target.Model) == "" {
			return fmt.Errorf("model alias %q: upstream model is required", alias)
		}
		if target.Provider != "" && target.Provider != req.ConnectionName {
			return fmt.Errorf("model alias %q targets connection %q, want %q", alias, target.Provider, req.ConnectionName)
		}
	}
	for label, alias := range map[string]string{"default": req.DefaultModel, "utility": req.UtilityModel} {
		if alias != "" {
			if _, ok := req.Models[alias]; !ok {
				return fmt.Errorf("%s model alias %q is not part of this enrollment", label, alias)
			}
		}
	}
	return nil
}

func validateNoActiveKeyInDurableStrings(apiKey string, values ...string) error {
	if apiKey == "" {
		return nil
	}
	for _, value := range values {
		if strings.Contains(value, apiKey) {
			return errors.New("durable provider configuration contains the active API key")
		}
	}
	return nil
}

func validateAddModel(req AddModelRequest) error {
	if !config.ValidProviderConnectionName(req.ConnectionName) {
		return fmt.Errorf("invalid connection name %q", req.ConnectionName)
	}
	if !config.ValidModelAlias(req.Alias) {
		return fmt.Errorf("invalid model alias %q", req.Alias)
	}
	if strings.TrimSpace(req.UpstreamModel) == "" {
		return fmt.Errorf("model alias %q: upstream model is required", req.Alias)
	}
	return nil
}

func validateAddModelCandidate(before, candidate config.Config, req AddModelRequest, target config.ModelTarget) error {
	if got, ok := candidate.Models[req.Alias]; !ok || got != target {
		return fmt.Errorf("semantic model addition failed for %q", req.Alias)
	}
	expectedModels := make(map[string]config.ModelTarget, len(before.Models)+1)
	for alias, existing := range before.Models {
		expectedModels[alias] = existing
	}
	expectedModels[req.Alias] = target
	expectedAgent := before.Agent
	if req.Default {
		expectedAgent.DefaultModel = req.Alias
	}
	if req.Utility {
		expectedAgent.UtilityModel = req.Alias
	}
	if !equalMaps(candidate.Providers, before.Providers) ||
		!equalMaps(candidate.Models, expectedModels) ||
		!reflect.DeepEqual(candidate.Agent, expectedAgent) {
		return errors.New("unrelated provider, model, or agent configuration changed during model addition")
	}
	return nil
}

func migrateLegacyDocument(doc *tomlDocument, cfg config.Config, credentialMigrated bool) {
	if cfg.ProviderRegistrySource() != config.ProviderRegistryLegacy {
		return
	}
	doc.deleteTable("provider")
	for name, connection := range cfg.Providers {
		if name == "default" {
			if credentialMigrated {
				connection.APIKey = secretRef(name)
			} else {
				connection.APIKey = ""
			}
		}
		setConnection(doc, name, connection)
	}
	for alias, target := range cfg.Models {
		setModel(doc, alias, target)
	}
	if cfg.Agent.DefaultModel != "" {
		doc.setValue("agent", "default_model", strconv.Quote(cfg.Agent.DefaultModel))
	}
	if cfg.Agent.UtilityModel != "" {
		doc.setValue("agent", "utility_model", strconv.Quote(cfg.Agent.UtilityModel))
	}
}

func (m *Manager) legacyCredential(cfg config.Config, explicitAuthFree bool) (string, error) {
	if cfg.ProviderRegistrySource() != config.ProviderRegistryLegacy {
		return "", nil
	}
	connection := cfg.Providers["default"]
	if connection.APIKey == "" {
		env := ""
		switch connection.Type {
		case "anthropic":
			env = "ANTHROPIC_API_KEY"
		case "openai":
			env = "OPENAI_API_KEY"
		}
		if value := os.Getenv(env); value != "" {
			return value, nil
		}
		if explicitAuthFree {
			return "", nil
		}
		return "", fmt.Errorf("legacy provider credential is not visible to this management process; preserve it in [provider].api_key or a secret reference, or explicitly confirm auth-free intent")
	}
	if !secret.IsRef(connection.APIKey) {
		return connection.APIKey, nil
	}
	if m.Identity == nil {
		return "", errors.New("secret-store identity is required to migrate the legacy provider credential")
	}
	value, err := secret.Resolve(secret.OpenFile(m.SecretsPath, m.Identity), connection.APIKey)
	if err != nil {
		return "", fmt.Errorf("resolve legacy provider credential: %w", err)
	}
	return value, nil
}

func ensureCanonicalManagedSource(raw []byte, cfg config.Config) error {
	text := string(raw)
	inlineManaged := regexp.MustCompile(`(?m)^\s*(provider|providers|models|agent)\s*=\s*\{`)
	if inlineManaged.MatchString(text) {
		return errors.New("managed provider TOML must use canonical table form; inline tables cannot be safely rewritten")
	}
	doc := newTOMLDocument(raw)
	if cfg.ProviderRegistrySource() == config.ProviderRegistryLegacy {
		if _, _, ok := doc.tableSpan("provider"); !ok {
			return errors.New("legacy provider TOML must use canonical [provider] table form before management")
		}
	}
	for name := range cfg.Providers {
		if cfg.ProviderRegistrySource() == config.ProviderRegistryLegacy {
			break
		}
		if _, _, ok := doc.tableSpan("providers." + name); !ok {
			return fmt.Errorf("provider %q must use canonical [providers.%s] table form before management", name, name)
		}
	}
	for alias := range cfg.Models {
		if cfg.ProviderRegistrySource() == config.ProviderRegistryLegacy {
			break
		}
		if _, _, ok := doc.tableSpan("models." + alias); !ok {
			return fmt.Errorf("model alias %q must use canonical [models.%s] table form before management", alias, alias)
		}
	}
	return nil
}

func validateAddCandidate(before, candidate config.Config, req AddRequest, connection config.ProviderConnection) error {
	got, ok := candidate.Providers[req.ConnectionName]
	if !ok || got != connection {
		return fmt.Errorf("semantic provider addition failed for %q; refusing secret mutation", req.ConnectionName)
	}
	for alias, requested := range req.Models {
		requested.Provider = req.ConnectionName
		if gotTarget, ok := candidate.Models[alias]; !ok || gotTarget != requested {
			return fmt.Errorf("semantic model addition failed for %q; refusing secret mutation", alias)
		}
	}
	if req.DefaultModel != "" && candidate.Agent.DefaultModel != req.DefaultModel {
		return errors.New("semantic default-model update failed; refusing secret mutation")
	}
	for name, old := range before.Providers {
		if before.ProviderRegistrySource() == config.ProviderRegistryLegacy {
			continue
		}
		if gotOld, ok := candidate.Providers[name]; !ok || gotOld != old {
			return fmt.Errorf("unrelated provider %q changed during mutation", name)
		}
	}
	if before.ProviderRegistrySource() != config.ProviderRegistryLegacy {
		for alias, old := range before.Models {
			if gotOld, ok := candidate.Models[alias]; !ok || gotOld != old {
				return fmt.Errorf("unrelated model alias %q changed during mutation", alias)
			}
		}
	}
	expectedAgent := before.Agent
	if req.DefaultModel != "" {
		expectedAgent.DefaultModel = req.DefaultModel
	}
	if req.UtilityModel != "" {
		expectedAgent.UtilityModel = req.UtilityModel
	}
	if before.ProviderRegistrySource() != config.ProviderRegistryLegacy && !reflect.DeepEqual(candidate.Agent, expectedAgent) {
		return errors.New("unrelated agent configuration changed during provider addition")
	}
	return nil
}

func validateRemoveCandidate(before, candidate config.Config, removed string) error {
	for name, old := range before.Providers {
		if name == removed {
			continue
		}
		if got, ok := candidate.Providers[name]; !ok || got != old {
			return fmt.Errorf("unrelated provider %q changed during removal", name)
		}
	}
	if !reflect.DeepEqual(before.Models, candidate.Models) || !reflect.DeepEqual(before.Agent, candidate.Agent) {
		return errors.New("unrelated models or agent configuration changed during provider removal")
	}
	return nil
}

func setConnection(doc *tomlDocument, name string, connection config.ProviderConnection) {
	table := "providers." + name
	doc.setValue(table, "type", strconv.Quote(connection.Type))
	doc.setOptional(table, "api_key", connection.APIKey)
	doc.setOptional(table, "base_url", connection.BaseURL)
	doc.setOptionalInt(table, "max_tokens", connection.MaxTokens)
}

func setModel(doc *tomlDocument, alias string, target config.ModelTarget) {
	table := "models." + alias
	doc.setValue(table, "provider", strconv.Quote(target.Provider))
	doc.setValue(table, "model", strconv.Quote(target.Model))
	doc.setOptionalInt(table, "max_tokens", target.MaxTokens)
}

var (
	tableHeaderRE    = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*(?:#.*)?$`)
	anyTableHeaderRE = regexp.MustCompile(`^\s*\[\[?[^\]]+\]\]?\s*(?:#.*)?$`)
)

// tomlDocument is a narrow, syntax-aware editor for operator-owned TOML. It
// preserves all lines outside the exact managed keys and tables.
type tomlDocument struct{ lines []string }

func newTOMLDocument(raw []byte) *tomlDocument {
	text := string(raw)
	if text == "" {
		return &tomlDocument{}
	}
	return &tomlDocument{lines: strings.Split(strings.TrimSuffix(text, "\n"), "\n")}
}

func (d *tomlDocument) bytes() []byte {
	if len(d.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(d.lines, "\n") + "\n")
}

func (d *tomlDocument) tableSpan(table string) (start, end int, ok bool) {
	for i, line := range d.lines {
		match := tableHeaderRE.FindStringSubmatch(line)
		if match == nil || strings.TrimSpace(match[1]) != table {
			continue
		}
		end = len(d.lines)
		for j := i + 1; j < len(d.lines); j++ {
			if anyTableHeaderRE.MatchString(d.lines[j]) {
				end = j
				break
			}
		}
		return i, end, true
	}
	return 0, 0, false
}

func (d *tomlDocument) ensureTable(table string) (start, end int) {
	if start, end, ok := d.tableSpan(table); ok {
		return start, end
	}
	if len(d.lines) > 0 && strings.TrimSpace(d.lines[len(d.lines)-1]) != "" {
		d.lines = append(d.lines, "")
	}
	d.lines = append(d.lines, "["+table+"]")
	return len(d.lines) - 1, len(d.lines)
}

func (d *tomlDocument) setValue(table, key, rendered string) {
	start, end := d.ensureTable(table)
	keyRE := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	for i := start + 1; i < end; i++ {
		if !keyRE.MatchString(d.lines[i]) {
			continue
		}
		comment := inlineComment(d.lines[i])
		d.lines[i] = key + " = " + rendered + comment
		return
	}
	d.lines = append(d.lines[:end], append([]string{key + " = " + rendered}, d.lines[end:]...)...)
}

func (d *tomlDocument) deleteValue(table, key string) {
	start, end, ok := d.tableSpan(table)
	if !ok {
		return
	}
	keyRE := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	for i := start + 1; i < end; i++ {
		if keyRE.MatchString(d.lines[i]) {
			d.lines = append(d.lines[:i], d.lines[i+1:]...)
			return
		}
	}
}

func (d *tomlDocument) setOptional(table, key, value string) {
	if value == "" {
		d.deleteValue(table, key)
		return
	}
	d.setValue(table, key, strconv.Quote(value))
}

func (d *tomlDocument) setOptionalInt(table, key string, value int) {
	if value == 0 {
		d.deleteValue(table, key)
		return
	}
	d.setValue(table, key, strconv.Itoa(value))
}

func (d *tomlDocument) deleteTable(table string) {
	start, end, ok := d.tableSpan(table)
	if !ok {
		return
	}
	d.lines = append(d.lines[:start], d.lines[end:]...)
}

func inlineComment(line string) string {
	quoted := false
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quoted {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if r == '#' && !quoted {
			return " " + strings.TrimSpace(line[i:])
		}
	}
	return ""
}

func referencedAliases(cfg config.Config, connection string) []string {
	var aliases []string
	for alias, target := range cfg.Models {
		if target.Provider == connection {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	return aliases
}

// SortedKeys returns m's keys in ascending order.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func secretName(connection string) string { return "provider/" + connection + "/api-key" }
func secretRef(connection string) string  { return "secret://" + secretName(connection) }

func catalogScopeName(connection string) string { return "provider/" + connection + "/catalog-scope" }

func deleteSecretIfPresent(store secret.Store, name string) error {
	err := store.Delete(name)
	if errors.Is(err, secret.ErrNotFound) {
		return nil
	}
	return err
}

func (m *Manager) newCatalogScope() (string, error) {
	random := m.Random
	if random == nil {
		random = rand.Reader
	}
	b := make([]byte, 32)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", fmt.Errorf("catalogue access: generate enrollment scope: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func redactError(err error, key string) error {
	if err == nil || key == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "[REDACTED]"))
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
	Phase         string `json:"phase"`
	TransactionID string `json:"transaction_id,omitempty"`
	ConfigExisted bool   `json:"config_existed"`
	SecretExisted bool   `json:"secret_existed"`
	ReadyExisted  bool   `json:"ready_existed"`
	ConfigMode    uint32 `json:"config_mode"`
	SecretMode    uint32 `json:"secret_mode"`
	ReadyMode     uint32 `json:"ready_mode"`
	ServiceActive bool   `json:"service_active"`
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
