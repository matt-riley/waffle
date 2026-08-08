package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/dashboard"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/providerconfig"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/skillinstall"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/workspace"
)

const dashboardProcessGenerationBytes = 32

type dashboardProviderCatalogue struct {
	providers interface {
		CatalogSnapshot(context.Context, string) (providerconfig.CatalogSnapshot, error)
	}
	catalogue providerCatalogue
}

func (c dashboardProviderCatalogue) Refresh(ctx context.Context, name string) (dashboard.CapabilityCatalogueResult, error) {
	if c.providers == nil || c.catalogue == nil {
		return dashboard.CapabilityCatalogueResult{}, dashboard.ErrCapabilitiesUnavailable
	}
	name = strings.TrimSpace(name)
	snapshot, err := c.providers.CatalogSnapshot(ctx, name)
	if err != nil {
		return dashboard.CapabilityCatalogueResult{}, err
	}
	connection, _, err := effectiveCatalogConnection(name, snapshot.Connection, snapshot.ScopeID)
	if err != nil {
		return dashboard.CapabilityCatalogueResult{}, err
	}
	result, err := c.catalogue.Models(ctx, connection, snapshot.APIKey, true)
	if err != nil {
		return dashboard.CapabilityCatalogueResult{}, err
	}
	return dashboard.CapabilityCatalogueResult{
		Result:        result,
		PrivateValues: []string{snapshot.APIKey, snapshot.ScopeID},
	}, nil
}

func newDashboardProcessGeneration(random io.Reader) (string, error) {
	if random == nil {
		return "", fmt.Errorf("dashboard process generation: random source required")
	}
	value := make([]byte, dashboardProcessGenerationBytes)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", fmt.Errorf("dashboard process generation: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func dashboardRestartScheduler() dashboard.RestartScheduler {
	if dashboardManagedProcess() {
		return dashboard.ManagedRestartScheduler{}
	}
	return dashboard.StandaloneRestartScheduler{}
}

func dashboardManagedProcess() bool {
	return strings.TrimSpace(os.Getenv("INVOCATION_ID")) != ""
}

func newDashboardCapabilities(
	cfg config.Config,
	st *store.Store,
	ws memory.Workspace,
	sessions *session.Store,
	providers *providerconfig.Manager,
	catalogue providerCatalogue,
) (*dashboard.Capabilities, error) {
	if st == nil || sessions == nil || providers == nil || catalogue == nil || strings.TrimSpace(ws.Dir) == "" {
		return nil, dashboard.ErrCapabilitiesUnavailable
	}
	toModelChanges := func(changes []providerconfig.SessionAliasChange) []session.ModelAliasChange {
		modelChanges := make([]session.ModelAliasChange, 0, len(changes))
		for _, change := range changes {
			modelChanges = append(modelChanges, session.ModelAliasChange{
				SessionID: change.SessionID, OriginalAlias: change.From, ReplacementAlias: change.To,
				OriginalVersion: change.FromVersion, ReplacementVersion: change.ToVersion,
				OriginalUpdatedAt: change.FromUpdatedAt, ReplacementUpdatedAt: change.ToUpdatedAt,
			})
		}
		return modelChanges
	}
	providers.SetSessionApply(func(ctx context.Context, changes []providerconfig.SessionAliasChange) error {
		return sessions.ReplaceModelAliases(ctx, toModelChanges(changes))
	})
	providers.SetSessionRecovery(func(ctx context.Context, changes []providerconfig.SessionAliasChange) error {
		return sessions.RestoreModelAliases(ctx, toModelChanges(changes))
	})
	providers.SetSessionAliasPlanner(func(ctx context.Context, alias, replacement string) ([]providerconfig.SessionAliasChange, error) {
		ids, err := sessions.ModelAliasReferences(ctx, alias)
		if err != nil {
			return nil, err
		}
		changes := make([]providerconfig.SessionAliasChange, 0, len(ids))
		updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
		for _, id := range ids {
			current, getErr := sessions.Get(ctx, id)
			if getErr != nil {
				return nil, getErr
			}
			changes = append(changes, providerconfig.SessionAliasChange{
				SessionID: id, From: alias, To: replacement,
				FromVersion: current.ModelAliasVersion, ToVersion: current.ModelAliasVersion + 1,
				FromUpdatedAt: current.UpdatedAt.UTC().Format(time.RFC3339Nano), ToUpdatedAt: updatedAt,
			})
		}
		return changes, nil
	})
	home, err := config.Home()
	if err != nil {
		return nil, fmt.Errorf("resolve Waffle home for reviewed skill staging: %w", err)
	}
	installer := skillinstall.New(
		ws.SkillsDir(),
		filepath.Join(home, "skill-staging"),
		cfg.Dashboard.SkillImportRoots,
		cfg.Dashboard.SkillGitHosts,
	)
	// Share the host policy_audit trail with skillinstall mutations (#152).
	installer.AuditDB = st.DB
	// A skill installed without its audit row is reported, not dropped (#297).
	installer.Log = slog.Default()
	return &dashboard.Capabilities{
		Providers:    providers,
		Sessions:     sessions,
		SkillSources: dashboard.NewCapabilitySkillSources(cfg.Dashboard.SkillImportRoots, cfg.Dashboard.SkillGitHosts),
		Skills: &dashboard.WorkspaceCapabilitySkills{
			DB:          st.DB,
			Workspace:   ws,
			Attachments: &skill.Attachments{DB: st.DB, Workspace: ws, Lifecycle: st.SkillLifecycleGuard()},
			Lifecycle:   st.SkillLifecycleGuard(),
			Installer:   installer,
		},
		Catalogue: dashboardProviderCatalogue{
			providers: providers,
			catalogue: catalogue,
		},
	}, nil
}

// deskSecretIdentity is Desk's window onto the secret-store identity (#192).
// It probes on every call rather than caching, so the checklist reflects an
// identity created out-of-band by `waffle secret init` without a restart, and
// it never returns the identity itself to any caller.
type deskSecretIdentity struct{}

func (deskSecretIdentity) IdentityConfigured() (bool, error) {
	if _, err := secret.LoadIdentity(); err != nil {
		if errors.Is(err, secret.ErrNoIdentity) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CreateIdentity generates the identity into the OS keyring and discards the
// value. `waffle secret export-identity` is the sanctioned way to read it back
// for backup; it is never returned here, and so never reaches the browser.
func (deskSecretIdentity) CreateIdentity() error {
	_, err := secret.InitIdentity(false)
	return err
}

func defaultDashboardProviderManager() (*providerconfig.Manager, error) {
	identity, err := secret.LoadIdentity()
	if err != nil && !errors.Is(err, secret.ErrNoIdentity) {
		return nil, err
	}
	manager, err := providerconfig.New(identity)
	if err != nil {
		return nil, err
	}
	configureDefaultProviderManager(manager)
	if !dashboardManagedProcess() {
		manager.Restart = func(context.Context) error { return nil }
		manager.Stop = func(context.Context) error { return nil }
		manager.ServiceActive = func(context.Context) (bool, error) { return false, nil }
		manager.RestoreService = func(context.Context, bool) error { return nil }
	}
	return manager, nil
}

func configureServeWorkspaceManager(cfg config.Config, manager *workspace.Manager, brokerURL string) {
	if manager == nil || brokerURL == "" {
		return
	}
	manager.BrokerURL = brokerURL
	if cfg.Workspace.Egress != "full" {
		manager.ProxyURL = strings.TrimRight(brokerURL, "/") + "/egress"
	}
}

// verifyProviderReadiness re-proves provider readiness for the config this
// process just loaded. It reuses an already-built manager when Desk supplied one
// and otherwise builds its own, because the stale-generation problem it repairs
// is independent of whether Desk is enabled.
//
// Every failure is logged rather than returned: readiness verification is a
// repair, and a host that cannot complete it must still finish starting.
func verifyProviderReadiness(ctx context.Context, log *slog.Logger, manager *providerconfig.Manager) {
	if manager == nil {
		built, err := defaultDashboardProviderManager()
		if err != nil {
			log.Warn("provider readiness verification unavailable", "err", err)
			return
		}
		manager = built
	}
	refreshed, err := manager.VerifyReadiness(ctx)
	switch {
	case errors.Is(err, providerconfig.ErrLocked):
		// A concurrent provider mutation owns the lock and will write the
		// generation itself.
		log.Debug("provider readiness verification skipped; provider config is locked")
	case err != nil:
		log.Warn("verify provider readiness", "err", err)
	case refreshed:
		log.Info("re-proved provider readiness for the current config")
	}
}

func serveBrokerURL(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return ""
	}
	return "http://waffle-host:" + port
}

var _ dashboard.CapabilityCatalogue = dashboardProviderCatalogue{}
