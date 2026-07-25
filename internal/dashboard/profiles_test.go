package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/providerconfig"
)

type profileStoreStub struct {
	putCalls    []providerconfig.ProfileRequest
	putModes    []providerconfig.CommitMode
	putErr      error
	removeName  string
	removeRefs  []string
	removeMode  providerconfig.CommitMode
	removeCalls int
	removeErr   error
	restart     bool
}

func (s *profileStoreStub) PutProfile(_ context.Context, request providerconfig.ProfileRequest, mode providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	s.putCalls = append(s.putCalls, request)
	s.putModes = append(s.putModes, mode)
	if s.putErr != nil {
		return providerconfig.MutationResult{}, s.putErr
	}
	return providerconfig.MutationResult{RestartRequired: s.restart}, nil
}

func (s *profileStoreStub) RemoveProfile(_ context.Context, name string, refs []string, mode providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	s.removeCalls++
	s.removeName = name
	s.removeRefs = refs
	s.removeMode = mode
	if s.removeErr != nil {
		return providerconfig.MutationResult{}, s.removeErr
	}
	return providerconfig.MutationResult{RestartRequired: s.restart}, nil
}

type profileReferenceStub struct {
	refs []string
	err  error
}

func (s profileReferenceStub) ProfileReferences(context.Context, string) ([]string, error) {
	return s.refs, s.err
}

func profileEditorConfig() config.Config {
	return config.Config{
		Sandbox: config.Sandbox{Mode: "host"},
		Agent: config.Agent{
			Groups: map[string]config.AgentGroup{
				config.GroupMain: {Tools: config.ToolPolicy{
					Allow:        []string{"bash", "read_file", "write_file"},
					DenyPrefixes: []string{"rm -rf"},
				}},
			},
			Profiles: map[string]config.AgentProfile{
				"reviewer": {
					System: "You review changes.", Model: "primary", Sandbox: "docker",
					Tools:        config.ToolPolicy{Allow: []string{"read_file"}},
					DenyPrefixes: []string{"git push"},
					MaxTokens:    4096,
				},
			},
		},
	}
}

func newProfileEditorHarness(t *testing.T, refs ProfileReferenceSource) (*ProfileEditor, *profileStoreStub) {
	t.Helper()
	store := &profileStoreStub{}
	now := time.Unix(20_000, 0).UTC()
	previews := NewPreviewStore(func() time.Time { return now }, previewEntropy(64))
	editor := NewProfileEditor(
		store,
		NewPostureService(profileEditorConfig(), nil, nil),
		previews,
		refs,
		nil,
	)
	return editor, store
}

func TestProfileEditorListsStructuredFieldsOnly(t *testing.T) {
	editor, _ := newProfileEditorHarness(t, nil)
	view, err := editor.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Profiles) != 1 {
		t.Fatalf("profiles = %#v", view.Profiles)
	}
	want := ProfileFieldsView{
		Name: "reviewer", System: "You review changes.", Model: "primary",
		Sandbox: "docker", Allow: []string{"read_file"}, Deny: []string{},
		DenyPrefixes: []string{"git push"}, MaxTokens: 4096,
		AllowedChildren: []string{},
	}
	if !reflect.DeepEqual(view.Profiles[0], want) {
		t.Fatalf("profile = %+v, want %+v", view.Profiles[0], want)
	}
	// AC1: the payload is structured; no TOML is offered for editing.
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"[agent.profile", "toml", "="} {
		if strings.Contains(string(payload), forbidden) {
			t.Errorf("list payload leaked raw config syntax %q: %s", forbidden, payload)
		}
	}
}

// AC3: a narrowing edit previews before and after, and only then yields a token.
func TestProfileEditorPreviewsBeforeAndAfter(t *testing.T) {
	editor, store := newProfileEditorHarness(t, nil)
	request := providerconfig.ProfileRequest{
		Name: "reviewer", System: "You review carefully.", Sandbox: "docker",
		Allow: []string{"read_file"}, Deny: []string{"bash"},
	}

	preview, err := editor.Preview(request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !preview.Exists {
		t.Fatal("existing profile previewed as new")
	}
	if preview.PreviewToken == "" || preview.ExpiresInSeconds <= 0 {
		t.Fatalf("preview issued no usable token: %+v", preview)
	}
	if preview.Before.System.Text != "You review changes." {
		t.Fatalf("before prompt = %q", preview.Before.System.Text)
	}
	if preview.After.System.Text != "You review carefully." {
		t.Fatalf("after prompt = %q", preview.After.System.Text)
	}
	// The after side must reflect the candidate, not the stored profile.
	if !containsStringValue(preview.After.Effective.Deny, "bash") {
		t.Fatalf("after policy did not apply the candidate deny: %+v", preview.After.Effective)
	}
	if containsStringValue(preview.Before.Effective.Deny, "bash") {
		t.Fatalf("before policy already denied bash: %+v", preview.Before.Effective)
	}
	if len(store.putCalls) != 0 {
		t.Fatal("previewing wrote config")
	}
}

// AC2: a widening edit is refused, names the field, and never issues a token.
func TestProfileEditorRefusesWideningWithNamedField(t *testing.T) {
	cfg := config.Config{
		Sandbox: config.Sandbox{Mode: "docker"},
		Agent: config.Agent{Groups: map[string]config.AgentGroup{
			config.GroupMain: {
				Sandbox: "docker",
				Tools:   config.ToolPolicy{Allow: []string{"read_file"}},
			},
		}},
	}
	store := &profileStoreStub{}
	now := time.Unix(20_000, 0).UTC()
	editor := NewProfileEditor(
		store,
		NewPostureService(cfg, nil, nil),
		NewPreviewStore(func() time.Time { return now }, previewEntropy(64)),
		nil, nil,
	)

	for _, tc := range []struct {
		name      string
		request   providerconfig.ProfileRequest
		wantField string
	}{
		{
			name:      "sandbox escape",
			request:   providerconfig.ProfileRequest{Name: "escape", Sandbox: "host"},
			wantField: "sandbox",
		},
		{
			name:      "allow beyond the group",
			request:   providerconfig.ProfileRequest{Name: "escape", Allow: []string{"read_file", "bash"}},
			wantField: "tools.allow",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := editor.Preview(tc.request)
			var widening *config.ProfileWideningError
			if !errors.As(err, &widening) {
				t.Fatalf("Preview error = %v, want a widening refusal", err)
			}
			if widening.Field != tc.wantField {
				t.Fatalf("field = %q, want %q", widening.Field, tc.wantField)
			}

			// The HTTP shape names the field so the editor can point at it.
			recorder := httptest.NewRecorder()
			writeProfileError(recorder, err)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", recorder.Code)
			}
			var response ProfileWideningResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Code != "profile_widens_group" || response.Field != tc.wantField {
				t.Fatalf("response = %+v", response)
			}
		})
	}
	if len(store.putCalls) != 0 {
		t.Fatal("a refused edit reached the config store")
	}
}

func TestProfileEditorSaveRequiresTheMatchingPreview(t *testing.T) {
	editor, store := newProfileEditorHarness(t, nil)
	store.restart = true
	request := providerconfig.ProfileRequest{
		Name: "reviewer", System: "You review carefully.", Allow: []string{"read_file"},
	}

	if _, err := editor.Save(t.Context(), request, "not-a-token"); err == nil {
		t.Fatal("save accepted an unissued token")
	}
	if len(store.putCalls) != 0 {
		t.Fatal("an unreviewed save reached the config store")
	}

	preview, err := editor.Preview(request)
	if err != nil {
		t.Fatal(err)
	}

	// The token is bound to the exact candidate, so swapping fields between
	// preview and confirm is refused. Presenting a token also burns it
	// (shared PreviewStore anti-replay), so a tampered confirm forces a fresh
	// review rather than leaving the original token usable.
	swapped := request
	swapped.Allow = []string{"bash"}
	if _, err := editor.Save(t.Context(), swapped, preview.PreviewToken); err == nil {
		t.Fatal("save accepted a candidate that differed from the preview")
	}
	if len(store.putCalls) != 0 {
		t.Fatal("a swapped save reached the config store")
	}
	if _, err := editor.Save(t.Context(), request, preview.PreviewToken); err == nil {
		t.Fatal("a token survived being presented with the wrong candidate")
	}

	preview, err = editor.Preview(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := editor.Save(t.Context(), request, preview.PreviewToken)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if response.Profile != "reviewer" || !response.RestartRequired {
		t.Fatalf("response = %+v", response)
	}
	if len(store.putCalls) != 1 {
		t.Fatalf("put calls = %d", len(store.putCalls))
	}
	// AC5: the change is restart-deferred like the other capability mutations.
	if store.putModes[0] != providerconfig.CommitForRestart {
		t.Fatalf("commit mode = %v, want CommitForRestart", store.putModes[0])
	}

	// A token is single-use.
	if _, err := editor.Save(t.Context(), request, preview.PreviewToken); err == nil {
		t.Fatal("preview token was reusable")
	}
}

// AC4: deleting a referenced profile is refused with the references named.
func TestProfileEditorDeleteNamesReferences(t *testing.T) {
	editor, store := newProfileEditorHarness(t, profileReferenceStub{
		refs: []string{"scheduled job Daily review", "workspace matt-riley/waffle"},
	})

	preview, err := editor.PreviewDelete(t.Context(), "reviewer")
	if err != nil {
		t.Fatalf("PreviewDelete: %v", err)
	}
	if preview.Eligible {
		t.Fatal("a referenced profile previewed as deletable")
	}
	want := []string{"scheduled job Daily review", "workspace matt-riley/waffle"}
	if !reflect.DeepEqual(preview.References, want) {
		t.Fatalf("references = %v, want %v", preview.References, want)
	}
	// No token is issued for an ineligible delete, so it cannot be confirmed.
	if preview.PreviewToken != "" {
		t.Fatal("an ineligible delete was given a token")
	}
	if store.removeCalls != 0 {
		t.Fatal("previewing a delete reached the config store")
	}
}

func TestProfileEditorDeleteForwardsReferencesToTheManager(t *testing.T) {
	// No references: the delete is eligible and gets a token.
	editor, store := newProfileEditorHarness(t, profileReferenceStub{})
	preview, err := editor.PreviewDelete(t.Context(), "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Eligible || preview.PreviewToken == "" {
		t.Fatalf("clean delete was not previewed as eligible: %+v", preview)
	}

	response, err := editor.Delete(t.Context(), "reviewer", preview.PreviewToken)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if response.Profile != "reviewer" {
		t.Fatalf("response = %+v", response)
	}
	if store.removeName != "reviewer" || store.removeMode != providerconfig.CommitForRestart {
		t.Fatalf("remove call = %q mode %v", store.removeName, store.removeMode)
	}
	// The manager re-checks references under its own lock, so it is handed
	// whatever the second read found rather than the preview's answer.
	if store.removeRefs == nil {
		t.Fatal("references were not forwarded to the manager")
	}
}

func TestProfileEditorDeleteRejectsUnknownProfile(t *testing.T) {
	editor, _ := newProfileEditorHarness(t, nil)
	_, err := editor.PreviewDelete(t.Context(), "absent")
	if !errors.Is(err, providerconfig.ErrProfileNotFound) {
		t.Fatalf("error = %v, want ErrProfileNotFound", err)
	}
}

func TestProfileEditorRejectsInvalidNames(t *testing.T) {
	editor, store := newProfileEditorHarness(t, nil)
	for _, name := range []string{"", "Not A Slug", "../escape", strings.Repeat("a", 100)} {
		if _, err := editor.Preview(providerconfig.ProfileRequest{Name: name}); !errors.Is(err, ErrProfileInvalid) {
			t.Errorf("Preview(%q) error = %v, want ErrProfileInvalid", name, err)
		}
		if _, err := editor.PreviewDelete(t.Context(), name); !errors.Is(err, ErrProfileInvalid) {
			t.Errorf("PreviewDelete(%q) error = %v, want ErrProfileInvalid", name, err)
		}
	}
	if len(store.putCalls) != 0 || store.removeCalls != 0 {
		t.Fatal("an invalid name reached the config store")
	}
}

func TestProfileRoutesRejectUnknownFieldsAndUnreviewedWrites(t *testing.T) {
	editor, store := newProfileEditorHarness(t, nil)
	security := mustSecurity(t, "127.0.0.1:8422")
	mux := http.NewServeMux()
	RegisterProfileRoutes(mux, ProfileRouteConfig{
		Editor: editor,
		Mutation: func(limit int64, next http.Handler) http.Handler {
			return NewMutationHandler(security, NewIdempotencyStore(nil, 64, time.Minute), limit, next)
		},
	})

	post := func(path, body, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422"+path, strings.NewReader(body))
		request.Host = "127.0.0.1:8422"
		request.Header.Set("X-Waffle-Desk-Token", security.Token())
		request.Header.Set("Idempotency-Key", key)
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder
	}

	// A client cannot smuggle config keys the editor does not model.
	unknown := post("/api/v1/desk/profiles/preview", `{"name":"reviewer","raw_toml":"[agent]"}`, "unknown-field")
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want 400: %s", unknown.Code, unknown.Body.String())
	}

	// Saving without a reviewed change is refused before the store is touched.
	unreviewed := post("/api/v1/desk/profiles", `{"name":"reviewer"}`, "no-token")
	if unreviewed.Code != http.StatusBadRequest {
		t.Fatalf("unreviewed save status = %d, want 400: %s", unreviewed.Code, unreviewed.Body.String())
	}
	if len(store.putCalls) != 0 {
		t.Fatal("an unreviewed save reached the config store")
	}

	// The list endpoint is a GET and takes no mutation.
	list := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/profiles", nil)
	list.Host = "127.0.0.1:8422"
	listRecorder := httptest.NewRecorder()
	mux.ServeHTTP(listRecorder, list)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", listRecorder.Code, listRecorder.Body.String())
	}
}

func containsStringValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
