package openaip

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/modelcatalog"
)

const maxCatalogResponseBytes = 16 * 1024 * 1024

// Catalog lists models from an OpenAI-compatible endpoint.
type Catalog struct {
	APIKey       string
	BaseURL      string
	Client       *http.Client
	UserFiltered bool
}

// NewCatalog builds an OpenAI-compatible model catalogue source.
func NewCatalog(apiKey, baseURL string, userFiltered bool) *Catalog {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Catalog{
		APIKey:  apiKey,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		UserFiltered: userFiltered,
	}
}

// ListModels implements modelcatalog.Source.
func (c *Catalog) ListModels(ctx context.Context) ([]modelcatalog.Model, error) {
	if c.UserFiltered {
		models, status, err := c.listModels(ctx, "/models/user")
		if err == nil || status != http.StatusNotFound {
			return models, err
		}
	}
	models, _, err := c.listModels(ctx, "/models")
	return models, err
}

type catalogResponse struct {
	Data []catalogModel `json:"data"`
}

type catalogModel struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	OwnedBy             string   `json:"owned_by"`
	ContextLength       int64    `json:"context_length"`
	SupportedParameters []string `json:"supported_parameters"`
	Architecture        struct {
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
}

func (c *Catalog) listModels(ctx context.Context, path string) ([]modelcatalog.Model, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("openai catalogue: create request: %w", err)
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("openai catalogue: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		bodyText := string(body)
		if c.APIKey != "" {
			bodyText = strings.ReplaceAll(bodyText, c.APIKey, strings.Repeat("*", len(c.APIKey)))
		}
		return nil, resp.StatusCode, fmt.Errorf("openai catalogue: %s: %s", resp.Status, strings.TrimSpace(modelcatalog.SafeText(bodyText)))
	}

	limited := &io.LimitedReader{R: resp.Body, N: maxCatalogResponseBytes + 1}
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("openai catalogue: read response: %w", err)
	}
	if len(body) > maxCatalogResponseBytes {
		return nil, resp.StatusCode, fmt.Errorf("openai catalogue response exceeds %d bytes", maxCatalogResponseBytes)
	}

	var wire catalogResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("openai catalogue: decode model catalogue: %w", err)
	}
	models := make([]modelcatalog.Model, 0, len(wire.Data))
	for i, descriptor := range wire.Data {
		if field, ok := descriptor.secretField(c.APIKey); ok {
			return nil, resp.StatusCode, fmt.Errorf("openai catalogue: model %d %s contains the active API key", i, field)
		}
		models = append(models, descriptor.model())
	}
	models, err = modelcatalog.Normalize(models)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("openai catalogue: normalize models: %w", err)
	}
	return models, resp.StatusCode, nil
}

func (c *Catalog) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

func (m catalogModel) model() modelcatalog.Model {
	model := modelcatalog.Model{
		ID:            m.ID,
		DisplayName:   m.Name,
		Owner:         m.OwnedBy,
		ContextWindow: m.ContextLength,
	}
	for _, parameter := range m.SupportedParameters {
		if parameter == "tools" || parameter == "tool_choice" {
			model.Capabilities = append(model.Capabilities, "tool-calling")
			break
		}
	}
	for _, modality := range m.Architecture.OutputModalities {
		if modality == "text" {
			model.Capabilities = append(model.Capabilities, "text-output")
			break
		}
	}
	return model
}

func (m catalogModel) secretField(apiKey string) (string, bool) {
	if apiKey == "" {
		return "", false
	}
	fields := []struct {
		name  string
		value string
	}{
		{name: "ID", value: m.ID},
		{name: "name", value: m.Name},
		{name: "owner", value: m.OwnedBy},
	}
	for _, parameter := range m.SupportedParameters {
		fields = append(fields, struct {
			name  string
			value string
		}{name: "supported parameter", value: parameter})
	}
	for _, modality := range m.Architecture.OutputModalities {
		fields = append(fields, struct {
			name  string
			value string
		}{name: "output modality", value: modality})
	}
	for _, field := range fields {
		if strings.Contains(field.value, apiKey) {
			return field.name, true
		}
	}
	return "", false
}

var _ modelcatalog.Source = (*Catalog)(nil)
