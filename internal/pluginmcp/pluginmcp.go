// Package pluginmcp maps a validated portable mcp.json entry (internal/plugin)
// onto the runtime MCP client (internal/mcp) without weakening waffle's
// security posture (#391).
//
// The portable surface carries no waffle policy — no execution, egress,
// groups, or token. Those stay operator-controlled: the mapping produces a
// runtime mcp.Server/HTTPOpts whose environment is BuildProcessEnv-limited
// plus the plugin's explicit env object, whose cwd defaults to the plugin
// root, and whose HTTP headers are fixed package data. Waffle policy
// (broker egress for docker-mode groups, deny-by-default unattended tiers,
// credentials from the secret store) is applied by the caller at connect
// time, exactly as it is for native [[mcp]] servers.
package pluginmcp

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/matt-riley/waffle/internal/mcp"
	"github.com/matt-riley/waffle/internal/plugin"
)

// MapStdio resolves a validated portable stdio entry into a runtime
// mcp.Server. A bare command stays bare (platform search at launch); a
// ./-relative command is resolved within the plugin root, and the working
// directory defaults to the resolved plugin root. ./ cwd values are
// resolved with full §4.1 containment. ${PLUGIN_ROOT}/${PLUGIN_DATA}
// placeholders in args/env/cwd are expanded at launch time by the
// PLUGIN_ROOT/PLUGIN_DATA runtime (issue #392) and pass through untouched
// here.
func MapStdio(s plugin.MCPServer, root string) (mcp.Server, error) {
	// The default cwd is the filesystem-resolved plugin root (spec §9.1:
	// PLUGIN_ROOT is the absolute resolved path).
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return mcp.Server{}, fmt.Errorf("mcp server %q: resolve plugin root: %w", s.Name, err)
	}
	command := s.Command
	cwd := resolvedRoot
	if strings.HasPrefix(command, "./") {
		resolved, err := plugin.ResolveWithin(root, command)
		if err != nil {
			return mcp.Server{}, fmt.Errorf("mcp server %q: resolve command: %w", s.Name, err)
		}
		command = resolved
	} else if strings.Contains(command, "/") {
		// Defense in depth: LoadMCP already rejects these forms; the mapping
		// boundary refuses absolute, ../, and non-./ relative paths too.
		return mcp.Server{}, fmt.Errorf("mcp server %q: command %q must be a bare name or a ./-relative plugin path", s.Name, command)
	}
	switch {
	case s.Cwd == "":
		// Default: the plugin root (§7.2.1).
	case strings.HasPrefix(s.Cwd, "./"):
		resolved, err := plugin.ResolveWithin(root, s.Cwd)
		if err != nil {
			return mcp.Server{}, fmt.Errorf("mcp server %q: resolve cwd: %w", s.Name, err)
		}
		cwd = resolved
	default:
		// ${PLUGIN_ROOT}/${PLUGIN_DATA} forms expand at launch (#392).
		cwd = s.Cwd
	}
	return mcp.Server{
		Name:    s.Name,
		Command: command,
		Args:    s.Args,
		EnvVars: s.Env,
		Cwd:     cwd,
	}, nil
}

// MapHTTP resolves a validated portable streamable-http entry into its
// endpoint URL and mcp.HTTPOpts. Fixed headers from the entry are applied
// by the runtime, with client-generated headers taking precedence. Egress
// and credentials are decided by the caller at connect time, exactly as for
// native url servers.
func MapHTTP(s plugin.MCPServer) (string, mcp.HTTPOpts, error) {
	if s.Type != plugin.MCPTypeStreamableHTTP {
		return "", mcp.HTTPOpts{}, fmt.Errorf("mcp server %q: %s is not a streamable-http entry", s.Name, s.Type)
	}
	opts := mcp.HTTPOpts{}
	if len(s.Headers) > 0 {
		opts.Headers = make(http.Header, len(s.Headers))
		for name, value := range s.Headers {
			opts.Headers[name] = []string{value}
		}
	}
	return s.URL, opts, nil
}
