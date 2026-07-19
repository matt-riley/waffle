package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm/anthropicp"
	"github.com/matt-riley/waffle/internal/llm/openaip"
	"github.com/matt-riley/waffle/internal/modelcatalog"
)

const openRouterBaseURL = "https://openrouter.ai/api/v1"

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

type catalogueSourceFactory func(modelcatalog.Connection, string, bool) (modelcatalog.Source, error)

type providerCatalogueService struct {
	store     *modelcatalog.Store
	newSource catalogueSourceFactory
	now       func() time.Time
}

func resolveProviderPreset(kind, override string) (providerPreset, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	preset := providerPreset{Name: kind}
	switch kind {
	case "openai":
		preset.RuntimeType = "openai"
	case "anthropic":
		preset.RuntimeType = "anthropic"
	case "openrouter":
		preset.RuntimeType = "openai"
		preset.StoredBaseURL = openRouterBaseURL
	case "openai-compatible":
		preset.RuntimeType = "openai"
		if strings.TrimSpace(override) == "" {
			return providerPreset{}, errors.New("provider preset openai-compatible requires a base URL")
		}
	default:
		return providerPreset{}, fmt.Errorf("unsupported provider preset %q", kind)
	}

	if strings.TrimSpace(override) != "" {
		baseURL, err := normalizeProviderBaseURL(override)
		if err != nil {
			return providerPreset{}, fmt.Errorf("provider preset %q base URL: %w", kind, err)
		}
		preset.StoredBaseURL = baseURL
	}
	return preset, nil
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
	if !parsed.IsAbs() || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
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
