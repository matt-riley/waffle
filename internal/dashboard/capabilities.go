package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/modelcatalog"
	"github.com/matt-riley/waffle/internal/providerconfig"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/skillinstall"
)

const (
	CapabilityProviderMaxBodyBytes = 64 << 10
	capabilityMutationMaxBodyBytes = 16 << 10
	capabilitySnapshotLockWait     = time.Second
	capabilitySnapshotRetryDelay   = 10 * time.Millisecond
	capabilityRemovalPreviewTTL    = time.Minute
	restartScheduleTimeout         = 5 * time.Second
)

const (
	capabilityModelRemovalOperation    = "desk-model-removal"
	capabilityProviderRemovalOperation = "desk-provider-removal"
)

var (
	ErrCapabilityModelNotFound       = errors.New("capability model not found")
	ErrCapabilitySkillNotFound       = errors.New("capability skill not found")
	ErrCapabilityReplacementRequired = errors.New("capability removal requires an explicit replacement")
	ErrAfterResponseUnavailable      = errors.New("after-response scheduling unavailable")
	ErrCapabilitiesUnavailable       = errors.New("capabilities dependency unavailable")
)

type CapabilityProviders interface {
	Snapshot(context.Context) (providerconfig.Listing, error)
	AddWithMode(context.Context, providerconfig.AddRequest, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	AddModelWithMode(context.Context, providerconfig.AddModelRequest, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	ActivateModelWithMode(context.Context, string, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	ActivateUtilityModelWithMode(context.Context, string, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	PreviewModelRemoval(context.Context, string) (providerconfig.ModelRemovalPreview, error)
	PreviewProviderRemoval(context.Context, string) (providerconfig.ProviderRemovalPreview, error)
	RemoveModelWithMode(context.Context, string, string, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	RemoveModelWithModeAtRevision(context.Context, string, string, string, []providerconfig.SessionAliasChange, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	RemoveWithMode(context.Context, string, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	Test(context.Context, string) error
	TestProspective(context.Context, providerconfig.ProspectiveProbeRequest) error
}

type CapabilitySessions interface {
	Get(context.Context, string) (*session.Session, error)
	SetModelAliasIfVersion(context.Context, string, string, int64) error
	ModelAliasReferences(context.Context, string) ([]string, error)
	ReplaceModelAlias(context.Context, string, string) error
}

type capabilityExactSessionRestorer interface {
	RestoreModelAliases(context.Context, []session.ModelAliasChange) error
}

type capabilityRevisionProviderRemover interface {
	RemoveWithExpectedRevision(context.Context, string, string, providerconfig.CommitMode) (providerconfig.MutationResult, error)
}

type capabilityLockedProviderSnapshot interface {
	WithLockedSnapshot(context.Context, func(context.Context, providerconfig.Listing) error) error
}

type CapabilitySkills interface {
	List(context.Context, string) ([]CapabilitySkill, error)
	Attach(context.Context, string, string) error
	Detach(context.Context, string, string) error
	Stage(context.Context, skillinstall.StageRequest) (skillinstall.Manifest, error)
	Install(context.Context, string, string) (CapabilitySkill, error)
	Activate(context.Context, string) error
	Deactivate(context.Context, string) error
	Uninstall(context.Context, string) error
}

type CapabilityCatalogue interface {
	Refresh(context.Context, string) (CapabilityCatalogueResult, error)
}

// CapabilityCatalogueResult keeps the private values supplied to a catalogue
// fetch beside its untrusted result so the public boundary can redact them.
type CapabilityCatalogueResult struct {
	Result        modelcatalog.Result
	PrivateValues []string
}

// InstallDispositionUnaudited is appended to a committed install's
// disposition when its policy_audit row was lost (#297). A suffix, so it
// composes with the base disposition rather than replacing it.
const InstallDispositionUnaudited = "_without_audit_record"

type CapabilitySkill struct {
	Name               string `json:"name"`
	Description        string `json:"description,omitempty"`
	Active             bool   `json:"active"`
	Attached           bool   `json:"attached"`
	Missing            bool   `json:"missing,omitempty"`
	InstallDisposition string `json:"install_disposition,omitempty"`
}

type CapabilitySession struct {
	ID         string `json:"id"`
	ModelAlias string `json:"model_alias,omitempty"`
}

type CapabilitiesSnapshot struct {
	Providers       providerconfig.Listing  `json:"providers"`
	ProviderPresets []providerconfig.Preset `json:"provider_presets"`
	Session         *CapabilitySession      `json:"session,omitempty"`
	SkillSources    CapabilitySkillSources  `json:"skill_sources"`
	Skills          []CapabilitySkill       `json:"skills"`
}

type CapabilityCatalogueView struct {
	Connection string                     `json:"connection"`
	FetchedAt  time.Time                  `json:"fetched_at"`
	Stale      bool                       `json:"stale"`
	Warning    string                     `json:"warning,omitempty"`
	Models     []CapabilityCatalogueModel `json:"models"`
}

// CapabilityCatalogueModel is the catalogue-card representation. It carries
// the exact upstream ID and only credential-free local enrolment state.
type CapabilityCatalogueModel struct {
	ID              string   `json:"id"`
	DisplayName     string   `json:"display_name,omitempty"`
	Owner           string   `json:"owner,omitempty"`
	ContextWindow   int64    `json:"context_window,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	AliasSuggestion string   `json:"alias_suggestion,omitempty"`
	EnrolledAlias   string   `json:"enrolled_alias,omitempty"`
}

// CapabilityProviderTestResult is a fixed, safe probe outcome with no
// upstream error details.
type CapabilityProviderTestResult struct {
	Outcome providerconfig.ProbeOutcome `json:"outcome"`
}

// CapabilityRemovalReference names one sanitized resource that prevents a
// removal from being treated as an implicit configuration change.
type CapabilityRemovalReference struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// CapabilityRemovalPreview is a short-lived, credential-free destructive
// operation preview. The token is opaque and bound server-side to the exact
// current provider/session state.
type CapabilityRemovalPreview struct {
	Kind                string                       `json:"kind"`
	Target              string                       `json:"target"`
	Provider            string                       `json:"provider,omitempty"`
	References          []CapabilityRemovalReference `json:"references"`
	ReplacementRequired bool                         `json:"replacement_required"`
	PreviewToken        string                       `json:"preview_token"`
	ExpiresAt           time.Time                    `json:"expires_at"`
}

// Capabilities owns the narrow application operations used by the additive
// Desk routes. It never receives or emits raw secret-store state.
type Capabilities struct {
	Providers    CapabilityProviders
	Sessions     CapabilitySessions
	SkillSources CapabilitySkillSources
	Skills       CapabilitySkills
	Catalogue    CapabilityCatalogue
	Previews     *PreviewStore
	Now          func() time.Time
}

func (c *Capabilities) Snapshot(ctx context.Context, sessionID string) (CapabilitiesSnapshot, error) {
	if c == nil || c.Providers == nil {
		return CapabilitiesSnapshot{}, ErrCapabilitiesUnavailable
	}
	providers, err := c.Providers.Snapshot(ctx)
	if err != nil {
		return CapabilitiesSnapshot{}, err
	}
	snapshot := CapabilitiesSnapshot{
		Providers:       providers,
		ProviderPresets: providerconfig.Presets(),
		SkillSources:    NewCapabilitySkillSources(c.SkillSources.LocalRoots, c.SkillSources.GitHosts),
		Skills:          make([]CapabilitySkill, 0),
	}
	if sessionID != "" {
		if c.Sessions == nil {
			return CapabilitiesSnapshot{}, ErrCapabilitiesUnavailable
		}
		current, err := c.Sessions.Get(ctx, sessionID)
		if err != nil {
			return CapabilitiesSnapshot{}, err
		}
		snapshot.Session = &CapabilitySession{ID: current.ID, ModelAlias: current.ModelAlias}
	}
	if c.Skills != nil {
		skills, err := c.Skills.List(ctx, sessionID)
		if err != nil {
			return CapabilitiesSnapshot{}, err
		}
		if skills != nil {
			snapshot.Skills = skills
		}
	}
	return snapshot, nil
}

func (c *Capabilities) SetSessionModel(ctx context.Context, sessionID, alias string) error {
	if c == nil || c.Providers == nil || c.Sessions == nil {
		return ErrCapabilitiesUnavailable
	}
	current, err := c.Sessions.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	alias = strings.TrimSpace(alias)
	if locked, ok := c.Providers.(capabilityLockedProviderSnapshot); ok {
		return locked.WithLockedSnapshot(ctx, func(lockedCtx context.Context, snapshot providerconfig.Listing) error {
			if _, ok := snapshot.Models[alias]; !ok {
				return ErrCapabilityModelNotFound
			}
			return c.Sessions.SetModelAliasIfVersion(lockedCtx, sessionID, alias, current.ModelAliasVersion)
		})
	}
	snapshot, err := c.Providers.Snapshot(ctx)
	if err != nil {
		return err
	}
	if _, ok := snapshot.Models[alias]; !ok {
		return ErrCapabilityModelNotFound
	}
	return c.Sessions.SetModelAliasIfVersion(ctx, sessionID, alias, current.ModelAliasVersion)
}

func (c *Capabilities) SetDefaultModel(ctx context.Context, alias string) (providerconfig.MutationResult, error) {
	if c == nil || c.Providers == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	return c.Providers.ActivateModelWithMode(ctx, alias, providerconfig.CommitForRestart)
}

func (c *Capabilities) SetUtilityModel(ctx context.Context, alias string) (providerconfig.MutationResult, error) {
	if c == nil || c.Providers == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	return c.Providers.ActivateUtilityModelWithMode(ctx, alias, providerconfig.CommitForRestart)
}

func (c *Capabilities) AddModel(ctx context.Context, request providerconfig.AddModelRequest) (providerconfig.MutationResult, error) {
	if c == nil || c.Providers == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	return c.Providers.AddModelWithMode(ctx, request, providerconfig.CommitForRestart)
}

func (c *Capabilities) EnrollProvider(ctx context.Context, request providerconfig.AddRequest) (providerconfig.MutationResult, error) {
	if c == nil || c.Providers == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	return c.Providers.AddWithMode(ctx, request, providerconfig.CommitForRestart)
}

type capabilityRemovalBinding struct {
	Kind       string                       `json:"kind"`
	Target     string                       `json:"target"`
	Revision   string                       `json:"revision"`
	References []CapabilityRemovalReference `json:"references"`
}

func (c *Capabilities) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Capabilities) PreviewModelRemoval(ctx context.Context, alias string) (CapabilityRemovalPreview, error) {
	if c == nil || c.Previews == nil {
		return CapabilityRemovalPreview{}, ErrCapabilitiesUnavailable
	}
	binding, preview, err := c.currentModelRemovalBinding(ctx, alias)
	if err != nil {
		return CapabilityRemovalPreview{}, err
	}
	return c.issueRemovalPreview(binding, preview.Provider, preview.ReplacementRequired)
}

func (c *Capabilities) PreviewProviderRemoval(ctx context.Context, name string) (CapabilityRemovalPreview, error) {
	if c == nil || c.Previews == nil {
		return CapabilityRemovalPreview{}, ErrCapabilitiesUnavailable
	}
	binding, _, err := c.currentProviderRemovalBinding(ctx, name)
	if err != nil {
		return CapabilityRemovalPreview{}, err
	}
	return c.issueRemovalPreview(binding, "", false)
}

func (c *Capabilities) issueRemovalPreview(binding capabilityRemovalBinding, provider string, replacementRequired bool) (CapabilityRemovalPreview, error) {
	resource, err := json.Marshal(binding)
	if err != nil {
		return CapabilityRemovalPreview{}, ErrCapabilitiesUnavailable
	}
	operation := capabilityModelRemovalOperation
	if binding.Kind == "provider" {
		operation = capabilityProviderRemovalOperation
	}
	token := c.Previews.Issue(operation, string(resource), capabilityRemovalPreviewTTL)
	return CapabilityRemovalPreview{
		Kind: binding.Kind, Target: binding.Target, Provider: provider,
		References:          append([]CapabilityRemovalReference(nil), binding.References...),
		ReplacementRequired: replacementRequired, PreviewToken: token,
		ExpiresAt: c.now().Add(capabilityRemovalPreviewTTL),
	}, nil
}

func (c *Capabilities) currentModelRemovalBinding(ctx context.Context, alias string) (capabilityRemovalBinding, CapabilityRemovalPreview, error) {
	if c == nil || c.Providers == nil || c.Sessions == nil {
		return capabilityRemovalBinding{}, CapabilityRemovalPreview{}, ErrCapabilitiesUnavailable
	}
	state, err := c.Providers.PreviewModelRemoval(ctx, strings.TrimSpace(alias))
	if err != nil {
		return capabilityRemovalBinding{}, CapabilityRemovalPreview{}, err
	}
	sessionIDs, err := c.Sessions.ModelAliasReferences(ctx, state.Alias)
	if err != nil {
		return capabilityRemovalBinding{}, CapabilityRemovalPreview{}, err
	}
	sort.Strings(sessionIDs)
	references := make([]CapabilityRemovalReference, 0, len(state.Profiles)+len(sessionIDs)+2)
	if state.Default {
		references = append(references, CapabilityRemovalReference{Kind: "default", Name: "default"})
	}
	if state.Utility {
		references = append(references, CapabilityRemovalReference{Kind: "utility", Name: "utility"})
	}
	for _, name := range state.Profiles {
		references = append(references, CapabilityRemovalReference{Kind: "profile", Name: name})
	}
	for _, id := range sessionIDs {
		references = append(references, CapabilityRemovalReference{Kind: "session", Name: id})
	}
	binding := capabilityRemovalBinding{Kind: "model", Target: state.Alias, Revision: state.Revision, References: references}
	return binding, CapabilityRemovalPreview{
		Kind: binding.Kind, Target: binding.Target, Provider: state.Provider,
		References: references, ReplacementRequired: len(references) > 0,
	}, nil
}

func (c *Capabilities) currentProviderRemovalBinding(ctx context.Context, name string) (capabilityRemovalBinding, CapabilityRemovalPreview, error) {
	if c == nil || c.Providers == nil {
		return capabilityRemovalBinding{}, CapabilityRemovalPreview{}, ErrCapabilitiesUnavailable
	}
	state, err := c.Providers.PreviewProviderRemoval(ctx, strings.TrimSpace(name))
	if err != nil {
		return capabilityRemovalBinding{}, CapabilityRemovalPreview{}, err
	}
	references := make([]CapabilityRemovalReference, 0, len(state.ModelAliases))
	for _, alias := range state.ModelAliases {
		references = append(references, CapabilityRemovalReference{Kind: "model_alias", Name: alias})
	}
	return capabilityRemovalBinding{Kind: "provider", Target: state.Name, Revision: state.Revision, References: references}, CapabilityRemovalPreview{
		Kind: "provider", Target: state.Name, References: references,
	}, nil
}

func encodeCapabilityRemovalBinding(binding capabilityRemovalBinding) (string, error) {
	data, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *Capabilities) ConfirmModelRemoval(ctx context.Context, alias, replacement, token string) (providerconfig.MutationResult, error) {
	if c == nil || c.Providers == nil || c.Sessions == nil || c.Previews == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	resource, err := c.Previews.ConsumeBound(token, capabilityModelRemovalOperation)
	if err != nil {
		return providerconfig.MutationResult{}, err
	}
	var bound capabilityRemovalBinding
	if err := json.Unmarshal([]byte(resource), &bound); err != nil || bound.Kind != "model" || bound.Target != strings.TrimSpace(alias) {
		return providerconfig.MutationResult{}, ErrPreviewMismatch
	}
	current, preview, err := c.currentModelRemovalBinding(ctx, alias)
	if err != nil {
		return providerconfig.MutationResult{}, err
	}
	currentResource, err := encodeCapabilityRemovalBinding(current)
	if err != nil || resource != currentResource {
		return providerconfig.MutationResult{}, ErrPreviewMismatch
	}
	replacement = strings.TrimSpace(replacement)
	if preview.ReplacementRequired && replacement == "" {
		return providerconfig.MutationResult{}, ErrCapabilityReplacementRequired
	}

	var journalChanges []providerconfig.SessionAliasChange
	for _, reference := range current.References {
		if reference.Kind != "session" {
			continue
		}
		value, getErr := c.Sessions.Get(ctx, reference.Name)
		if getErr != nil {
			return providerconfig.MutationResult{}, getErr
		}
		if value.ModelAlias != current.Target {
			return providerconfig.MutationResult{}, ErrPreviewMismatch
		}
		journalChanges = append(journalChanges, providerconfig.SessionAliasChange{
			SessionID: reference.Name, From: current.Target, To: replacement,
			FromVersion: value.ModelAliasVersion, ToVersion: value.ModelAliasVersion + 1,
			FromUpdatedAt: value.UpdatedAt.UTC().Format(time.RFC3339Nano),
			ToUpdatedAt:   c.now().UTC().Format(time.RFC3339Nano),
		})
	}
	result, err := c.Providers.RemoveModelWithModeAtRevision(ctx, current.Target, replacement, current.Revision, journalChanges, providerconfig.CommitForRestart)
	if err == nil {
		return result, nil
	}
	restoreErr := c.restoreSessionAliasChanges(ctx, journalChanges)
	if errors.Is(err, providerconfig.ErrRevisionMismatch) {
		return providerconfig.MutationResult{}, errors.Join(ErrPreviewMismatch, restoreErr)
	}
	return providerconfig.MutationResult{}, errors.Join(err, restoreErr)
}

func (c *Capabilities) restoreSessionAliasChanges(ctx context.Context, changes []providerconfig.SessionAliasChange) error {
	if len(changes) == 0 {
		return nil
	}
	restorer, ok := c.Sessions.(capabilityExactSessionRestorer)
	if !ok {
		return errors.New("exact session alias restoration is unavailable")
	}
	modelChanges := make([]session.ModelAliasChange, 0, len(changes))
	for _, change := range changes {
		modelChanges = append(modelChanges, session.ModelAliasChange{
			SessionID: change.SessionID, OriginalAlias: change.From, ReplacementAlias: change.To,
			OriginalVersion: change.FromVersion, ReplacementVersion: change.ToVersion,
			OriginalUpdatedAt: change.FromUpdatedAt, ReplacementUpdatedAt: change.ToUpdatedAt,
		})
	}
	return restorer.RestoreModelAliases(ctx, modelChanges)
}

func (c *Capabilities) ConfirmProviderRemoval(ctx context.Context, name, token string) (providerconfig.MutationResult, error) {
	if c == nil || c.Providers == nil || c.Previews == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	resource, err := c.Previews.ConsumeBound(token, capabilityProviderRemovalOperation)
	if err != nil {
		return providerconfig.MutationResult{}, err
	}
	var bound capabilityRemovalBinding
	if err := json.Unmarshal([]byte(resource), &bound); err != nil || bound.Kind != "provider" || bound.Target != strings.TrimSpace(name) {
		return providerconfig.MutationResult{}, ErrPreviewMismatch
	}
	current, _, err := c.currentProviderRemovalBinding(ctx, name)
	if err != nil {
		return providerconfig.MutationResult{}, err
	}
	currentResource, err := encodeCapabilityRemovalBinding(current)
	if err != nil || resource != currentResource {
		return providerconfig.MutationResult{}, ErrPreviewMismatch
	}
	if remover, ok := c.Providers.(capabilityRevisionProviderRemover); ok {
		result, err := remover.RemoveWithExpectedRevision(ctx, current.Target, current.Revision, providerconfig.CommitForRestart)
		if errors.Is(err, providerconfig.ErrRevisionMismatch) {
			return providerconfig.MutationResult{}, ErrPreviewMismatch
		}
		return result, err
	}
	return c.Providers.RemoveWithMode(ctx, current.Target, providerconfig.CommitForRestart)
}

func (c *Capabilities) TestProvider(ctx context.Context, name string) (CapabilityProviderTestResult, error) {
	if c == nil || c.Providers == nil {
		return CapabilityProviderTestResult{}, ErrCapabilitiesUnavailable
	}
	return CapabilityProviderTestResult{Outcome: providerconfig.ClassifyProbeError(c.Providers.Test(ctx, strings.TrimSpace(name)))}, nil
}

func (c *Capabilities) TestProspectiveProvider(ctx context.Context, request providerconfig.ProspectiveProbeRequest) (CapabilityProviderTestResult, error) {
	if c == nil || c.Providers == nil {
		return CapabilityProviderTestResult{}, ErrCapabilitiesUnavailable
	}
	if err := providerconfig.ValidateProspectiveProbe(request); err != nil {
		return CapabilityProviderTestResult{}, err
	}
	return CapabilityProviderTestResult{Outcome: providerconfig.ClassifyProbeError(c.Providers.TestProspective(ctx, request))}, nil
}

func (c *Capabilities) RefreshCatalogue(ctx context.Context, connection string) (CapabilityCatalogueView, error) {
	if c == nil || c.Catalogue == nil {
		return CapabilityCatalogueView{}, ErrCapabilitiesUnavailable
	}
	result, err := c.Catalogue.Refresh(ctx, connection)
	if err != nil {
		return CapabilityCatalogueView{}, err
	}
	models := redactCapabilityCatalogueModels(result.Result.Models, result.PrivateValues...)
	if models == nil {
		models = make([]modelcatalog.Model, 0)
	}
	listing := providerconfig.Listing{}
	if c.Providers != nil {
		var err error
		listing, err = c.Providers.Snapshot(ctx)
		if err != nil {
			return CapabilityCatalogueView{}, err
		}
	}
	enrolledAliases := enrolledAliasesByUpstreamModel(listing, result.Result.Connection.Name)
	viewModels := make([]CapabilityCatalogueModel, 0, len(models))
	for _, model := range models {
		viewModel := CapabilityCatalogueModel{
			ID: model.ID, DisplayName: model.DisplayName, Owner: model.Owner,
			ContextWindow: model.ContextWindow, Capabilities: model.Capabilities,
		}
		if suggestion, suggestionErr := modelcatalog.AliasFor(model.ID); suggestionErr == nil {
			viewModel.AliasSuggestion = suggestion
		}
		viewModel.EnrolledAlias = enrolledAliases[model.ID]
		viewModels = append(viewModels, viewModel)
	}
	return CapabilityCatalogueView{
		Connection: redactCapabilityCatalogueText(result.Result.Connection.Name, result.PrivateValues...),
		FetchedAt:  result.Result.FetchedAt,
		Stale:      result.Result.Stale,
		Warning:    redactCapabilityCatalogueText(result.Result.Warning, result.PrivateValues...),
		Models:     viewModels,
	}, nil
}

// enrolledAliasesByUpstreamModel indexes one connection's enrolled aliases by
// the upstream model they target, so catalogue rendering stays a single pass
// over the listing instead of one sorted scan per catalogue model. Ties keep
// the lowest alias, matching the previous sorted first-match behaviour.
func enrolledAliasesByUpstreamModel(listing providerconfig.Listing, connection string) map[string]string {
	aliases := make(map[string]string, len(listing.Models))
	for alias, enrolled := range listing.Models {
		if enrolled.Provider != connection {
			continue
		}
		if existing, ok := aliases[enrolled.Model]; ok && existing <= alias {
			continue
		}
		aliases[enrolled.Model] = alias
	}
	return aliases
}

func redactCapabilityCatalogueModels(models []modelcatalog.Model, private ...string) []modelcatalog.Model {
	redacted := make([]modelcatalog.Model, len(models))
	for index, model := range models {
		model.ID = redactCapabilityCatalogueText(model.ID, private...)
		model.DisplayName = redactCapabilityCatalogueText(model.DisplayName, private...)
		model.Owner = redactCapabilityCatalogueText(model.Owner, private...)
		model.Capabilities = append([]string(nil), model.Capabilities...)
		for capabilityIndex := range model.Capabilities {
			model.Capabilities[capabilityIndex] = redactCapabilityCatalogueText(model.Capabilities[capabilityIndex], private...)
		}
		redacted[index] = model
	}
	return redacted
}

func redactCapabilityCatalogueText(value string, private ...string) string {
	for _, privateValue := range private {
		if privateValue != "" {
			value = strings.ReplaceAll(value, privateValue, "[REDACTED]")
		}
	}
	return value
}

func (c *Capabilities) AttachSkill(ctx context.Context, sessionID, name string) error {
	if c == nil || c.Skills == nil || c.Sessions == nil {
		return ErrCapabilitiesUnavailable
	}
	if _, err := c.Sessions.Get(ctx, sessionID); err != nil {
		return err
	}
	return c.Skills.Attach(ctx, sessionID, name)
}

func (c *Capabilities) DetachSkill(ctx context.Context, sessionID, name string) error {
	if c == nil || c.Skills == nil || c.Sessions == nil {
		return ErrCapabilitiesUnavailable
	}
	if _, err := c.Sessions.Get(ctx, sessionID); err != nil {
		return err
	}
	return c.Skills.Detach(ctx, sessionID, name)
}

func (c *Capabilities) StageSkill(ctx context.Context, request skillinstall.StageRequest) (skillinstall.Manifest, error) {
	if c == nil || c.Skills == nil {
		return skillinstall.Manifest{}, ErrCapabilitiesUnavailable
	}
	return c.Skills.Stage(ctx, request)
}

func (c *Capabilities) InstallSkill(ctx context.Context, stageID, digest string) (CapabilitySkill, error) {
	if c == nil || c.Skills == nil {
		return CapabilitySkill{}, ErrCapabilitiesUnavailable
	}
	return c.Skills.Install(ctx, stageID, digest)
}

func (c *Capabilities) ActivateSkill(ctx context.Context, name string) (providerconfig.MutationResult, error) {
	if c == nil || c.Skills == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	name = strings.TrimSpace(name)
	if err := c.Skills.Activate(ctx, name); err != nil {
		return providerconfig.MutationResult{}, err
	}
	return skillMutationResult("activate-skill", name), nil
}

func (c *Capabilities) DeactivateSkill(ctx context.Context, name string) (providerconfig.MutationResult, error) {
	if c == nil || c.Skills == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	name = strings.TrimSpace(name)
	if err := c.Skills.Deactivate(ctx, name); err != nil {
		return providerconfig.MutationResult{}, err
	}
	return skillMutationResult("deactivate-skill", name), nil
}

func (c *Capabilities) UninstallSkill(ctx context.Context, name string) (providerconfig.MutationResult, error) {
	if c == nil || c.Skills == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	name = strings.TrimSpace(name)
	if err := c.Skills.Uninstall(ctx, name); err != nil {
		return providerconfig.MutationResult{}, err
	}
	return skillMutationResult("uninstall-skill", name), nil
}

func skillMutationResult(operation, name string) providerconfig.MutationResult {
	sum := sha256.Sum256([]byte(operation + "\x00" + name))
	return providerconfig.MutationResult{
		RestartRequired: true,
		TransactionID:   hex.EncodeToString(sum[:16]),
	}
}

type CapabilityMutationFactory func(int64, http.Handler) http.Handler

type CapabilitiesRouteConfig struct {
	Service  *Capabilities
	Mutation CapabilityMutationFactory
	Restart  RestartScheduler
}

// RegisterCapabilitiesRoutes mounts the Desk capability API endpoints.
// The caller owns security, idempotency, the shared router, and server wiring.
func RegisterCapabilitiesRoutes(mux *http.ServeMux, routeConfig CapabilitiesRouteConfig) {
	if mux == nil || routeConfig.Service == nil {
		return
	}
	mux.Handle("GET /api/v1/desk/capabilities", negotiateFragments(capabilitiesSnapshotHandler(routeConfig.Service)))
	if routeConfig.Mutation == nil {
		return
	}
	mutation := func(limit int64, handler http.HandlerFunc) http.Handler {
		return routeConfig.Mutation(limit, negotiateFragments(handler))
	}
	mux.Handle("POST /api/v1/desk/models/session", mutation(capabilityMutationMaxBodyBytes, sessionModelHandler(routeConfig.Service)))
	mux.Handle("POST /api/v1/desk/models/default", mutation(capabilityMutationMaxBodyBytes, globalAliasHandler(routeConfig, false)))
	mux.Handle("POST /api/v1/desk/models/utility", mutation(capabilityMutationMaxBodyBytes, globalAliasHandler(routeConfig, true)))
	mux.Handle("POST /api/v1/desk/models/catalogue/refresh", mutation(capabilityMutationMaxBodyBytes, catalogueRefreshHandler(routeConfig.Service)))
	mux.Handle("POST /api/v1/desk/models", mutation(capabilityMutationMaxBodyBytes, addModelHandler(routeConfig)))
	mux.Handle("GET /api/v1/desk/models/{alias}/removal-preview", modelRemovalPreviewHandler(routeConfig.Service))
	mux.Handle("POST /api/v1/desk/models/{alias}/remove", mutation(capabilityMutationMaxBodyBytes, modelRemovalHandler(routeConfig)))
	mux.Handle("POST /api/v1/desk/providers", mutation(CapabilityProviderMaxBodyBytes, providerEnrollmentHandler(routeConfig)))
	mux.Handle("POST /api/v1/desk/providers/test", mutation(CapabilityProviderMaxBodyBytes, providerProspectiveTestHandler(routeConfig.Service)))
	mux.Handle("POST /api/v1/desk/providers/{name}/test", mutation(capabilityMutationMaxBodyBytes, providerTestHandler(routeConfig.Service)))
	mux.Handle("GET /api/v1/desk/providers/{name}/removal-preview", providerRemovalPreviewHandler(routeConfig.Service))
	mux.Handle("POST /api/v1/desk/providers/{name}/remove", mutation(capabilityMutationMaxBodyBytes, providerRemovalHandler(routeConfig)))
	mux.Handle("POST /api/v1/desk/skills/session/attach", mutation(capabilityMutationMaxBodyBytes, sessionSkillHandler(routeConfig.Service, true)))
	mux.Handle("POST /api/v1/desk/skills/session/detach", mutation(capabilityMutationMaxBodyBytes, sessionSkillHandler(routeConfig.Service, false)))
	mux.Handle("POST /api/v1/desk/skills/stage", mutation(capabilityMutationMaxBodyBytes, stageSkillHandler(routeConfig.Service)))
	mux.Handle("POST /api/v1/desk/skills/install", mutation(capabilityMutationMaxBodyBytes, installSkillHandler(routeConfig.Service)))
	mux.Handle("POST /api/v1/desk/skills/{name}/activate", mutation(capabilityMutationMaxBodyBytes, skillLifecycleHandler(routeConfig, "activate")))
	mux.Handle("POST /api/v1/desk/skills/{name}/deactivate", mutation(capabilityMutationMaxBodyBytes, skillLifecycleHandler(routeConfig, "deactivate")))
	mux.Handle("POST /api/v1/desk/skills/{name}/uninstall", mutation(capabilityMutationMaxBodyBytes, skillLifecycleHandler(routeConfig, "uninstall")))
}

func modelRemovalPreviewHandler(service *Capabilities) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		preview, err := service.PreviewModelRemoval(r.Context(), r.PathValue("alias"))
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, preview)
	})
}

func modelRemovalHandler(routeConfig CapabilitiesRouteConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after, ok := w.(AfterResponseWriter)
		if !ok || routeConfig.Restart == nil {
			writeCapabilityError(w, ErrAfterResponseUnavailable)
			return
		}
		var request struct {
			PreviewToken string `json:"preview_token"`
			Replacement  string `json:"replacement"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		result, err := routeConfig.Service.ConfirmModelRemoval(
			r.Context(), r.PathValue("alias"), request.Replacement, request.PreviewToken,
		)
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		deferRestart(after, routeConfig.Restart, result.TransactionID)
		writeCapabilityMutation(w, result, routeConfig.Restart)
	}
}

func providerRemovalPreviewHandler(service *Capabilities) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		preview, err := service.PreviewProviderRemoval(r.Context(), r.PathValue("name"))
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, preview)
	})
}

func providerRemovalHandler(routeConfig CapabilitiesRouteConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after, ok := w.(AfterResponseWriter)
		if !ok || routeConfig.Restart == nil {
			writeCapabilityError(w, ErrAfterResponseUnavailable)
			return
		}
		var request struct {
			PreviewToken string `json:"preview_token"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		result, err := routeConfig.Service.ConfirmProviderRemoval(
			r.Context(), r.PathValue("name"), request.PreviewToken,
		)
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		deferRestart(after, routeConfig.Restart, result.TransactionID)
		writeCapabilityMutation(w, result, routeConfig.Restart)
	}
}

func capabilitiesSnapshotHandler(service *Capabilities) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), capabilitySnapshotLockWait)
		defer cancel()
		for {
			snapshot, err := service.Snapshot(ctx, r.URL.Query().Get("session_id"))
			if !errors.Is(err, providerconfig.ErrLocked) {
				if err != nil {
					writeCapabilityError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, snapshot)
				return
			}
			timer := time.NewTimer(capabilitySnapshotRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				writeCapabilityError(w, errors.Join(ErrCapabilitiesUnavailable, ctx.Err()))
				return
			case <-timer.C:
			}
		}
	})
}

func sessionModelHandler(service *Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			SessionID string `json:"session_id"`
			Alias     string `json:"alias"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		if err := service.SetSessionModel(r.Context(), request.SessionID, request.Alias); err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

func globalAliasHandler(routeConfig CapabilitiesRouteConfig, utility bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after, ok := w.(AfterResponseWriter)
		if !ok || routeConfig.Restart == nil {
			writeCapabilityError(w, ErrAfterResponseUnavailable)
			return
		}
		var request struct {
			Alias string `json:"alias"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		var (
			result providerconfig.MutationResult
			err    error
		)
		if utility {
			result, err = routeConfig.Service.SetUtilityModel(r.Context(), request.Alias)
		} else {
			result, err = routeConfig.Service.SetDefaultModel(r.Context(), request.Alias)
		}
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		deferRestart(after, routeConfig.Restart, result.TransactionID)
		writeCapabilityMutation(w, result, routeConfig.Restart)
	}
}

func catalogueRefreshHandler(service *Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Connection string `json:"connection"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		result, err := service.RefreshCatalogue(r.Context(), request.Connection)
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func addModelHandler(routeConfig CapabilitiesRouteConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after, ok := w.(AfterResponseWriter)
		if !ok || routeConfig.Restart == nil {
			writeCapabilityError(w, ErrAfterResponseUnavailable)
			return
		}
		var request struct {
			ConnectionName string `json:"connection_name"`
			Alias          string `json:"alias"`
			UpstreamModel  string `json:"upstream_model"`
			Default        bool   `json:"default"`
			Utility        bool   `json:"utility"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		result, err := routeConfig.Service.AddModel(r.Context(), providerconfig.AddModelRequest{
			ConnectionName: request.ConnectionName,
			Alias:          request.Alias,
			UpstreamModel:  request.UpstreamModel,
			Default:        request.Default,
			Utility:        request.Utility,
		})
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		deferRestart(after, routeConfig.Restart, result.TransactionID)
		writeCapabilityMutation(w, result, routeConfig.Restart)
	}
}

func providerEnrollmentHandler(routeConfig CapabilitiesRouteConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after, ok := w.(AfterResponseWriter)
		if !ok || routeConfig.Restart == nil {
			writeCapabilityError(w, ErrAfterResponseUnavailable)
			return
		}
		var request struct {
			ConnectionName string                        `json:"connection_name"`
			Type           string                        `json:"type"`
			BaseURL        string                        `json:"base_url"`
			MaxTokens      int                           `json:"max_tokens"`
			APIKey         string                        `json:"api_key"`
			Models         map[string]config.ModelTarget `json:"models"`
			DefaultModel   string                        `json:"default_model"`
			UtilityModel   string                        `json:"utility_model"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		preset, err := providerconfig.ResolvePreset(request.Type, request.BaseURL)
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		credential := []byte(request.APIKey)
		defer func() {
			clear(credential)
			request.APIKey = ""
		}()
		result, err := routeConfig.Service.EnrollProvider(r.Context(), providerconfig.AddRequest{
			ConnectionName: request.ConnectionName,
			Connection: config.ProviderConnection{
				Type:      preset.RuntimeType,
				BaseURL:   preset.BaseURL,
				MaxTokens: request.MaxTokens,
			},
			Models:       request.Models,
			DefaultModel: request.DefaultModel,
			UtilityModel: request.UtilityModel,
			APIKey:       string(credential),
		})
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		deferRestart(after, routeConfig.Restart, result.TransactionID)
		writeCapabilityMutation(w, result, routeConfig.Restart)
	}
}

func providerTestHandler(service *Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := service.TestProvider(r.Context(), r.PathValue("name"))
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func providerProspectiveTestHandler(service *Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ConnectionName string `json:"connection_name"`
			Type           string `json:"type"`
			BaseURL        string `json:"base_url"`
			MaxTokens      int    `json:"max_tokens"`
			Model          string `json:"model"`
			APIKey         string `json:"api_key"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		preset, err := providerconfig.ResolvePreset(request.Type, request.BaseURL)
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		credential := []byte(request.APIKey)
		defer func() {
			clear(credential)
			request.APIKey = ""
		}()
		result, err := service.TestProspectiveProvider(r.Context(), providerconfig.ProspectiveProbeRequest{
			ConnectionName: strings.TrimSpace(request.ConnectionName),
			Connection: config.ProviderConnection{
				Type:      preset.RuntimeType,
				BaseURL:   preset.BaseURL,
				MaxTokens: request.MaxTokens,
			},
			Model:  strings.TrimSpace(request.Model),
			APIKey: string(credential),
		})
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func sessionSkillHandler(service *Capabilities, attach bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			SessionID string `json:"session_id"`
			Name      string `json:"name"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		var err error
		if attach {
			err = service.AttachSkill(r.Context(), request.SessionID, request.Name)
		} else {
			err = service.DetachSkill(r.Context(), request.SessionID, request.Name)
		}
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

func stageSkillHandler(service *Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			LocalPath string `json:"local_path"`
			GitURL    string `json:"git_url"`
			Commit    string `json:"commit"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		manifest, err := service.StageSkill(r.Context(), skillinstall.StageRequest{
			LocalPath: request.LocalPath,
			GitURL:    request.GitURL,
			Commit:    request.Commit,
		})
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, manifest)
	}
}

func installSkillHandler(service *Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			StageID string `json:"stage_id"`
			Digest  string `json:"digest"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		installed, err := service.InstallSkill(r.Context(), request.StageID, request.Digest)
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, installed)
	}
}

func skillLifecycleHandler(routeConfig CapabilitiesRouteConfig, operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after, ok := w.(AfterResponseWriter)
		if !ok || routeConfig.Restart == nil {
			writeCapabilityError(w, ErrAfterResponseUnavailable)
			return
		}
		var (
			result providerconfig.MutationResult
			err    error
		)
		switch operation {
		case "activate":
			result, err = routeConfig.Service.ActivateSkill(r.Context(), r.PathValue("name"))
		case "deactivate":
			result, err = routeConfig.Service.DeactivateSkill(r.Context(), r.PathValue("name"))
		case "uninstall":
			result, err = routeConfig.Service.UninstallSkill(r.Context(), r.PathValue("name"))
		default:
			err = ErrCapabilitiesUnavailable
		}
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		deferRestart(after, routeConfig.Restart, result.TransactionID)
		writeCapabilityMutation(w, result, routeConfig.Restart)
	}
}

// capabilityMutationResponse is the public body for Waffle-wide mutations that
// may require a process restart. Restart carries the sanitized schedule
// outcome so the browser can leave the wait state without host detail.
// Transaction IDs stay server-side (deferRestart / logs only) and must not
// appear in the browser-facing JSON.
type capabilityMutationResponse struct {
	RestartRequired bool                    `json:"restart_required"`
	Restart         *RestartScheduleOutcome `json:"restart,omitempty"`
}

func writeCapabilityMutation(w http.ResponseWriter, result providerconfig.MutationResult, scheduler RestartScheduler) {
	response := capabilityMutationResponse{
		RestartRequired: result.RestartRequired,
	}
	if result.RestartRequired && scheduler != nil {
		outcome := plannedRestartOutcome(scheduler)
		response.Restart = &outcome
	}
	writeJSON(w, http.StatusAccepted, response)
}

func deferRestart(after AfterResponseWriter, scheduler RestartScheduler, transactionID string) {
	after.AfterResponse(func() RestartScheduleOutcome {
		ctx, cancel := context.WithTimeout(context.Background(), restartScheduleTimeout)
		defer cancel()
		return restartOutcomeFromError(scheduler.Schedule(ctx, transactionID))
	})
}

func decodeCapabilityRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeStrictJSON(w, r, target, func(w http.ResponseWriter) {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Code:    "invalid_request",
			Message: "capability request is invalid",
		})
	})
}

// capabilityErrorMapping is one stable, redacted HTTP mapping for a known
// capability sentinel. Codes and messages are fixed strings chosen here —
// never raw upstream error text, paths, URLs, or credential material.
type capabilityErrorMapping struct {
	err     error
	status  int
	code    string
	message string
}

// capabilityErrorMappings is the ordered table of known capability failures.
// First matching errors.Is wins. Unmapped errors keep the generic fallback.
var capabilityErrorMappings = []capabilityErrorMapping{
	{session.ErrNotFound, http.StatusNotFound, "session_not_found", "session was not found"},
	{session.ErrModelAliasChanged, http.StatusConflict, "session_model_changed", "session model changed; refresh and try again"},
	{ErrCapabilityModelNotFound, http.StatusNotFound, "model_not_found", "model alias was not found"},
	{ErrCapabilitySkillNotFound, http.StatusNotFound, "skill_not_found", "skill was not found"},
	{skill.ErrSkillNotFound, http.StatusNotFound, "skill_not_found", "skill was not found"},
	{skill.ErrSkillActive, http.StatusConflict, "skill_active", "deactivate the skill before uninstalling it"},
	{skill.ErrSkillAttached, http.StatusConflict, "skill_attached", "skill is still attached to one or more sessions"},
	{ErrCapabilityReplacementRequired, http.StatusConflict, "replacement_required", "choose an explicit replacement for every current reference before removing this model alias"},
	{ErrPreviewExpired, http.StatusConflict, "preview_invalid", "removal confirmation is invalid or expired; request a new preview"},
	{ErrPreviewEvicted, http.StatusConflict, "preview_invalid", "removal confirmation is invalid or expired; request a new preview"},
	{ErrPreviewMismatch, http.StatusConflict, "preview_invalid", "removal confirmation no longer matches current state; request a new preview"},
	{ErrPreviewUnknown, http.StatusConflict, "preview_invalid", "removal confirmation is invalid or expired; request a new preview"},
	{ErrPreviewUsed, http.StatusConflict, "preview_invalid", "removal confirmation was already used; request a new preview"},
	{ErrAfterResponseUnavailable, http.StatusServiceUnavailable, "capabilities_unavailable", "capabilities are unavailable"},
	{ErrCapabilitiesUnavailable, http.StatusServiceUnavailable, "capabilities_unavailable", "capabilities are unavailable"},

	// skillinstall review/install path
	{skillinstall.ErrInvalidRequest, http.StatusUnprocessableEntity, "skill_request_invalid", "skill stage request is invalid"},
	{skillinstall.ErrSourceNotAllowed, http.StatusForbidden, "skill_source_not_allowed", "skill source is not allowed; configure [dashboard] skill_import_roots or skill_git_hosts"},
	{skillinstall.ErrGitHostNotAllowed, http.StatusForbidden, "skill_git_host_not_allowed", "git host is not in [dashboard] skill_git_hosts"},
	{skillinstall.ErrCommitRequired, http.StatusUnprocessableEntity, "skill_commit_required", "exact pinned git commit is required"},
	{skillinstall.ErrBoundedGitUnsupported, http.StatusUnprocessableEntity, "skill_git_archive_unsupported", "exact-commit git archive is not supported for this source"},
	{skillinstall.ErrCommitMismatch, http.StatusUnprocessableEntity, "skill_commit_mismatch", "fetched git commit does not match the requested commit"},
	{skillinstall.ErrUnsafeTree, http.StatusUnprocessableEntity, "skill_tree_unsafe", "skill source tree failed safety checks"},
	{skillinstall.ErrTreeTooLarge, http.StatusUnprocessableEntity, "skill_tree_too_large", "skill source exceeds review size limits"},
	{skillinstall.ErrAuditFailed, http.StatusUnprocessableEntity, "skill_audit_failed", "skill audit failed; review the flags and fix the source"},
	{skillinstall.ErrSkillExists, http.StatusConflict, "skill_already_installed", "a skill with that name is already installed"},
	{skillinstall.ErrStageNotFound, http.StatusNotFound, "skill_stage_not_found", "skill install stage was not found"},
	{skillinstall.ErrStageExpired, http.StatusConflict, "skill_stage_expired", "skill install stage expired; stage the skill again"},
	{skillinstall.ErrStageChanged, http.StatusConflict, "skill_stage_changed", "staged skill changed after review; stage the skill again"},
	{skillinstall.ErrDigestMismatch, http.StatusConflict, "skill_digest_mismatch", "review digest does not match the staged skill"},
	{skillinstall.ErrAtomicRenameUnsupported, http.StatusServiceUnavailable, "skill_install_unsupported", "atomic skill installation is not supported on this platform"},

	// providerconfig enrollment/mutation path
	{providerconfig.ErrLocked, http.StatusConflict, "provider_locked", "provider configuration is locked by another operation — retry"},
	{providerconfig.ErrAliasConflict, http.StatusConflict, "model_alias_exists", "that model alias is already enrolled; choose another alias"},
	{providerconfig.ErrReferenced, http.StatusConflict, "provider_referenced", "provider connection is still referenced by model aliases"},
	{providerconfig.ErrDeferredRestartPending, http.StatusConflict, "provider_restart_pending", "provider configuration restart is pending — wait for the restart to finish"},
	{providerconfig.ErrDeferredHealth, http.StatusBadGateway, "provider_restart_health_failed", "provider restart health check failed; previous configuration was restored"},
	{providerconfig.ErrDeferredIntegrity, http.StatusConflict, "provider_restart_integrity_failed", "deferred provider transaction failed integrity checks"},
}

func writeCapabilityError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	response := errorResponse{
		Code:    "capability_failed",
		Message: "capability request could not be completed",
	}
	var attachmentConflict *skill.AttachmentConflictError
	if errors.As(err, &attachmentConflict) {
		status = http.StatusConflict
		response.Code = "skill_attached"
		labels := make([]string, 0, len(attachmentConflict.References))
		for _, reference := range attachmentConflict.References {
			label := reference.SessionID
			if reference.Title != "" {
				label = reference.Title + " (" + reference.SessionID + ")"
			}
			labels = append(labels, label)
		}
		response.Message = "skill is attached to sessions: " + strings.Join(labels, ", ")
		writeJSON(w, status, response)
		return
	}
	for _, mapping := range capabilityErrorMappings {
		if errors.Is(err, mapping.err) {
			status = mapping.status
			response.Code = mapping.code
			response.Message = mapping.message
			break
		}
	}
	writeJSON(w, status, response)
}
