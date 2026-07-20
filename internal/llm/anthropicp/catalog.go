package anthropicp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/pagination"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/matt-riley/waffle/internal/modelcatalog"
)

const (
	maxAnthropicCatalogResponseBytes = 16 * 1024 * 1024
	maxAnthropicCatalogPages         = 100
	maxAnthropicCatalogErrorBytes    = 4096
)

var errAnthropicCatalogResponseTooLarge = fmt.Errorf("anthropic catalogue response exceeds %d bytes", maxAnthropicCatalogResponseBytes)

// Catalog lists models from the Anthropic Models API.
type Catalog struct {
	client anthropic.Client
	apiKey string
}

// NewCatalog builds an Anthropic model catalogue source.
func NewCatalog(apiKey, baseURL string) *Catalog {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	client := &http.Client{
		Transport: &boundedCatalogTransport{
			base:     http.DefaultTransport,
			maxBytes: maxAnthropicCatalogResponseBytes,
		},
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Catalog{
		client: anthropic.NewClient(
			option.WithAPIKey(apiKey),
			option.WithBaseURL(baseURL),
			option.WithHTTPClient(client),
		),
		apiKey: apiKey,
	}
}

// ListModels implements modelcatalog.Source.
func (c *Catalog) ListModels(ctx context.Context) ([]modelcatalog.Model, error) {
	page, err := c.client.Models.List(ctx, anthropic.ModelListParams{
		Limit: param.NewOpt(int64(1000)),
	})
	if err != nil {
		return nil, sanitizeAnthropicCatalogError(c.apiKey, err)
	}

	models := make([]modelcatalog.Model, 0)
	for pageNumber := 1; page != nil; pageNumber++ {
		if err := validateAnthropicCatalogPage(page); err != nil {
			return nil, err
		}
		if len(page.Data) > modelcatalog.MaxModels-len(models) {
			return nil, fmt.Errorf("anthropic catalogue has more than %d entries; maximum is %d", modelcatalog.MaxModels, modelcatalog.MaxModels)
		}
		for i, descriptor := range page.Data {
			if descriptorContainsAPIKey(descriptor, c.apiKey) {
				return nil, fmt.Errorf("anthropic catalogue: model %d contains the active API key", len(models)+i)
			}
			models = append(models, modelcatalog.Model{
				ID:            descriptor.ID,
				DisplayName:   descriptor.DisplayName,
				ContextWindow: descriptor.MaxInputTokens,
			})
		}

		if pageNumber == maxAnthropicCatalogPages && anthropicPageHasNext(page) {
			return nil, fmt.Errorf("anthropic catalogue exceeds %d pages", maxAnthropicCatalogPages)
		}
		page, err = page.GetNextPage()
		if err != nil {
			return nil, sanitizeAnthropicCatalogError(c.apiKey, err)
		}
	}

	models, err = modelcatalog.Normalize(models)
	if err != nil {
		return nil, fmt.Errorf("anthropic catalogue: normalize models: %w", err)
	}
	return models, nil
}

func anthropicPageHasNext(page *pagination.Page[anthropic.ModelInfo]) bool {
	return page.HasMore
}

func validateAnthropicCatalogPage(page *pagination.Page[anthropic.ModelInfo]) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(page.RawJSON()), &envelope); err != nil {
		return fmt.Errorf("anthropic catalogue: decode pagination envelope: %w", err)
	}
	data, ok := envelope["data"]
	if !ok || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("anthropic catalogue: response data field is required and must be an array")
	}
	var descriptors []json.RawMessage
	if err := json.Unmarshal(data, &descriptors); err != nil {
		return fmt.Errorf("anthropic catalogue: decode response data array: %w", err)
	}
	hasMoreJSON, ok := envelope["has_more"]
	if !ok || bytes.Equal(bytes.TrimSpace(hasMoreJSON), []byte("null")) {
		return errors.New("anthropic catalogue: response has_more field is required and must be a boolean")
	}
	var hasMore bool
	if err := json.Unmarshal(hasMoreJSON, &hasMore); err != nil {
		return fmt.Errorf("anthropic catalogue: decode response has_more boolean: %w", err)
	}
	if hasMore != page.HasMore {
		return errors.New("anthropic catalogue: response has_more field could not be decoded reliably")
	}
	if hasMore && (len(descriptors) == 0 || strings.TrimSpace(page.LastID) == "") {
		return errors.New("anthropic catalogue: response has_more is true without a usable nonblank last_id cursor and data page")
	}
	return nil
}

func descriptorContainsAPIKey(descriptor anthropic.ModelInfo, apiKey string) bool {
	if apiKey == "" {
		return false
	}
	var raw any
	if err := json.Unmarshal([]byte(descriptor.RawJSON()), &raw); err == nil {
		return valueContains(raw, apiKey)
	}
	return strings.Contains(descriptor.ID, apiKey) ||
		strings.Contains(descriptor.DisplayName, apiKey) ||
		strings.Contains(string(descriptor.Type), apiKey)
}

func valueContains(value any, needle string) bool {
	switch value := value.(type) {
	case string:
		return strings.Contains(value, needle)
	case []any:
		for _, item := range value {
			if valueContains(item, needle) {
				return true
			}
		}
	case map[string]any:
		for _, item := range value {
			if valueContains(item, needle) {
				return true
			}
		}
	}
	return false
}

type boundedCatalogTransport struct {
	base     http.RoundTripper
	maxBytes int64
}

func (t *boundedCatalogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if resp != nil && resp.Body != nil {
		resp.Body = &boundedCatalogBody{body: resp.Body, remaining: t.maxBytes}
	}
	return resp, err
}

type boundedCatalogBody struct {
	body      io.ReadCloser
	remaining int64
}

func (b *boundedCatalogBody) Read(p []byte) (int, error) {
	if b.remaining > 0 {
		if int64(len(p)) > b.remaining {
			p = p[:b.remaining]
		}
		n, err := b.body.Read(p)
		b.remaining -= int64(n)
		if n > 0 && errors.Is(err, io.EOF) {
			err = nil
		}
		return n, err
	}
	var probe [1]byte
	n, err := b.body.Read(probe[:])
	if n > 0 {
		return 0, errAnthropicCatalogResponseTooLarge
	}
	return 0, err
}

func (b *boundedCatalogBody) Close() error {
	return b.body.Close()
}

type anthropicCatalogError struct {
	message string
	match   error
}

func (e *anthropicCatalogError) Error() string { return e.message }
func (e *anthropicCatalogError) Is(target error) bool {
	return e.match != nil && errors.Is(e.match, target)
}

func sanitizeAnthropicCatalogError(apiKey string, err error) error {
	message := err.Error()
	if apiKey != "" {
		message = strings.ReplaceAll(message, apiKey, strings.Repeat("*", len(apiKey)))
	}
	message = "anthropic catalogue: " + modelcatalog.SafeText(message)
	if len(message) > maxAnthropicCatalogErrorBytes {
		message = truncateUTF8(message, maxAnthropicCatalogErrorBytes)
	}
	return &anthropicCatalogError{
		message: message,
		match:   safeAnthropicCatalogMatch(err),
	}
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func safeAnthropicCatalogMatch(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, errAnthropicCatalogResponseTooLarge):
		return errAnthropicCatalogResponseTooLarge
	default:
		return nil
	}
}

var _ modelcatalog.Source = (*Catalog)(nil)
