package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/llm/anthropicp"
	"github.com/matt-riley/waffle/internal/llm/openaip"
	"github.com/matt-riley/waffle/internal/secret"
)

type providerFactory func(apiKey, baseURL string) llm.Provider

type secretResolver func(config.ProviderConnection) (string, func(string) string, error)

// modelRuntimeResolver is the provider boundary for configured model aliases.
// Agents retain stable aliases; only this type substitutes the selected
// connection's upstream model identifier immediately before an API call.
type modelRuntimeResolver struct {
	cfg       config.Config
	factories map[string]providerFactory
	secrets   secretResolver

	mu        sync.Mutex
	clients   map[string]llm.Provider
	redactors map[string]func(string) string
}

func newModelRuntimeResolver(cfg config.Config) *modelRuntimeResolver {
	return newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"anthropic": func(apiKey, baseURL string) llm.Provider {
			return anthropicp.New(apiKey, baseURL)
		},
		"openai": func(apiKey, baseURL string) llm.Provider {
			if baseURL == "" {
				baseURL = "https://api.openai.com/v1"
			}
			return openaip.New(apiKey, baseURL)
		},
	}, resolveProviderConnectionSecret)
}

func newModelRuntimeResolverWith(cfg config.Config, factories map[string]providerFactory, secrets secretResolver) *modelRuntimeResolver {
	return &modelRuntimeResolver{
		cfg:       cfg,
		factories: factories,
		secrets:   secrets,
		clients:   make(map[string]llm.Provider),
		redactors: make(map[string]func(string) string),
	}
}

// Complete implements llm.Provider. It deterministically resolves req.Model
// as an alias, chooses the configured connection, and sends only the upstream
// model identifier to that client.
func (r *modelRuntimeResolver) Complete(ctx context.Context, req llm.Request, onEvent llm.StreamFunc) (*llm.Response, error) {
	provider, target, _, err := r.resolve(req.Model)
	if err != nil {
		return nil, err
	}
	req.Model = target.UpstreamModel
	if req.MaxTokens <= 0 {
		req.MaxTokens = target.MaxTokens
	}
	return provider.Complete(ctx, req, onEvent)
}

func (r *modelRuntimeResolver) resolve(alias string) (llm.Provider, config.ResolvedModel, func(string) string, error) {
	target, err := r.resolveTarget(alias)
	if err != nil {
		return nil, config.ResolvedModel{}, nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if provider := r.clients[target.ConnectionName]; provider != nil {
		return provider, target, r.redactors[target.ConnectionName], nil
	}
	if r.secrets == nil {
		return nil, config.ResolvedModel{}, nil, fmt.Errorf("model alias %q connection %q: no secret resolver configured", alias, target.ConnectionName)
	}
	apiKey, redact, err := r.secrets(target.Connection)
	if err != nil {
		return nil, config.ResolvedModel{}, nil, fmt.Errorf("model alias %q connection %q: resolve credentials: %w", alias, target.ConnectionName, err)
	}
	factory := r.factories[target.Connection.Type]
	if factory == nil {
		return nil, config.ResolvedModel{}, nil, fmt.Errorf("model alias %q connection %q: unsupported provider type %q", alias, target.ConnectionName, target.Connection.Type)
	}
	provider := factory(apiKey, target.Connection.BaseURL)
	if provider == nil {
		return nil, config.ResolvedModel{}, nil, fmt.Errorf("model alias %q connection %q: provider factory returned nil", alias, target.ConnectionName)
	}
	r.clients[target.ConnectionName] = provider
	r.redactors[target.ConnectionName] = redact
	return provider, target, redact, nil
}

func (r *modelRuntimeResolver) resolveTarget(alias string) (config.ResolvedModel, error) {
	if len(r.cfg.Providers) > 0 || len(r.cfg.Models) > 0 {
		target, err := r.cfg.ResolveModel(alias)
		if err == nil {
			if r.cfg.Provider.Name != "" && target.Connection.APIKey == "" {
				target.Connection.APIKey = envKey(target.Connection.Type)
			}
			return target, nil
		}
		if r.cfg.Provider.Name == "" {
			return target, err
		}
		// A normalized legacy registry still permits historical profile model
		// IDs on its single provider connection.
	}
	// Compatibility for callers that construct Config values directly instead
	// of loading a legacy [provider] table through config.Load.
	providerType := r.cfg.Provider.Name
	if providerType == "" {
		providerType = "anthropic"
	}
	upstreamModel := alias
	if upstreamModel == "" {
		upstreamModel = r.cfg.Provider.Model
	}
	apiKey := r.cfg.Provider.APIKey
	if apiKey == "" {
		apiKey = envKey(providerType)
	}
	return config.ResolvedModel{
		Alias:          alias,
		ConnectionName: "default",
		Connection: config.ProviderConnection{
			Type:      providerType,
			APIKey:    apiKey,
			BaseURL:   r.cfg.Provider.BaseURL,
			MaxTokens: r.cfg.Provider.MaxTokens,
		},
		UpstreamModel: upstreamModel,
		MaxTokens:     r.cfg.Provider.MaxTokens,
	}, nil
}

func resolveRuntimeProfileModel(cfg config.Config, profile config.AgentProfile) (string, error) {
	// An explicit registry has no singular provider. Its profile values are
	// aliases, with "default" and "utility" selecting the agent aliases.
	if cfg.Provider.Name == "" && (len(cfg.Providers) > 0 || len(cfg.Models) > 0) {
		alias := strings.TrimSpace(profile.Model)
		switch alias {
		case "", "default":
			alias = cfg.Agent.DefaultModel
		case "utility":
			alias = cfg.Agent.UtilityModel
			if alias == "" {
				return "", fmt.Errorf("profile model %q requires [agent] utility_model to be set", profile.Model)
			}
		}
		if alias == "" {
			return "", fmt.Errorf("agent.default_model is not configured")
		}
		if _, err := cfg.ResolveModel(alias); err != nil {
			return "", err
		}
		return alias, nil
	}
	return cfg.ResolveProfileModel(profile)
}

// redactFor returns the redactor associated with alias's connection. The
// connection is resolved lazily if necessary so a caller never borrows a
// redactor from another provider connection.
func (r *modelRuntimeResolver) redactFor(alias string) func(string) string {
	_, _, redact, err := r.resolve(alias)
	if err != nil || redact == nil {
		return func(s string) string { return s }
	}
	return redact
}

// redact applies every redactor for a client already used by this runtime.
// This is suitable for an Agent's shared transcript boundary: it prevents a
// tool result from carrying any enrolled provider credential into any model.
func (r *modelRuntimeResolver) redact(s string) string {
	r.mu.Lock()
	names := make([]string, 0, len(r.redactors))
	for name := range r.redactors {
		names = append(names, name)
	}
	sort.Strings(names)
	redactors := make([]func(string) string, 0, len(names))
	for _, name := range names {
		if redact := r.redactors[name]; redact != nil {
			redactors = append(redactors, redact)
		}
	}
	r.mu.Unlock()
	for _, redact := range redactors {
		s = redact(s)
	}
	return s
}

func resolveProviderConnectionSecret(connection config.ProviderConnection) (string, func(string) string, error) {
	var key string
	var store secret.Store
	if connection.APIKey != "" {
		var err error
		if strings.HasPrefix(connection.APIKey, "secret://provider/") {
			store, err = secret.TryOpen()
			if err == nil && store == nil {
				err = fmt.Errorf("no secret store is available: run `waffle secret init`")
			}
			if err == nil {
				key, err = secret.Resolve(store, connection.APIKey)
			}
		} else {
			// Legacy singular providers retain their conventional environment
			// fallback. Named connections fail closed above.
			key, err = secret.ResolveRef(connection.APIKey, envName(connection.Type))
		}
		if err != nil {
			return "", nil, err
		}
	}
	if key == "" && secret.IsRef(connection.APIKey) {
		return "", nil, fmt.Errorf("api_key is %q but no secret store is available: run `waffle secret init`, or set %s", connection.APIKey, envName(connection.Type))
	}
	secretName := connection.Type + "/api-key"
	if secret.IsRef(connection.APIKey) {
		secretName = strings.TrimPrefix(connection.APIKey, "secret://")
	}
	if store == nil {
		store, _ = secret.TryOpen()
	}
	redact, _ := secret.RedactorFor(store, secretName, key)
	return key, redact, nil
}

func runtimeUtilityModel(cfg config.Config) string {
	if len(cfg.Providers) > 0 || len(cfg.Models) > 0 {
		return cfg.Agent.UtilityModel
	}
	return cfg.Provider.UtilityModel
}
