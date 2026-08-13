package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// MCPSchemaID is the canonical $schema identifier for the Agent Plugins
// 1.0.0 MCP configuration (spec §7.2.1, §10).
const MCPSchemaID = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"

// supportedMCPSchemas maps canonical mcp.json $schema identifiers to the
// spec version each implements. Selection is local, like the plugin.json
// map: schema contents are pinned and never fetched.
var supportedMCPSchemas = map[string]string{
	MCPSchemaID: "1.0.0",
}

// MCP transports from spec §7.2.1.
const (
	MCPTypeStdio          = "stdio"
	MCPTypeStreamableHTTP = "streamable-http"
	MCPTypeSSE            = "sse"
)

// MCPServer is one validated portable mcp.json server entry (spec §7.2.1),
// exactly matching one of the closed variants: stdio (Command/Args/Env/Cwd)
// or streamable-http/sse (URL/Headers). It carries no waffle policy — the
// portable surface has no notion of execution, egress, groups, or token;
// those stay enforceable at the mapping/connect layer.
type MCPServer struct {
	Name    string
	Type    string
	Command string
	Args    []string
	Env     map[string]string
	Cwd     string
	URL     string
	Headers map[string]string
}

// MCPSkip reports one skipped server entry (§7.2.2 rule 3/4).
type MCPSkip struct {
	Name   string
	Reason string
}

// MCPResult is the outcome of loading mcp.json. When Disabled is non-empty,
// MCP is disabled for the whole plugin (§7.2.2 rule 2) while other component
// types continue; Skips are individual invalid entries (§7.2.2 rule 3).
type MCPResult struct {
	Servers  []MCPServer
	Skips    []MCPSkip
	Disabled string
}

// LoadMCP reads and validates <root>/mcp.json (spec §7.2.1). A missing
// mcp.json is not an error and yields no servers. A present-but-invalid top
// level — bad JSON, non-object, unknown top-level field, missing or
// unsupported $schema, a version that differs from the plugin's, or a
// non-object mcpServers — disables MCP for the plugin (§7.2.2 rule 2) with
// the reason in MCPResult.Disabled. An invalid individual server entry is
// skipped with a report while valid entries still load (§7.2.2 rule 3).
// pluginVersion is the Agent Plugins version resolved from plugin.json
// (e.g. "1.0.0"); when non-empty, mcp.json must declare the same version.
func LoadMCP(root string, pluginVersion string) (MCPResult, error) {
	resolvedRoot, err := resolvedDir(root)
	if err != nil {
		return MCPResult{}, err
	}
	path := filepath.Join(resolvedRoot, "mcp.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return MCPResult{}, nil
	}
	if err != nil {
		return MCPResult{}, fmt.Errorf("read mcp.json: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		// §6.2: a fixed location of the wrong kind invalidates the component
		// type (and only that type).
		return MCPResult{Disabled: "mcp.json is not a regular file"}, nil
	}
	body, err := readBoundedFile(path, maxManifestSize)
	if err != nil {
		return MCPResult{}, fmt.Errorf("read mcp.json: %w", err)
	}
	return parseMCP(body, pluginVersion)
}

// parseMCP decodes and validates the mcp.json document against the closed
// top-level schema and per-variant server schemas.
func parseMCP(body []byte, pluginVersion string) (MCPResult, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var top map[string]json.RawMessage
	if err := decoder.Decode(&top); err != nil {
		return MCPResult{Disabled: "mcp.json is not a JSON object"}, nil
	}
	if top == nil {
		return MCPResult{Disabled: "mcp.json is not a JSON object"}, nil
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return MCPResult{Disabled: "trailing data after the mcp.json object"}, nil
	}
	// §7.2.1: no other top-level fields are permitted — unlike plugin.json,
	// unknown fields here disable MCP rather than being ignored.
	for _, key := range sortedKeys(top) {
		if key != "$schema" && key != "mcpServers" {
			return MCPResult{Disabled: fmt.Sprintf("unknown top-level field %q in mcp.json", key)}, nil
		}
	}

	schemaRaw, ok := top["$schema"]
	if !ok {
		return MCPResult{Disabled: "mcp.json missing required $schema"}, nil
	}
	schemaID, err := jsonString(schemaRaw, "$schema")
	if err != nil {
		return MCPResult{Disabled: err.Error()}, nil
	}
	version, supported := supportedMCPSchemas[schemaID]
	if !supported {
		return MCPResult{Disabled: schemaRejection(schemaID).Error()}, nil
	}
	if pluginVersion != "" && version != pluginVersion {
		return MCPResult{Disabled: fmt.Sprintf(
			"mcp.json declares Agent Plugins version %s but plugin.json declares %s (§10 mismatch)",
			version, pluginVersion)}, nil
	}

	serversRaw, ok := top["mcpServers"]
	if !ok {
		return MCPResult{Disabled: "mcp.json missing required mcpServers"}, nil
	}
	if !jsonKind(serversRaw, '{') {
		return MCPResult{Disabled: "mcpServers must be an object"}, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &entries); err != nil {
		return MCPResult{Disabled: "mcpServers must be an object"}, nil
	}
	var result MCPResult
	for _, name := range sortedKeys(entries) {
		server, reason := parseMCPServer(name, entries[name])
		if reason != "" {
			result.Skips = append(result.Skips, MCPSkip{Name: name, Reason: reason})
			continue
		}
		result.Servers = append(result.Servers, server)
	}
	return result, nil
}

// parseMCPServer validates one server entry against its declared variant's
// closed schema (§7.2.1). A non-empty reason means the entry is invalid and
// must be skipped (§7.2.2 rule 3).
func parseMCPServer(name string, raw json.RawMessage) (MCPServer, string) {
	skip := func(format string, args ...any) (MCPServer, string) {
		return MCPServer{}, fmt.Sprintf("mcp server %q: %s", name, fmt.Sprintf(format, args...))
	}
	if !jsonKind(raw, '{') {
		return skip("server entry must be an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return skip("server entry must be an object")
	}

	typeRaw, ok := fields["type"]
	if !ok {
		return skip("missing required %q field", "type")
	}
	serverType, err := jsonString(typeRaw, "type")
	if err != nil {
		return skip("field %q must be a string", "type")
	}

	var server MCPServer
	server.Name = name
	server.Type = serverType
	switch serverType {
	case MCPTypeStdio:
		return parseMCPServerStdio(name, fields, server)
	case MCPTypeStreamableHTTP, MCPTypeSSE:
		server, reason := parseMCPRemote(name, fields, server)
		if reason != "" {
			return skip("%s", reason)
		}
		if serverType == MCPTypeSSE {
			// §7.2.2 rule 4: an unsupported transport is skippable; waffle
			// deliberately does not implement the deprecated HTTP+SSE
			// transport (issue #391 design question).
			return skip("transport %q is not supported", MCPTypeSSE)
		}
		return server, ""
	default:
		return skip("unknown type %q", serverType)
	}
}

// stdioServerFields and remoteServerFields are the closed per-variant field
// sets (§7.2.1): a field belonging to another variant invalidates the entry.
var (
	stdioServerFields  = map[string]bool{"type": true, "command": true, "args": true, "env": true, "cwd": true}
	remoteServerFields = map[string]bool{"type": true, "url": true, "headers": true}
)

func parseMCPServerStdio(name string, fields map[string]json.RawMessage, server MCPServer) (MCPServer, string) {
	skip := func(format string, args ...any) (MCPServer, string) {
		return MCPServer{}, fmt.Sprintf("mcp server %q: %s", name, fmt.Sprintf(format, args...))
	}
	for _, key := range sortedKeys(fields) {
		if !stdioServerFields[key] {
			return skip("field %q is not permitted on a stdio server", key)
		}
	}
	commandRaw, ok := fields["command"]
	if !ok {
		return skip("stdio server requires %q", "command")
	}
	command, err := jsonString(commandRaw, "command")
	if err != nil {
		return skip("field %q must be a string", "command")
	}
	if !validCommandForm(command) {
		return skip("command %q must be a bare executable name or a ./-relative plugin path, as a single token", command)
	}
	if raw, ok := fields["args"]; ok {
		args, err := jsonStringSlice(raw, "args")
		if err != nil {
			return skip("%s", err)
		}
		server.Args = args
	}
	if raw, ok := fields["env"]; ok {
		env, reason := parseStringObject(raw, "env")
		if reason != "" {
			return skip("%s", reason)
		}
		for key := range env {
			if key == "PLUGIN_ROOT" || key == "PLUGIN_DATA" {
				// Spec §9.1: these are reserved; the client supplies them
				// itself after the overlay. Such an entry invalidates the
				// server config.
				return skip("env must not define reserved variable %q", key)
			}
		}
		server.Env = env
	}
	if raw, ok := fields["cwd"]; ok {
		cwd, err := jsonString(raw, "cwd")
		if err != nil {
			return skip("field %q must be a string", "cwd")
		}
		if !validCwdForm(cwd) {
			return skip("cwd %q must be ./-relative, ${PLUGIN_ROOT}..., or ${PLUGIN_DATA}...", cwd)
		}
		server.Cwd = cwd
	}
	server.Command = command
	return server, ""
}

func parseMCPRemote(name string, fields map[string]json.RawMessage, server MCPServer) (MCPServer, string) {
	skip := func(format string, args ...any) (MCPServer, string) {
		return MCPServer{}, fmt.Sprintf("mcp server %q: %s", name, fmt.Sprintf(format, args...))
	}
	for _, key := range sortedKeys(fields) {
		if !remoteServerFields[key] {
			return skip("field %q is not permitted on a remote server", key)
		}
	}
	urlRaw, ok := fields["url"]
	if !ok {
		return skip("remote server requires %q", "url")
	}
	rawURL, err := jsonString(urlRaw, "url")
	if err != nil {
		return skip("field %q must be a string", "url")
	}
	if !validRemoteURL(rawURL) {
		return skip("url %q must be absolute http(s) without userinfo or fragment; non-loopback must use https", rawURL)
	}
	if raw, ok := fields["headers"]; ok {
		headers, reason := parseStringObject(raw, "headers")
		if reason != "" {
			return skip("%s", reason)
		}
		for headerName := range headers {
			if !validHeaderName(headerName) || strings.ContainsAny(headers[headerName], "\r\n") {
				return skip("invalid header %q", headerName)
			}
		}
		if !uniqueHeaderNames(headers) {
			return skip("headers contain duplicate case-insensitive names")
		}
		server.Headers = headers
	}
	server.URL = rawURL
	return server, ""
}

// parseStringObject decodes a JSON object whose every value is a string,
// rejecting null and other types, and enforces simple key hygiene.
func parseStringObject(raw json.RawMessage, field string) (map[string]string, string) {
	if !jsonKind(raw, '{') {
		return nil, fmt.Sprintf("field %q must be an object of strings", field)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Sprintf("field %q must be an object of strings", field)
	}
	out := make(map[string]string, len(values))
	for _, key := range sortedKeys(values) {
		if key == "" || strings.ContainsRune(key, '=') {
			return nil, fmt.Sprintf("invalid key %q in %q", key, field)
		}
		value, err := jsonString(values[key], field+"."+key)
		if err != nil {
			return nil, err.Error()
		}
		out[key] = value
	}
	return out, ""
}

// validCommandForm checks the §7.2.1 command rule statically: a single
// executable token that is either a bare name or a ./-relative path.
// Absolute paths, ../ paths, non-./ relative paths with separators, and
// multi-token shell strings are rejected. Existence is not required (the
// binary may be built by the plugin at runtime); ./-relative resolution and
// containment happen at the mapping layer.
func validCommandForm(command string) bool {
	if command == "" || strings.ContainsAny(command, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(command, "./") {
		return true
	}
	// A bare name has no path separator on any platform; reject both /
	// and \ so a Windows-style "..\\bin\\server" or "C:\\bin\\server"
	// cannot bypass the bare-name-or-./ rule.
	return !strings.ContainsAny(command, "/\\")
}

// validCwdForm checks the §7.2.1 cwd rule: ./-relative, ${PLUGIN_ROOT}...,
// or ${PLUGIN_DATA}... (exact or with a / suffix). The ./-form is checked
// lexically here (no existence requirement); the mapping layer enforces
// full containment via ResolveWithin.
func validCwdForm(cwd string) bool {
	if strings.HasPrefix(cwd, "./") {
		cleaned := path.Clean(cwd)
		return cleaned != ".." && !strings.HasPrefix(cleaned, "../") && !path.IsAbs(cleaned)
	}
	return cwd == "${PLUGIN_ROOT}" || strings.HasPrefix(cwd, "${PLUGIN_ROOT}/") ||
		cwd == "${PLUGIN_DATA}" || strings.HasPrefix(cwd, "${PLUGIN_DATA}/")
}

// validRemoteURL enforces the §7.2.1 url rule: absolute http(s), no
// userinfo or fragment, and http only for localhost/loopback literals.
func validRemoteURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return false
	}
	return true
}

// isLoopbackHost reports whether host is localhost or an IP literal in a
// loopback range (spec §7.2.1: http is only allowed for those). Hand-rolled
// so this package imports no networking packages at all (see
// TestNoNetworkingImports); the recognized forms are the ones clients and
// plugins actually use.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if host == "::1" || host == "0:0:0:0:0:0:0:1" {
		return true
	}
	// 127.0.0.0/8: four dot-separated numeric parts with a leading 127,
	// each octet 0–255 so "127.999.999.999" is not treated as loopback.
	parts := strings.Split(host, ".")
	if len(parts) != 4 || parts[0] != "127" {
		return false
	}
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

// validHeaderName reports whether name is an RFC 7230 token (a valid HTTP
// header field name).
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return false
	}
	return true
}

// uniqueHeaderNames reports whether every header name is unique under
// case-insensitive comparison (§7.2.1: duplicate case-insensitive names are
// invalid).
func uniqueHeaderNames(headers map[string]string) bool {
	seen := make(map[string]struct{}, len(headers))
	for name := range headers {
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}
