// Package providerconfig manages on-host provider enrollment as one
// transaction across config.toml, the encrypted secret store, and service
// activation.
package providerconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	// ErrReferenced means a connection cannot be removed while model aliases
	// still point at it.
	ErrReferenced = errors.New("provider connection is referenced")
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
	ConfigPath  string
	SecretsPath string
	LockPath    string
	Identity    *age.X25519Identity
	Probe       Probe
	Restart     func(context.Context) error
	Health      func(context.Context) error
	// RestoreService is called after files are rolled back. The status is the
	// pre-transaction state, allowing a host adapter to restart or stop Waffle.
	RestoreService func(context.Context, Status) error
	// AfterCommit is an observation/fault-injection hook. It is invoked after
	// the named resource ("secret" then "config") is durably committed.
	AfterCommit func(resource string) error
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
	}, nil
}

// Add validates and enrolls a connection and one or more aliases.
func (m *Manager) Add(ctx context.Context, req AddRequest) (err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()

	if err := validateAdd(req); err != nil {
		return err
	}
	before, err := m.capture()
	if err != nil {
		return err
	}
	if _, exists := before.cfg.Providers[req.ConnectionName]; exists {
		return fmt.Errorf("provider connection %q already exists", req.ConnectionName)
	}
	for alias := range req.Models {
		if _, exists := before.cfg.Models[alias]; exists {
			return fmt.Errorf("model alias %q already exists", alias)
		}
	}
	previousStatus, err := m.statusFromConfig(ctx, before.cfg)
	if err != nil {
		return err
	}
	legacyKey, err := m.legacyCredential(before.cfg)
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
	defer func() { _ = os.Remove(configStage) }()

	secretStage, err := m.stageSecrets(before.secretBytes, func(store secret.Store) error {
		if legacyKey != "" {
			if err := store.Set(secretName("default"), legacyKey); err != nil {
				return err
			}
		}
		if req.APIKey == "" {
			return nil
		}
		return store.Set(secretName(req.ConnectionName), req.APIKey)
	})
	if err != nil {
		return redactError(err, req.APIKey)
	}
	defer func() { _ = os.Remove(secretStage) }()

	if m.Probe == nil {
		return errors.New("provider probe is not configured")
	}
	aliases := sortedKeys(req.Models)
	for _, alias := range aliases {
		target, resolveErr := candidate.ResolveModel(alias)
		if resolveErr != nil {
			return resolveErr
		}
		if probeErr := m.Probe(ctx, target, req.APIKey); probeErr != nil {
			return redactError(fmt.Errorf("probe model alias %q: %w", alias, probeErr), req.APIKey)
		}
	}

	return m.commit(ctx, before, configStage, secretStage, candidate, previousStatus, req.APIKey)
}

// Remove deletes an unreferenced connection and its scoped credential.
func (m *Manager) Remove(ctx context.Context, name string) (err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if !config.ValidProviderConnectionName(name) {
		return fmt.Errorf("invalid connection name %q", name)
	}
	before, err := m.capture()
	if err != nil {
		return err
	}
	if _, ok := before.cfg.Providers[name]; !ok {
		return fmt.Errorf("provider connection %q does not exist", name)
	}
	refs := referencedAliases(before.cfg, name)
	if len(refs) > 0 {
		return fmt.Errorf("%w: %q is used by model aliases %s", ErrReferenced, name, strings.Join(refs, ", "))
	}
	previousStatus, err := m.statusFromConfig(ctx, before.cfg)
	if err != nil {
		return err
	}
	configStage, candidate, err := m.stageConfig(before.configBytes, func(doc *tomlDocument, cfg config.Config) error {
		migrateLegacyDocument(doc, cfg, false)
		doc.deleteTable("providers." + name)
		return nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(configStage) }()
	secretStage, err := m.stageSecrets(before.secretBytes, func(store secret.Store) error {
		deleteErr := store.Delete(secretName(name))
		if errors.Is(deleteErr, secret.ErrNotFound) {
			return nil
		}
		return deleteErr
	})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(secretStage) }()
	return m.commit(ctx, before, configStage, secretStage, candidate, previousStatus, "")
}

// Test probes the first alias for a named connection using its encrypted key.
func (m *Manager) Test(ctx context.Context, name string) error {
	if m.Probe == nil {
		return errors.New("provider probe is not configured")
	}
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		return err
	}
	aliases := referencedAliases(cfg, name)
	if len(aliases) == 0 {
		if _, ok := cfg.Providers[name]; !ok {
			return fmt.Errorf("provider connection %q does not exist", name)
		}
		return fmt.Errorf("provider connection %q has no model aliases to test", name)
	}
	target, err := cfg.ResolveModel(aliases[0])
	if err != nil {
		return err
	}
	key, err := m.connectionKey(target.Connection)
	if err != nil {
		return err
	}
	if err := m.Probe(ctx, target, key); err != nil {
		return redactError(fmt.Errorf("probe provider %q model alias %q: %w", name, aliases[0], err), key)
	}
	return nil
}

// Status reports Installed unless a configured default alias is healthy.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		return Status{}, err
	}
	return m.statusFromConfig(ctx, cfg)
}

// List returns a deterministic, credential-free JSON document.
func (m *Manager) List(ctx context.Context) ([]byte, error) {
	cfg, err := config.Load(m.ConfigPath)
	if err != nil {
		return nil, err
	}
	status, err := m.statusFromConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	providers := make(map[string]ProviderSummary, len(cfg.Providers))
	for name, connection := range cfg.Providers {
		providers[name] = ProviderSummary{Type: connection.Type, BaseURL: connection.BaseURL, MaxTokens: connection.MaxTokens}
	}
	models := make(map[string]ModelSummary, len(cfg.Models))
	for alias, target := range cfg.Models {
		models[alias] = ModelSummary{Provider: target.Provider, Model: target.Model, MaxTokens: target.MaxTokens}
	}
	listing := Listing{State: status.State, DefaultModel: status.DefaultModel, Providers: providers, Models: models}
	b, err := json.MarshalIndent(listing, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

type snapshot struct {
	configBytes []byte
	secretBytes []byte
	configMode  fs.FileMode
	secretMode  fs.FileMode
	configExist bool
	secretExist bool
	cfg         config.Config
}

func (m *Manager) capture() (snapshot, error) {
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
	return snapshot{configBytes, secretBytes, configMode, secretMode, configExist, secretExist, cfg}, nil
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

func (m *Manager) commit(ctx context.Context, before snapshot, configStage, secretStage string, candidate config.Config, previous Status, key string) (err error) {
	if err := writeBackups(m, before); err != nil {
		return err
	}
	committed := false
	defer func() {
		if err == nil {
			removeErr := errors.Join(removeIfExists(m.ConfigPath+".bak"), removeIfExists(m.SecretsPath+".bak"))
			err = errors.Join(err, removeErr)
			return
		}
		if committed {
			restoreErr := restoreSnapshot(m, before)
			if m.RestoreService != nil {
				restoreErr = errors.Join(restoreErr, m.RestoreService(ctx, previous))
			}
			err = errors.Join(redactError(err, key), restoreErr)
		}
	}()

	if err = commitStage(secretStage, m.SecretsPath, 0o600); err != nil {
		return err
	}
	committed = true
	if m.AfterCommit != nil {
		if err = m.AfterCommit("secret"); err != nil {
			return err
		}
	}
	if err = commitStage(configStage, m.ConfigPath, 0o600); err != nil {
		return err
	}
	if m.AfterCommit != nil {
		if err = m.AfterCommit("config"); err != nil {
			return err
		}
	}
	if candidate.Agent.DefaultModel == "" {
		return nil
	}
	if m.Restart == nil || m.Health == nil {
		return errors.New("service activation callbacks are not configured")
	}
	if err = m.Restart(ctx); err != nil {
		return fmt.Errorf("restart waffle service: %w", err)
	}
	if err = m.Health(ctx); err != nil {
		return fmt.Errorf("waffle health check: %w", err)
	}
	return nil
}

func (m *Manager) statusFromConfig(ctx context.Context, cfg config.Config) (Status, error) {
	status := Status{State: "installed", DefaultModel: cfg.Agent.DefaultModel}
	if cfg.Agent.DefaultModel == "" {
		return status, nil
	}
	if _, err := cfg.ResolveModel(cfg.Agent.DefaultModel); err != nil {
		return Status{}, err
	}
	if m.Health != nil && m.Health(ctx) == nil {
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

func (m *Manager) legacyCredential(cfg config.Config) (string, error) {
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
		return os.Getenv(env), nil
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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func secretName(connection string) string { return "provider/" + connection + "/api-key" }
func secretRef(connection string) string  { return "secret://" + secretName(connection) }

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
	return nil
}

func restoreSnapshot(m *Manager, before snapshot) error {
	return errors.Join(
		restoreOne(m.SecretsPath, before.secretBytes, before.secretMode, before.secretExist),
		restoreOne(m.ConfigPath, before.configBytes, before.configMode, before.configExist),
	)
}

func restoreOne(path string, data []byte, mode fs.FileMode, existed bool) error {
	if !existed {
		return removeIfExists(path)
	}
	return writeDurable(path, data, mode)
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
