package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm/anthropicp"
	"github.com/matt-riley/waffle/internal/llm/openaip"
	"github.com/matt-riley/waffle/internal/modelcatalog"
	"github.com/matt-riley/waffle/internal/providerconfig"
)

const openRouterBaseURL = providerconfig.OpenRouterBaseURL

const (
	maxProviderPromptBytes = 64 * 1024
	cataloguePageSize      = 20
)

type providerPreset struct {
	Name          string
	RuntimeType   string
	StoredBaseURL string
}

type providerCatalogue interface {
	Discover(context.Context, modelcatalog.Connection, string) (modelcatalog.Result, error)
	Models(context.Context, modelcatalog.Connection, string, bool) (modelcatalog.Result, error)
	Save(modelcatalog.Connection, []modelcatalog.Model, time.Time) error
	Invalidate(string) error
}

type catalogueOutput struct {
	Connection string               `json:"connection"`
	FetchedAt  time.Time            `json:"fetched_at"`
	AgeSeconds int64                `json:"age_seconds"`
	Stale      bool                 `json:"stale"`
	Warning    string               `json:"warning,omitempty"`
	Models     []modelcatalog.Model `json:"models"`
}

type catalogueSourceFactory func(modelcatalog.Connection, string, bool) (modelcatalog.Source, error)

type providerCatalogueService struct {
	store     *modelcatalog.Store
	newSource catalogueSourceFactory
	now       func() time.Time
}

func promptLineNoReadAhead(r io.Reader, w io.Writer, label, defaultValue string) (string, error) {
	if defaultValue == "" {
		if _, err := fmt.Fprintf(w, "%s: ", label); err != nil {
			return "", fmt.Errorf("write %s prompt: %w", label, err)
		}
	} else {
		if _, err := fmt.Fprintf(w, "%s [%s]: ", label, defaultValue); err != nil {
			return "", fmt.Errorf("write %s prompt: %w", label, err)
		}
	}
	line := make([]byte, 0, 128)
	var one [1]byte
	for {
		n, err := r.Read(one[:])
		if n > 0 {
			if one[0] == '\n' {
				break
			}
			if len(line) == maxProviderPromptBytes {
				return "", fmt.Errorf("read %s: input line is too long (max %d bytes)", label, maxProviderPromptBytes)
			}
			line = append(line, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				break
			}
			return "", fmt.Errorf("read %s: %w", label, err)
		}
		if n == 0 {
			return "", fmt.Errorf("read %s: reader made no progress", label)
		}
	}
	value := strings.TrimSpace(strings.TrimSuffix(string(line), "\r"))
	if value == "" {
		return defaultValue, nil
	}
	return value, nil
}

func renderCataloguePage(w io.Writer, models []modelcatalog.Model, page int, private ...string) (int, error) {
	if len(models) == 0 {
		return 0, errors.New("model catalogue has no matching entries")
	}
	pageCount := (len(models) + cataloguePageSize - 1) / cataloguePageSize
	if page < 0 || page >= pageCount {
		return 0, fmt.Errorf("catalogue page %d is out of range", page+1)
	}
	start := page * cataloguePageSize
	end := min(start+cataloguePageSize, len(models))
	if _, err := fmt.Fprintf(w, "Page %d of %d\n", page+1, pageCount); err != nil {
		return 0, err
	}
	for index, model := range models[start:end] {
		displayName := safeCatalogueText(model.DisplayName, private...)
		id := safeCatalogueText(model.ID, private...)
		if displayName == "" {
			if _, err := fmt.Fprintf(w, "  %d. %s\n", index+1, id); err != nil {
				return 0, err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "  %d. %s\t%s\n", index+1, displayName, id); err != nil {
			return 0, err
		}
	}
	return end - start, nil
}

func selectCatalogueModel(r io.Reader, w io.Writer, label string, models []modelcatalog.Model, optional bool, private ...string) (modelcatalog.Model, bool, error) {
	if len(models) == 0 {
		return modelcatalog.Model{}, false, errors.New("discovered model catalogue is empty")
	}
	current := models
	page := 0
	displayed := false
	if len(models) <= 50 {
		if _, err := renderCataloguePage(w, current, page, private...); err != nil {
			return modelcatalog.Model{}, false, err
		}
		displayed = true
	}

	for {
		prompt := label + " (search term, exact ID, id:UNKNOWN-ID"
		if optional {
			prompt += ", or - for none"
		}
		prompt += ")"
		if displayed {
			prompt = label + " (number, exact ID, id:UNKNOWN-ID, search, n, p"
			if optional {
				prompt += ", or -"
			}
			prompt += ")"
		}
		value, err := promptLineNoReadAhead(r, w, prompt, "")
		if err != nil {
			return modelcatalog.Model{}, false, err
		}
		if optional && value == "-" {
			return modelcatalog.Model{}, false, nil
		}
		if value == "" {
			if _, err := fmt.Fprintln(w, "A model selection is required."); err != nil {
				return modelcatalog.Model{}, false, err
			}
			continue
		}
		for _, model := range models {
			if model.ID == value {
				return model, true, nil
			}
		}
		if exactID, ok := strings.CutPrefix(value, "id:"); ok {
			if exactID == "" {
				if _, err := fmt.Fprintln(w, "The id: exact-ID form requires a model ID."); err != nil {
					return modelcatalog.Model{}, false, err
				}
				continue
			}
			return modelcatalog.Model{ID: exactID}, true, nil
		}
		if number, parseErr := strconv.Atoi(value); parseErr == nil {
			start := page * cataloguePageSize
			if displayed && number >= 1 && start+number <= len(current) && number <= cataloguePageSize {
				return current[start+number-1], true, nil
			}
			if _, err := fmt.Fprintln(w, "Select a number shown on the current page."); err != nil {
				return modelcatalog.Model{}, false, err
			}
			continue
		}
		switch strings.ToLower(value) {
		case "n", "next":
			if !displayed || (page+1)*cataloguePageSize >= len(current) {
				if _, err := fmt.Fprintln(w, "There is no next page."); err != nil {
					return modelcatalog.Model{}, false, err
				}
				continue
			}
			page++
			if _, err := renderCataloguePage(w, current, page, private...); err != nil {
				return modelcatalog.Model{}, false, err
			}
			continue
		case "p", "previous":
			if !displayed || page == 0 {
				if _, err := fmt.Fprintln(w, "There is no previous page."); err != nil {
					return modelcatalog.Model{}, false, err
				}
				continue
			}
			page--
			if _, err := renderCataloguePage(w, current, page, private...); err != nil {
				return modelcatalog.Model{}, false, err
			}
			continue
		}

		matches := modelcatalog.Search(models, value)
		if len(matches) == 0 {
			if _, err := fmt.Fprintf(w, "Using exact model ID %s.\n", safeCatalogueText(value, private...)); err != nil {
				return modelcatalog.Model{}, false, err
			}
			return modelcatalog.Model{ID: value}, true, nil
		}
		current = matches
		page = 0
		if _, err := renderCataloguePage(w, current, page, private...); err != nil {
			return modelcatalog.Model{}, false, err
		}
		displayed = true
	}
}

func selectFavouriteModels(r io.Reader, w io.Writer, models []modelcatalog.Model, existing map[string]struct{}, private ...string) (map[string]config.ModelTarget, string, string, error) {
	favourites := make(map[string]config.ModelTarget)
	usedAliases := make(map[string]struct{}, len(existing))
	for alias := range existing {
		usedAliases[alias] = struct{}{}
	}
	upstreamAliases := make(map[string]string)

	addFavourite := func(model modelcatalog.Model) (string, error) {
		if alias, ok := upstreamAliases[model.ID]; ok {
			return alias, nil
		}
		alias, aliasErr := modelcatalog.AliasFor(model.ID)
		if aliasErr == nil {
			if _, collision := usedAliases[alias]; !collision {
				if _, err := fmt.Fprintf(w, "Using model alias %s for %s.\n", safeCatalogueText(alias, private...), safeCatalogueText(model.ID, private...)); err != nil {
					return "", err
				}
				favourites[alias] = config.ModelTarget{Model: model.ID}
				usedAliases[alias] = struct{}{}
				upstreamAliases[model.ID] = alias
				return alias, nil
			}
			if _, err := fmt.Fprintf(w, "model alias %q already exists; choose an explicit alias.\n", safeCatalogueText(alias, private...)); err != nil {
				return "", err
			}
		} else {
			if _, err := fmt.Fprintf(w, "%s; choose an explicit alias.\n", safeCatalogueText(aliasErr.Error(), private...)); err != nil {
				return "", err
			}
		}
		for {
			explicit, err := promptLineNoReadAhead(r, w, "Model alias for "+safeCatalogueText(model.ID, private...), "")
			if err != nil {
				return "", err
			}
			if !config.ValidModelAlias(explicit) {
				if _, err := fmt.Fprintf(w, "invalid model alias %q (want slug [a-z0-9-] max %d).\n", safeCatalogueText(explicit, private...), config.ProviderConnectionNameMax); err != nil {
					return "", err
				}
				continue
			}
			if _, collision := usedAliases[explicit]; collision {
				if _, err := fmt.Fprintf(w, "model alias %q already exists.\n", safeCatalogueText(explicit, private...)); err != nil {
					return "", err
				}
				continue
			}
			favourites[explicit] = config.ModelTarget{Model: model.ID}
			usedAliases[explicit] = struct{}{}
			upstreamAliases[model.ID] = explicit
			return explicit, nil
		}
	}

	defaultSelection, selected, err := selectCatalogueModel(r, w, "Default model", models, true, private...)
	if err != nil {
		return nil, "", "", err
	}
	defaultAlias := ""
	if selected {
		defaultAlias, err = addFavourite(defaultSelection)
		if err != nil {
			return nil, "", "", err
		}
	}

	utilitySelection, selected, err := selectCatalogueModel(r, w, "Utility model (- uses default)", models, true, private...)
	if err != nil {
		return nil, "", "", err
	}
	utilityAlias := ""
	if selected {
		utilityAlias, err = addFavourite(utilitySelection)
		if err != nil {
			return nil, "", "", err
		}
	}

	if len(favourites) == 0 {
		if _, err := fmt.Fprintln(w, "Select at least one favourite model for this provider."); err != nil {
			return nil, "", "", err
		}
		selection, _, selectionErr := selectCatalogueModel(r, w, "Favourite model", models, false, private...)
		if selectionErr != nil {
			return nil, "", "", selectionErr
		}
		if _, err := addFavourite(selection); err != nil {
			return nil, "", "", err
		}
	}

	for {
		answer, err := promptLineNoReadAhead(r, w, "Add another favourite? [y/N]", "")
		if err != nil {
			return nil, "", "", err
		}
		switch strings.ToLower(answer) {
		case "", "n", "no":
			return favourites, defaultAlias, utilityAlias, nil
		case "y", "yes":
			selection, _, selectionErr := selectCatalogueModel(r, w, "Favourite model", models, false, private...)
			if selectionErr != nil {
				return nil, "", "", selectionErr
			}
			if _, err := addFavourite(selection); err != nil {
				return nil, "", "", err
			}
		default:
			if _, err := fmt.Fprintln(w, "Enter y or n."); err != nil {
				return nil, "", "", err
			}
		}
	}
}

func safeCatalogueText(value string, private ...string) string {
	return modelcatalog.SafeText(modelcatalog.RedactText(value, private...))
}

func resolveProviderPreset(kind, override string) (providerPreset, error) {
	preset, err := providerconfig.ResolvePreset(kind, override)
	if err != nil {
		return providerPreset{}, err
	}
	return providerPreset{Name: preset.Name, RuntimeType: preset.RuntimeType, StoredBaseURL: preset.BaseURL}, nil
}

func effectiveCatalogConnection(name string, connection config.ProviderConnection, scopeID string) (modelcatalog.Connection, bool, error) {
	if !config.ValidProviderConnectionName(name) {
		return modelcatalog.Connection{}, false, fmt.Errorf("invalid connection name %q", name)
	}

	runtimeType := strings.ToLower(strings.TrimSpace(connection.Type))
	baseURL := strings.TrimSpace(connection.BaseURL)
	switch runtimeType {
	case "openai":
		if baseURL == "" {
			baseURL = openaip.DefaultBaseURL
		}
	case "anthropic":
		if baseURL == "" {
			baseURL = anthropicp.DefaultBaseURL
		}
	default:
		return modelcatalog.Connection{}, false, fmt.Errorf("provider connection %q has unsupported type %q", name, connection.Type)
	}

	baseURL, err := normalizeProviderBaseURL(baseURL)
	if err != nil {
		return modelcatalog.Connection{}, false, fmt.Errorf("provider connection %q base URL: %w", name, err)
	}
	catalogConnection := modelcatalog.Connection{
		Name:    name,
		Type:    runtimeType,
		BaseURL: baseURL,
		ScopeID: scopeID,
	}
	return catalogConnection, runtimeType == "openai" && isOpenRouterBaseURL(baseURL), nil
}

func newCatalogueSource(connection modelcatalog.Connection, apiKey string, openRouter bool) (modelcatalog.Source, error) {
	switch connection.Type {
	case "openai":
		return openaip.NewCatalog(apiKey, connection.BaseURL, openRouter), nil
	case "anthropic":
		return anthropicp.NewCatalog(apiKey, connection.BaseURL), nil
	default:
		return nil, fmt.Errorf("unsupported model catalogue provider type %q", connection.Type)
	}
}

func defaultProviderCatalogue() (providerCatalogue, error) {
	home, err := config.Home()
	if err != nil {
		return nil, fmt.Errorf("resolve Waffle home for model catalogue: %w", err)
	}
	return &providerCatalogueService{
		store:     modelcatalog.NewStore(home),
		newSource: newCatalogueSource,
		now:       time.Now,
	}, nil
}

func (s *providerCatalogueService) Discover(ctx context.Context, connection modelcatalog.Connection, apiKey string) (modelcatalog.Result, error) {
	source, err := s.source(connection, apiKey)
	if err != nil {
		return modelcatalog.Result{}, redactProviderError(err, apiKey)
	}
	models, err := source.ListModels(ctx)
	if err != nil {
		return modelcatalog.Result{}, redactProviderError(err, apiKey)
	}
	models, err = modelcatalog.Normalize(models)
	if err != nil {
		return modelcatalog.Result{}, redactProviderError(fmt.Errorf("normalize discovered model catalogue: %w", err), apiKey)
	}
	return modelcatalog.Result{Record: modelcatalog.Record{
		SchemaVersion: modelcatalog.SchemaVersion,
		Connection:    connection,
		FetchedAt:     s.clock()(),
		Models:        models,
	}}, nil
}

func (s *providerCatalogueService) Models(ctx context.Context, connection modelcatalog.Connection, apiKey string, force bool) (modelcatalog.Result, error) {
	if strings.TrimSpace(connection.ScopeID) == "" {
		return modelcatalog.Result{}, errors.New("model catalogue connection scope ID is empty")
	}
	if s == nil || s.store == nil {
		return modelcatalog.Result{}, errors.New("model catalogue store is not configured")
	}
	source, err := s.source(connection, apiKey)
	if err != nil {
		return modelcatalog.Result{}, redactProviderError(err, apiKey)
	}
	result, err := s.store.GetOrRefresh(ctx, connection, force, source.ListModels)
	if err != nil {
		return modelcatalog.Result{}, redactProviderError(err, apiKey)
	}
	return result, nil
}

func (s *providerCatalogueService) Save(connection modelcatalog.Connection, models []modelcatalog.Model, fetchedAt time.Time) error {
	if strings.TrimSpace(connection.ScopeID) == "" {
		return errors.New("model catalogue connection scope ID is empty")
	}
	if s == nil || s.store == nil {
		return errors.New("model catalogue store is not configured")
	}
	return s.store.Save(connection, models, fetchedAt)
}

func (s *providerCatalogueService) Invalidate(connection string) error {
	if s == nil || s.store == nil {
		return errors.New("model catalogue store is not configured")
	}
	return s.store.Invalidate(connection)
}

func (s *providerCatalogueService) source(connection modelcatalog.Connection, apiKey string) (modelcatalog.Source, error) {
	factory := newCatalogueSource
	if s != nil && s.newSource != nil {
		factory = s.newSource
	}
	return factory(connection, apiKey, connection.Type == "openai" && isOpenRouterBaseURL(connection.BaseURL))
}

func (s *providerCatalogueService) clock() func() time.Time {
	if s != nil && s.now != nil {
		return s.now
	}
	return time.Now
}

func normalizeProviderBaseURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", errors.New("must be a valid absolute HTTP(S) URL")
	}
	if !parsed.IsAbs() || parsed.Hostname() == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil {
		return "", errors.New("must not contain userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", errors.New("must not contain a query")
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", errors.New("must not contain a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func isOpenRouterBaseURL(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	return hostname == "openrouter.ai" || strings.HasSuffix(hostname, ".openrouter.ai")
}
