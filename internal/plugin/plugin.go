// Package plugin loads Agent Plugins 1.0.0 packages
// (https://github.com/agentplugins/agent-plugins-spec), the vendor-neutral
// package format waffle adopts for the two extension tiers it already ships,
// Skills and MCP servers (docs/plan.md, "Extension surfaces", #389).
//
// A plugin is a directory holding a plugin.json manifest at its root (spec
// §4.1, §5.1). This package implements the package model only: manifest
// loading and closed-schema validation (§5.2), $schema version selection
// (§5.2, §10), plugin-name constraints (§5.5), and plugin-root path
// containment (§4.1). Component discovery — skills/ and mcp.json — layers on
// top in later issues.
//
// Validation is hand-rolled and entirely local: the spec forbids retrieving
// a schema while loading a plugin, so this package carries a fixed
// compatibility map and imports no networking packages at all (see
// TestNoNetworkingImports).
//
// Symlink policy: the loader follows §4.1 — symlinks MAY resolve to targets
// inside the plugin root, and any package path resolving outside it is
// rejected. That is deliberately more permissive than
// internal/skillinstall's staging path, which rejects symlinks outright in
// untrusted fetched trees; a plugin fetched from the internet still crosses
// that review/safe-extract boundary, while a locally-authored plugin may
// carry in-root symlinks.
package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SchemaID is the canonical $schema identifier for the Agent Plugins 1.0.0
// manifest (spec §5.2, §10).
const SchemaID = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"

const (
	manifestFile    = "plugin.json"
	maxManifestSize = 1 << 20 // 1 MiB; the manifest is metadata, not content.
)

var (
	// ErrManifestInvalid rejects a plugin for a fatal plugin.json violation
	// (spec §5.2): any schema violation other than an unknown top-level
	// field or a non-object extensions field.
	ErrManifestInvalid = errors.New("invalid plugin manifest")

	// ErrUnsupportedSchema rejects a plugin whose $schema declares an
	// Agent Plugins version waffle does not support (spec §5.2, §10).
	ErrUnsupportedSchema = errors.New("unsupported plugin schema version")
)

// supportedSchemas maps canonical $schema identifiers to the spec version
// each implements. Selection is purely local (§5.2, §10): schema contents
// are pinned by the spec and never fetched over the network. A new
// identifier appears here only when waffle explicitly recognizes an Agent
// Plugins version as compatible.
var supportedSchemas = map[string]string{
	SchemaID: "1.0.0",
}

// permittedFields is the closed set of plugin.json top-level fields (spec
// §5.2). Anything else is reported and ignored.
var permittedFields = map[string]bool{
	"$schema":     true,
	"name":        true,
	"version":     true,
	"description": true,
	"author":      true,
	"homepage":    true,
	"repository":  true,
	"license":     true,
	"keywords":    true,
	"extensions":  true,
}

// Plugin is a loaded plugin package: its filesystem-resolved root and
// validated manifest (spec §4.1).
type Plugin struct {
	Root     string
	Manifest Manifest
	// Warnings carries the non-fatal findings the spec requires clients to
	// report but not act on: unknown top-level fields (§5.2) and a
	// non-object extensions field (§8.1).
	Warnings []string
}

// Manifest mirrors the closed plugin.json schema (spec §5.2–5.4).
type Manifest struct {
	Schema      string
	Name        string
	Version     string
	Description string
	Author      *Author
	Homepage    string
	Repository  string
	License     string
	Keywords    []string
	// Extensions holds the raw object value for each extension namespace.
	// waffle implements no namespaces yet, so per §8.1/§11.1 the contents of
	// every value pass through unvalidated.
	Extensions map[string]json.RawMessage
}

// Author is the optional author object (spec §5.4), closed to the name,
// email, and url string fields.
type Author struct {
	Name  string
	Email string
	URL   string
}

// Load resolves root and then loads and validates its plugin.json (spec
// §5.1) before any component discovery. A returned error rejects the whole
// plugin: callers MUST NOT discover or execute anything from it (§5.2,
// §11.3). Root symlinks are resolved, matching the spec's
// filesystem-resolved plugin root (§4.1).
func Load(root string) (Plugin, error) {
	resolvedRoot, err := resolvedDir(root)
	if err != nil {
		return Plugin{}, err
	}
	manifest, warnings, err := readManifest(filepath.Join(resolvedRoot, manifestFile))
	if err != nil {
		return Plugin{}, err
	}
	return Plugin{Root: resolvedRoot, Manifest: manifest, Warnings: warnings}, nil
}

// resolvedDir returns the filesystem-resolved form of dir, which must exist
// and be a real directory after symlink resolution.
func resolvedDir(dir string) (string, error) {
	if dir == "" || strings.ContainsRune(dir, 0) {
		return "", errors.New("plugin root must be a non-empty path without NUL bytes")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve plugin root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve plugin root: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve plugin root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("plugin root is not a directory")
	}
	return resolved, nil
}

// readManifest reads path with the same bounded discipline as
// internal/skillinstall's stage records: a regular, non-symlink file no
// larger than maxManifestSize holding exactly one JSON value.
func readManifest(path string) (Manifest, []string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, nil, fmt.Errorf("%w: no plugin.json at the plugin root", ErrManifestInvalid)
	}
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("read plugin.json: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Manifest{}, nil, fmt.Errorf("%w: plugin.json is not a regular file", ErrManifestInvalid)
	}
	if info.Size() > maxManifestSize {
		return Manifest{}, nil, fmt.Errorf("%w: plugin.json exceeds %d bytes", ErrManifestInvalid, maxManifestSize)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("read plugin.json: %w", err)
	}
	if int64(len(body)) > maxManifestSize {
		return Manifest{}, nil, fmt.Errorf("%w: plugin.json exceeds %d bytes", ErrManifestInvalid, maxManifestSize)
	}
	return parseManifest(body)
}

// parseManifest validates the closed plugin.json schema in code (§5.2).
// Exactly two violations are non-fatal — unknown top-level fields and a
// non-object extensions field — and are returned as warnings; every other
// violation rejects the plugin and names the offending field.
func parseManifest(body []byte) (Manifest, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		return Manifest{}, nil, fmt.Errorf("%w: plugin.json is not a JSON object: %v", ErrManifestInvalid, err)
	}
	if fields == nil { // JSON null decodes into a nil map without error.
		return Manifest{}, nil, fmt.Errorf("%w: plugin.json is not a JSON object", ErrManifestInvalid)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, nil, fmt.Errorf("%w: trailing data after the plugin.json object", ErrManifestInvalid)
	}

	var warnings []string
	for _, key := range sortedKeys(fields) {
		if permittedFields[key] {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("plugin.json field %q is unknown; ignored", key))
		delete(fields, key)
	}

	var manifest Manifest
	var err error

	schemaRaw, ok := fields["$schema"]
	if !ok {
		return Manifest{}, warnings, fatalField(`$schema`, "is required")
	}
	if manifest.Schema, err = jsonString(schemaRaw, "$schema"); err != nil {
		return Manifest{}, warnings, err
	}
	if _, supported := supportedSchemas[manifest.Schema]; !supported {
		return Manifest{}, warnings, schemaRejection(manifest.Schema)
	}

	nameRaw, ok := fields["name"]
	if !ok {
		return Manifest{}, warnings, fatalField("name", "is required")
	}
	if manifest.Name, err = jsonString(nameRaw, "name"); err != nil {
		return Manifest{}, warnings, err
	}
	if manifest.Name == "" {
		return Manifest{}, warnings, fatalField("name", "is required and must not be empty")
	}
	if !ValidName(manifest.Name) {
		return Manifest{}, warnings, rejectName(manifest.Name)
	}

	if raw, ok := fields["version"]; ok {
		if manifest.Version, err = jsonString(raw, "version"); err != nil {
			return Manifest{}, warnings, err
		}
	}
	if raw, ok := fields["description"]; ok {
		if manifest.Description, err = jsonString(raw, "description"); err != nil {
			return Manifest{}, warnings, err
		}
	}
	if raw, ok := fields["homepage"]; ok {
		if manifest.Homepage, err = jsonString(raw, "homepage"); err != nil {
			return Manifest{}, warnings, err
		}
	}
	if raw, ok := fields["repository"]; ok {
		if manifest.Repository, err = jsonString(raw, "repository"); err != nil {
			return Manifest{}, warnings, err
		}
	}
	if raw, ok := fields["license"]; ok {
		if manifest.License, err = jsonString(raw, "license"); err != nil {
			return Manifest{}, warnings, err
		}
	}
	if raw, ok := fields["keywords"]; ok {
		if manifest.Keywords, err = jsonStringSlice(raw, "keywords"); err != nil {
			return Manifest{}, warnings, err
		}
	}
	if raw, ok := fields["author"]; ok {
		if manifest.Author, err = parseAuthor(raw); err != nil {
			return Manifest{}, warnings, err
		}
	}
	if raw, ok := fields["extensions"]; ok {
		var extensionWarning string
		manifest.Extensions, extensionWarning, err = parseExtensions(raw)
		if err != nil {
			return Manifest{}, warnings, err
		}
		if extensionWarning != "" {
			warnings = append(warnings, extensionWarning)
		}
	}
	return manifest, warnings, nil
}

// parseAuthor decodes the closed author object (§5.4): only the name,
// email, and url string fields are permitted; anything else is fatal.
func parseAuthor(raw json.RawMessage) (*Author, error) {
	if !jsonKind(raw, '{') {
		return nil, fatalField("author", "must be an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fatalField("author", "must be an object")
	}
	author := &Author{}
	for _, key := range sortedKeys(fields) {
		field := "author." + key
		value, err := jsonString(fields[key], field)
		if err != nil {
			return nil, err
		}
		switch key {
		case "name":
			author.Name = value
		case "email":
			author.Email = value
		case "url":
			author.URL = value
		default:
			return nil, fmt.Errorf("%w: field %q is not permitted in author", ErrManifestInvalid, field)
		}
	}
	return author, nil
}

// parseExtensions implements §8.1. A non-object extensions field is
// reported and ignored (non-fatal). Inside an object extensions, every
// member value must itself be an object — §5.2 makes any schema violation
// beyond its two named exceptions fatal — but the contents of the values
// are never validated: waffle implements no extension namespaces yet, and
// unimplemented namespaces are ignored without interpreting their values.
func parseExtensions(raw json.RawMessage) (map[string]json.RawMessage, string, error) {
	if !jsonKind(raw, '{') {
		return nil, `plugin.json field "extensions" is not an object; ignored`, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, "", fatalField("extensions", "must be an object")
	}
	for _, namespace := range sortedKeys(values) {
		if !jsonKind(values[namespace], '{') {
			return nil, "", fatalField("extensions."+namespace, "must be an object")
		}
	}
	return values, "", nil
}

// schemaRejection builds the rejection error for an unsupported $schema,
// naming the declared version when the identifier follows the canonical
// pattern (§5.2: clients SHOULD report the unsupported version).
func schemaRejection(id string) error {
	supported := supportedVersionList()
	if version, ok := declaredSchemaVersion(id); ok {
		return fmt.Errorf("%w: declared Agent Plugins version %s is not supported (supported: %s)",
			ErrUnsupportedSchema, version, supported)
	}
	return fmt.Errorf("%w: unrecognized $schema %q (supported: %s)", ErrUnsupportedSchema, id, supported)
}

// declaredSchemaVersion extracts the spec version from a canonical
// identifier of the form
// https://agent-plugins.org/schemas/<version>/plugin.schema.json.
func declaredSchemaVersion(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, "https://agent-plugins.org/schemas/")
	if !ok {
		return "", false
	}
	version, suffix, found := strings.Cut(rest, "/")
	if !found || suffix != "plugin.schema.json" || version == "" ||
		strings.ContainsAny(version, "/#?") {
		return "", false
	}
	return version, true
}

func supportedVersionList() string {
	versions := make([]string, 0, len(supportedSchemas))
	for _, version := range supportedSchemas {
		versions = append(versions, version)
	}
	slices.Sort(versions)
	return strings.Join(versions, ", ")
}

// jsonString decodes raw as a JSON string, rejecting every other JSON type
// (null included) with an error naming the field.
func jsonString(raw json.RawMessage, field string) (string, error) {
	if !jsonKind(raw, '"') {
		return "", fatalField(field, "must be a string")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fatalField(field, "must be a string")
	}
	return value, nil
}

// jsonStringSlice decodes raw as an array whose every element is a JSON
// string; element violations are reported with a zero-based index.
func jsonStringSlice(raw json.RawMessage, field string) ([]string, error) {
	if !jsonKind(raw, '[') {
		return nil, fatalField(field, "must be an array of strings")
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, fatalField(field, "must be an array of strings")
	}
	values := make([]string, 0, len(elements))
	for index, element := range elements {
		value, err := jsonString(element, fmt.Sprintf("%s[%d]", field, index))
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

// jsonKind reports whether raw begins with the JSON token opener open after
// optional whitespace: '"' for strings, '{' for objects, '[' for arrays.
// null matches none of them, which is what the closed schema requires.
func jsonKind(raw json.RawMessage, open byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == open
}

func fatalField(field, requirement string) error {
	return fmt.Errorf("%w: field %q %s", ErrManifestInvalid, field, requirement)
}

func sortedKeys(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
