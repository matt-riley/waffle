package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/modelcatalog"
	"github.com/matt-riley/waffle/internal/providerconfig"
	"github.com/matt-riley/waffle/internal/secret"
)

type providerManager interface {
	Preflight(context.Context) error
	Add(context.Context, providerconfig.AddRequest) error
	AddModel(context.Context, providerconfig.AddModelRequest) error
	CatalogSnapshot(context.Context, string) (providerconfig.CatalogSnapshot, error)
	List(context.Context) ([]byte, error)
	Test(context.Context, string) error
	Remove(context.Context, string) error
	ActivateModel(context.Context, string) error
	RemoveModel(context.Context, string, string) error
}

var (
	openProviderManager   = defaultProviderManager
	openProviderCatalogue = defaultProviderCatalogue
	providerSecretReader  = readSecretValue
	providerHealthRetry   = 250 * time.Millisecond
)

func providerCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		providerUsage(stderr)
		return errUsage
	}
	switch args[0] {
	case "add":
		return providerAdd(ctx, args[1:], stdin, stdout, stderr)
	case "list":
		return providerList(ctx, args[1:], stdout)
	case "models":
		return providerModels(ctx, args[1:], stdout, stderr)
	case "test":
		if len(args) != 2 {
			return errors.New("usage: waffle provider test <connection>")
		}
		manager, err := openProviderManager()
		if err != nil {
			return err
		}
		if err := manager.Test(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "provider %s is reachable\n", args[1])
		return nil
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: waffle provider remove <connection>")
		}
		manager, err := openProviderManager()
		if err != nil {
			return err
		}
		if err := manager.Remove(ctx, args[1]); err != nil {
			return err
		}
		catalogue, err := openProviderCatalogue()
		if err != nil {
			fmt.Fprintln(stderr, "warning: provider removed but model catalogue cache could not be invalidated")
			fmt.Fprintf(stdout, "removed provider %s\n", args[1])
			return nil
		}
		if err := catalogue.Invalidate(args[1]); err != nil {
			fmt.Fprintln(stderr, "warning: provider removed but model catalogue cache could not be invalidated")
		}
		fmt.Fprintf(stdout, "removed provider %s\n", args[1])
		return nil
	case "model":
		return providerModelCmd(ctx, args[1:], stdout)
	case "help", "-h", "--help":
		providerUsage(stdout)
		return nil
	default:
		providerUsage(stderr)
		return fmt.Errorf("unknown provider command %q", args[0])
	}
}

func providerUsage(w io.Writer) {
	fmt.Fprint(w, `Manage provider connections and model aliases on this host.

Usage:
  waffle provider add [--name NAME] [--type anthropic|openai] [--base-url URL]
                      [--model ALIAS=UPSTREAM]... [--default ALIAS] [--utility ALIAS]
                      [--api-key-stdin | --api-key-file PATH]
  waffle provider list [--json]
  waffle provider models <connection> [--search QUERY] [--refresh] [--json]
  waffle provider test <connection>
  waffle provider remove <connection>
  waffle provider model add <connection> <upstream-id> [--alias ALIAS] [--default] [--utility]
  waffle provider model activate <alias>
  waffle provider model remove <alias> [--replace-with ALIAS]

With no secret-input option, add prompts without echo. API keys are never
accepted as command-line values.
`)
}

type modelFlag map[string]config.ModelTarget

func (m *modelFlag) String() string { return "" }
func (m *modelFlag) Set(value string) error {
	alias, upstream, ok := strings.Cut(value, "=")
	alias = strings.TrimSpace(alias)
	upstream = strings.TrimSpace(upstream)
	if !ok || alias == "" || upstream == "" {
		return errors.New("model must be ALIAS=UPSTREAM")
	}
	if *m == nil {
		*m = make(modelFlag)
	}
	if _, exists := (*m)[alias]; exists {
		return fmt.Errorf("model alias %q was supplied more than once", alias)
	}
	(*m)[alias] = config.ModelTarget{Model: upstream}
	return nil
}

func providerAdd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	interactive := len(args) == 0
	flags := flag.NewFlagSet("provider add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var name, providerType, baseURL, defaultModel, utilityModel, keyFile string
	var keyStdin, legacyAuthFree bool
	models := modelFlag{}
	flags.StringVar(&name, "name", "", "connection name")
	flags.StringVar(&providerType, "type", "", "provider type")
	flags.StringVar(&baseURL, "base-url", "", "provider base URL")
	flags.Var(&models, "model", "ALIAS=UPSTREAM")
	flags.StringVar(&defaultModel, "default", "", "default model alias")
	flags.StringVar(&utilityModel, "utility", "", "utility model alias")
	flags.BoolVar(&keyStdin, "api-key-stdin", false, "read API key from stdin")
	flags.StringVar(&keyFile, "api-key-file", "", "read API key from a 0600 regular file")
	flags.BoolVar(&legacyAuthFree, "legacy-auth-free", false, "confirm existing legacy provider intentionally needs no credential")
	if err := flags.Parse(args); err != nil {
		return sanitizeFlagError(err)
	}
	if flags.NArg() != 0 {
		return errors.New("provider add does not accept positional arguments")
	}
	if keyStdin && keyFile != "" {
		return errors.New("--api-key-stdin and --api-key-file are mutually exclusive")
	}
	var err error
	if name == "" {
		name, err = promptValue(stdin, stderr, "connection name")
		if err != nil {
			return err
		}
	}
	if providerType == "" {
		providerType, err = promptValue(stdin, stderr, "provider type (anthropic|openai)")
		if err != nil {
			return err
		}
	}
	if interactive {
		baseURL, err = promptOptional(stdin, stderr, "base URL (or - for provider default)")
		if err != nil {
			return err
		}
	}
	if len(models) == 0 {
		value, promptErr := promptValue(stdin, stderr, "models (comma-separated ALIAS=UPSTREAM)")
		if promptErr != nil {
			return promptErr
		}
		for _, model := range strings.Split(value, ",") {
			if err := models.Set(model); err != nil {
				return err
			}
		}
	}
	if interactive {
		defaultModel, err = promptOptional(stdin, stderr, "default model alias (or - to remain Installed)")
		if err != nil {
			return err
		}
		utilityModel, err = promptOptional(stdin, stderr, "utility model alias (or - for none)")
		if err != nil {
			return err
		}
	}

	manager, err := openProviderManager()
	if err != nil {
		return err
	}
	if err := manager.Preflight(ctx); err != nil {
		return err
	}

	var apiKey string
	switch {
	case keyStdin:
		apiKey, err = readAllSecret(stdin)
	case keyFile != "":
		apiKey, err = readModeCheckedKeyFile(keyFile)
	default:
		fmt.Fprint(stderr, "API key (input hidden; leave empty for an auth-free endpoint): ")
		apiKey, err = providerSecretReader(stdin, stderr)
	}
	if err != nil {
		return err
	}

	req := providerconfig.AddRequest{
		ConnectionName: name,
		Connection:     config.ProviderConnection{Type: providerType, BaseURL: baseURL},
		Models:         map[string]config.ModelTarget(models),
		DefaultModel:   defaultModel,
		UtilityModel:   utilityModel,
		APIKey:         apiKey,
		LegacyAuthFree: legacyAuthFree,
	}
	if err := manager.Add(ctx, req); err != nil {
		return redactProviderError(err, apiKey)
	}
	fmt.Fprintf(stdout, "provider %s validated and saved\n", name)
	if defaultModel != "" {
		fmt.Fprintf(stdout, "Waffle is Ready with default model %s\n", defaultModel)
	}
	return nil
}

func providerModelCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) < 2 {
		return errors.New("usage: waffle provider model <add|activate|remove> ...")
	}
	var addRequest providerconfig.AddModelRequest
	if args[0] == "add" {
		var err error
		addRequest, err = parseProviderModelAddArgs(args[1:])
		if err != nil {
			return err
		}
	}
	manager, err := openProviderManager()
	if err != nil {
		return err
	}
	if err := manager.Preflight(ctx); err != nil {
		return err
	}
	switch args[0] {
	case "add":
		if err := manager.AddModel(ctx, addRequest); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "added model alias %s for provider %s\n", addRequest.Alias, addRequest.ConnectionName)
		return nil
	case "activate":
		if len(args) != 2 {
			return errors.New("usage: waffle provider model activate <alias>")
		}
		if err := manager.ActivateModel(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "default model %s activated; Waffle is Ready\n", args[1])
		return nil
	case "remove":
		flags := flag.NewFlagSet("provider model remove", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		var replacement string
		flags.StringVar(&replacement, "replace-with", "", "replacement alias")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("provider model remove does not accept positional arguments after alias")
		}
		if err := manager.RemoveModel(ctx, args[1], replacement); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed model alias %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown provider model command %q", args[0])
	}
}

func parseProviderModelAddArgs(args []string) (providerconfig.AddModelRequest, error) {
	if len(args) < 2 {
		return providerconfig.AddModelRequest{}, errors.New("usage: waffle provider model add <connection> <upstream-id> [--alias ALIAS] [--default] [--utility]")
	}
	request := providerconfig.AddModelRequest{
		ConnectionName: args[0],
		UpstreamModel:  args[1],
	}
	aliasSet := false
	defaultSet := false
	utilitySet := false
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--alias":
			if aliasSet {
				return providerconfig.AddModelRequest{}, errors.New("provider model add option --alias was supplied more than once")
			}
			if i+1 >= len(args) {
				return providerconfig.AddModelRequest{}, errors.New("provider model add option --alias requires a value")
			}
			i++
			request.Alias = args[i]
			aliasSet = true
		case "--default":
			if defaultSet {
				return providerconfig.AddModelRequest{}, errors.New("provider model add option --default was supplied more than once")
			}
			request.Default = true
			defaultSet = true
		case "--utility":
			if utilitySet {
				return providerconfig.AddModelRequest{}, errors.New("provider model add option --utility was supplied more than once")
			}
			request.Utility = true
			utilitySet = true
		default:
			return providerconfig.AddModelRequest{}, fmt.Errorf("unknown provider model add option %q", args[i])
		}
	}
	if !aliasSet {
		alias, err := modelcatalog.AliasFor(request.UpstreamModel)
		if err != nil {
			return providerconfig.AddModelRequest{}, err
		}
		request.Alias = alias
	}
	return request, nil
}

type providerModelsOptions struct {
	connection string
	search     string
	refresh    bool
	json       bool
}

func parseProviderModelsArgs(args []string) (providerModelsOptions, error) {
	var options providerModelsOptions
	searchSet := false
	refreshSet := false
	jsonSet := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--search":
			if searchSet {
				return providerModelsOptions{}, errors.New("provider models option --search was supplied more than once")
			}
			if i+1 >= len(args) {
				return providerModelsOptions{}, errors.New("provider models option --search requires a value")
			}
			i++
			options.search = args[i]
			searchSet = true
		case "--refresh":
			if refreshSet {
				return providerModelsOptions{}, errors.New("provider models option --refresh was supplied more than once")
			}
			options.refresh = true
			refreshSet = true
		case "--json":
			if jsonSet {
				return providerModelsOptions{}, errors.New("provider models option --json was supplied more than once")
			}
			options.json = true
			jsonSet = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return providerModelsOptions{}, fmt.Errorf("unknown provider models option %q", args[i])
			}
			if options.connection != "" {
				return providerModelsOptions{}, fmt.Errorf("unexpected provider models argument %q", args[i])
			}
			options.connection = args[i]
		}
	}
	if options.connection == "" {
		return providerModelsOptions{}, errors.New("usage: waffle provider models <connection> [--search QUERY] [--refresh] [--json]")
	}
	return options, nil
}

func providerModels(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	options, err := parseProviderModelsArgs(args)
	if err != nil {
		return err
	}
	manager, err := openProviderManager()
	if err != nil {
		return err
	}
	snapshot, err := manager.CatalogSnapshot(ctx, options.connection)
	if err != nil {
		return err
	}
	connection, _, err := effectiveCatalogConnection(options.connection, snapshot.Connection, snapshot.ScopeID)
	if err != nil {
		return err
	}
	catalogue, err := openProviderCatalogue()
	if err != nil {
		return err
	}
	result, err := catalogue.Models(ctx, connection, snapshot.APIKey, options.refresh)
	if err != nil {
		return redactCatalogueError(err, snapshot.APIKey, snapshot.ScopeID)
	}
	models := result.Models
	if options.search != "" {
		models = modelcatalog.Search(models, options.search)
	}
	models = redactCatalogueModels(models, snapshot.APIKey, snapshot.ScopeID)
	warning := redactCatalogueText(result.Warning, snapshot.APIKey, snapshot.ScopeID)
	if options.json {
		return json.NewEncoder(stdout).Encode(catalogueOutput{
			Connection: options.connection,
			FetchedAt:  result.FetchedAt,
			AgeSeconds: int64(result.Age / time.Second),
			Stale:      result.Stale,
			Warning:    warning,
			Models:     models,
		})
	}
	if warning != "" {
		fmt.Fprintf(stderr, "warning: %s\n", modelcatalog.SafeText(warning))
	}
	for _, model := range models {
		capabilities := make([]string, len(model.Capabilities))
		for i, capability := range model.Capabilities {
			capabilities[i] = modelcatalog.SafeText(capability)
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\n",
			modelcatalog.SafeText(model.DisplayName),
			modelcatalog.SafeText(model.ID),
			strings.Join(capabilities, ", "),
		)
	}
	return nil
}

func redactCatalogueModels(models []modelcatalog.Model, private ...string) []modelcatalog.Model {
	redacted := make([]modelcatalog.Model, len(models))
	for i, model := range models {
		model.ID = redactCatalogueText(model.ID, private...)
		model.DisplayName = redactCatalogueText(model.DisplayName, private...)
		model.Owner = redactCatalogueText(model.Owner, private...)
		model.Capabilities = append([]string(nil), model.Capabilities...)
		for j := range model.Capabilities {
			model.Capabilities[j] = redactCatalogueText(model.Capabilities[j], private...)
		}
		redacted[i] = model
	}
	return redacted
}

func redactCatalogueText(value string, private ...string) string {
	for _, privateValue := range private {
		if privateValue != "" {
			value = strings.ReplaceAll(value, privateValue, "[REDACTED]")
		}
	}
	return value
}

func redactCatalogueError(err error, private ...string) error {
	if err == nil {
		return nil
	}
	return errors.New(redactCatalogueText(err.Error(), private...))
}

func providerList(ctx context.Context, args []string, stdout io.Writer) error {
	jsonOutput := false
	for _, arg := range args {
		if arg != "--json" || jsonOutput {
			return fmt.Errorf("unknown provider list option %q", arg)
		}
		jsonOutput = true
	}
	manager, err := openProviderManager()
	if err != nil {
		return err
	}
	b, err := manager.List(ctx)
	if err != nil {
		return err
	}
	if jsonOutput {
		_, err = stdout.Write(b)
		return err
	}
	var listing providerconfig.Listing
	if err := json.Unmarshal(b, &listing); err != nil {
		return fmt.Errorf("decode provider listing: %w", err)
	}
	fmt.Fprintf(stdout, "State: %s\n", titleState(listing.State))
	if listing.DefaultModel != "" {
		fmt.Fprintf(stdout, "Default model: %s\n", listing.DefaultModel)
	}
	for _, name := range sortedProviderNames(listing.Providers) {
		provider := listing.Providers[name]
		fmt.Fprintf(stdout, "%s\t%s", name, provider.Type)
		if provider.BaseURL != "" {
			fmt.Fprintf(stdout, "\t%s", provider.BaseURL)
		}
		fmt.Fprintln(stdout)
	}
	return nil
}

func defaultProviderManager() (providerManager, error) {
	id, err := secret.LoadIdentity()
	if err != nil {
		return nil, err
	}
	manager, err := providerconfig.New(id)
	if err != nil {
		return nil, err
	}
	manager.Probe = probeProviderModel
	manager.Restart = func(ctx context.Context) error { return runSystemctl(ctx, "restart", "waffle.service") }
	manager.Health = providerServiceHealth
	manager.Stop = func(ctx context.Context) error { return runSystemctl(ctx, "stop", "waffle.service") }
	manager.ServiceActive = providerServiceActive
	manager.RestoreService = func(ctx context.Context, wasActive bool) error {
		if wasActive {
			return runSystemctl(ctx, "restart", "waffle.service")
		}
		return runSystemctl(ctx, "stop", "waffle.service")
	}
	return manager, nil
}

func providerServiceActive(ctx context.Context) (bool, error) {
	cmd := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "waffle.service")
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 3 {
		return false, nil
	}
	return false, fmt.Errorf("query waffle.service active state: %w", err)
}

func probeProviderModel(ctx context.Context, target config.ResolvedModel, apiKey string) error {
	cfg := config.Config{
		Providers: map[string]config.ProviderConnection{target.ConnectionName: target.Connection},
		Models: map[string]config.ModelTarget{
			target.Alias: {Provider: target.ConnectionName, Model: target.UpstreamModel, MaxTokens: target.MaxTokens},
		},
	}
	resolver := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		target.Connection.Type: newModelRuntimeResolver(cfg).factories[target.Connection.Type],
	}, func(config.ProviderConnection) (string, func(string) string, error) {
		return apiKey, func(s string) string { return strings.ReplaceAll(s, apiKey, "[REDACTED]") }, nil
	})
	_, err := resolver.Complete(ctx, llm.Request{
		Model:     target.Alias,
		Messages:  []llm.Message{llm.UserText("Reply with OK.")},
		MaxTokens: 8,
	}, nil)
	return redactProviderError(err, apiKey)
}

func providerServiceHealth(ctx context.Context) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	addr := cfg.Gateway.StatusListen
	if addr == "" {
		addr = "127.0.0.1:8422"
	}
	healthCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	url := "http://" + addr + "/healthz"
	var lastErr error
	for {
		if err := providerHealthOnce(healthCtx, url); err == nil {
			return nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(providerHealthRetry)
		select {
		case <-healthCtx.Done():
			timer.Stop()
			return fmt.Errorf("wait for Waffle health: %w: %v", healthCtx.Err(), lastErr)
		case <-timer.C:
		}
	}
}

func providerHealthOnce(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health endpoint returned %s", resp.Status)
	}
	return nil
}

func runSystemctl(ctx context.Context, action, unit string) error {
	output, err := exec.CommandContext(ctx, "systemctl", action, unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", action, unit, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func promptValue(stdin io.Reader, stderr io.Writer, label string) (string, error) {
	fmt.Fprintf(stderr, "%s: ", label)
	var value string
	if _, err := fmt.Fscan(stdin, &value); err != nil {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	return strings.TrimSpace(value), nil
}

func promptOptional(stdin io.Reader, stderr io.Writer, label string) (string, error) {
	value, err := promptValue(stdin, stderr, label)
	if err != nil {
		return "", err
	}
	if value == "-" {
		return "", nil
	}
	return value, nil
}

func readAllSecret(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

const maxProviderKeyBytes = 64 * 1024

func readModeCheckedKeyFile(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return "", errors.New("open API key file")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("API key file %s must be a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("API key file %s must have mode 0600", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxProviderKeyBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxProviderKeyBytes {
		return "", fmt.Errorf("API key file %s is too large (max %d bytes)", path, maxProviderKeyBytes)
	}
	return strings.TrimSpace(string(b)), nil
}

func sanitizeFlagError(err error) error {
	message := err.Error()
	if strings.Contains(message, "api-key") {
		return errors.New("unknown or invalid provider secret-input option; API keys are not accepted as arguments")
	}
	return err
}

func redactProviderError(err error, key string) error {
	if err == nil || key == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "[REDACTED]"))
}

func sortedProviderNames(providers map[string]providerconfig.ProviderSummary) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func titleState(state string) string {
	if state == "" {
		return ""
	}
	return strings.ToUpper(state[:1]) + state[1:]
}
