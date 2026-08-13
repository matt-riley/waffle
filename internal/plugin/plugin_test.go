package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// resolved returns the filesystem-resolved form of path. t.TempDir on macOS
// sits under the /var symlink, so Load's resolved root differs from the
// string t.TempDir reports.
func resolved(t *testing.T, path string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writePluginDir(t *testing.T, root, manifest string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func manifestJSON(t *testing.T, fields map[string]any) string {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func baseFields() map[string]any {
	return map[string]any{"$schema": SchemaID, "name": "test-plugin"}
}

// with returns base fields plus one overridden or added field.
func with(key string, value any) map[string]any {
	fields := baseFields()
	fields[key] = value
	return fields
}

func TestLoadMinimalManifest(t *testing.T) {
	dir := writePluginDir(t, filepath.Join(t.TempDir(), "minimal"),
		manifestJSON(t, map[string]any{"$schema": SchemaID, "name": "minimal-plugin"}))

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := resolved(t, dir); got.Root != want {
		t.Errorf("Root = %q, want %q", got.Root, want)
	}
	if got.Manifest.Schema != SchemaID {
		t.Errorf("Schema = %q, want %q", got.Manifest.Schema, SchemaID)
	}
	if got.Manifest.Name != "minimal-plugin" {
		t.Errorf("Name = %q, want %q", got.Manifest.Name, "minimal-plugin")
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", got.Warnings)
	}
}

func TestLoadFullManifest(t *testing.T) {
	fields := baseFields()
	fields["name"] = "full-plugin"
	fields["version"] = "1.2.0"
	fields["description"] = "A test plugin"
	fields["author"] = map[string]any{
		"name":  "Author Name",
		"email": "author@example.com",
		"url":   "https://example.com",
	}
	fields["homepage"] = "https://docs.example.com/plugin"
	fields["repository"] = "https://github.com/example/plugin"
	fields["license"] = "MIT"
	fields["keywords"] = []string{"alpha", "beta"}
	fields["extensions"] = map[string]any{
		"com.example.client": map[string]any{"setting": true},
	}
	dir := writePluginDir(t, filepath.Join(t.TempDir(), "full"), manifestJSON(t, fields))

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m := got.Manifest
	if m.Name != "full-plugin" || m.Version != "1.2.0" || m.Description != "A test plugin" ||
		m.Homepage != "https://docs.example.com/plugin" ||
		m.Repository != "https://github.com/example/plugin" || m.License != "MIT" {
		t.Errorf("metadata fields wrong: %+v", m)
	}
	if m.Author == nil || m.Author.Name != "Author Name" ||
		m.Author.Email != "author@example.com" || m.Author.URL != "https://example.com" {
		t.Errorf("author = %+v", m.Author)
	}
	if !slices.Equal(m.Keywords, []string{"alpha", "beta"}) {
		t.Errorf("keywords = %v", m.Keywords)
	}
	if len(m.Extensions) != 1 || string(m.Extensions["com.example.client"]) != `{"setting":true}` {
		t.Errorf("extensions = %v", m.Extensions)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", got.Warnings)
	}
}

func TestLoadUnknownFieldsReportedAndIgnored(t *testing.T) {
	fields := baseFields()
	fields["bogus"] = true
	fields["components"] = []string{"x"}
	fields["hooks"] = map[string]any{"run": "echo"}
	dir := writePluginDir(t, filepath.Join(t.TempDir(), "unknown"), manifestJSON(t, fields))

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("unknown top-level fields must not reject the plugin: %v", err)
	}
	want := []string{
		`plugin.json field "bogus" is unknown; ignored`,
		`plugin.json field "components" is unknown; ignored`,
		`plugin.json field "hooks" is unknown; ignored`,
	}
	if !slices.Equal(got.Warnings, want) {
		t.Errorf("Warnings = %q, want %q", got.Warnings, want)
	}
	if got.Manifest.Name != "test-plugin" {
		t.Errorf("Name = %q, want %q", got.Manifest.Name, "test-plugin")
	}
}

func TestLoadFatalViolations(t *testing.T) {
	schema200 := "https://agent-plugins.org/schemas/2.0.0/plugin.schema.json"
	minimal := manifestJSON(t, baseFields())
	noSchema := baseFields()
	delete(noSchema, "$schema")
	noName := baseFields()
	delete(noName, "name")

	cases := []struct {
		name    string
		fields  map[string]any
		raw     string // overrides fields when set
		wantSub string // error must name the offending field or condition
		wantErr error
	}{
		{name: "missing schema", fields: noSchema, wantSub: `"$schema"`, wantErr: ErrManifestInvalid},
		{name: "schema wrong type", fields: with("$schema", 1), wantSub: `"$schema"`, wantErr: ErrManifestInvalid},
		{name: "schema unsupported version", fields: with("$schema", schema200), wantSub: "2.0.0", wantErr: ErrUnsupportedSchema},
		{name: "schema unrecognized", fields: with("$schema", "https://example.com/plugin.schema.json"), wantSub: "https://example.com/plugin.schema.json", wantErr: ErrUnsupportedSchema},
		{name: "missing name", fields: noName, wantSub: `"name"`, wantErr: ErrManifestInvalid},
		{name: "empty name", fields: with("name", ""), wantSub: `"name"`, wantErr: ErrManifestInvalid},
		{name: "name wrong type", fields: with("name", 7), wantSub: `"name"`, wantErr: ErrManifestInvalid},
		{name: "name violates constraints", fields: with("name", "My-Plugin"), wantSub: "My-Plugin", wantErr: ErrInvalidName},
		{name: "version wrong type", fields: with("version", []string{"1"}), wantSub: `"version"`, wantErr: ErrManifestInvalid},
		{name: "description wrong type", fields: with("description", 2), wantSub: `"description"`, wantErr: ErrManifestInvalid},
		{name: "homepage wrong type", fields: with("homepage", false), wantSub: `"homepage"`, wantErr: ErrManifestInvalid},
		{name: "repository wrong type", fields: with("repository", []any{}), wantSub: `"repository"`, wantErr: ErrManifestInvalid},
		{name: "license wrong type", fields: with("license", map[string]any{}), wantSub: `"license"`, wantErr: ErrManifestInvalid},
		{name: "keywords wrong type", fields: with("keywords", "single"), wantSub: `"keywords"`, wantErr: ErrManifestInvalid},
		{name: "keywords element wrong type", fields: with("keywords", []any{"ok", 3}), wantSub: `"keywords[1]"`, wantErr: ErrManifestInvalid},
		{name: "author wrong type", fields: with("author", "Acme"), wantSub: `"author"`, wantErr: ErrManifestInvalid},
		{name: "author subfield not permitted", fields: with("author", map[string]any{"role": "admin"}), wantSub: `"author.role"`, wantErr: ErrManifestInvalid},
		{name: "author subfield wrong type", fields: with("author", map[string]any{"email": 7}), wantSub: `"author.email"`, wantErr: ErrManifestInvalid},
		{name: "extensions member wrong type", fields: with("extensions", map[string]any{"com.example": "nope"}), wantSub: `"extensions.com.example"`, wantErr: ErrManifestInvalid},
		{name: "top-level array", raw: `[1, 2]`, wantSub: "not a JSON object", wantErr: ErrManifestInvalid},
		{name: "top-level string", raw: `"plugin"`, wantSub: "not a JSON object", wantErr: ErrManifestInvalid},
		{name: "top-level null", raw: `null`, wantSub: "not a JSON object", wantErr: ErrManifestInvalid},
		{name: "trailing data", raw: minimal + " {}", wantSub: "trailing data", wantErr: ErrManifestInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := tc.raw
			if manifest == "" {
				manifest = manifestJSON(t, tc.fields)
			}
			dir := writePluginDir(t, filepath.Join(t.TempDir(), "case"), manifest)
			if _, err := Load(dir); err == nil {
				t.Fatalf("Load succeeded, want rejection naming %q", tc.wantSub)
			} else if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not name %q", err, tc.wantSub)
			} else if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(%v, %v) = false", err, tc.wantErr)
			}
		})
	}
}

func TestLoadNonObjectExtensionsReportedAndIgnored(t *testing.T) {
	for _, value := range []any{"a string", []any{"x"}, 3, true, nil} {
		t.Run(fmt.Sprintf("%v", value), func(t *testing.T) {
			dir := writePluginDir(t, filepath.Join(t.TempDir(), "ext"),
				manifestJSON(t, with("extensions", value)))
			got, err := Load(dir)
			if err != nil {
				t.Fatalf("non-object extensions must not reject the plugin: %v", err)
			}
			if got.Manifest.Extensions != nil {
				t.Errorf("Extensions = %v, want dropped", got.Manifest.Extensions)
			}
			want := []string{`plugin.json field "extensions" is not an object; ignored`}
			if !slices.Equal(got.Warnings, want) {
				t.Errorf("Warnings = %q, want %q", got.Warnings, want)
			}
		})
	}
}

func TestLoadExtensionsUnknownNamespacesIgnored(t *testing.T) {
	// waffle implements no extension namespaces yet, so every namespace is
	// ignored without validating the contents of its value (spec §8.1).
	fields := with("extensions", map[string]any{
		"com.example.one": map[string]any{"anything": []any{1, "two", map[string]any{"deep": true}}},
		"org.other":       map[string]any{},
	})
	dir := writePluginDir(t, filepath.Join(t.TempDir(), "ns"), manifestJSON(t, fields))

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Manifest.Extensions) != 2 {
		t.Fatalf("Extensions = %v, want both namespaces kept raw", got.Manifest.Extensions)
	}
	if got.Manifest.Extensions["org.other"] == nil {
		t.Error("org.other namespace dropped")
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", got.Warnings)
	}
}

func TestLoadMissingManifest(t *testing.T) {
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrManifestInvalid) ||
		!strings.Contains(err.Error(), "plugin.json") {
		t.Errorf("Load(empty root) = %v, want ErrManifestInvalid naming plugin.json", err)
	}
}

func TestLoadManifestNotRegularFile(t *testing.T) {
	directoryRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(directoryRoot, "plugin.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(directoryRoot); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory plugin.json = %v, want not-a-regular-file rejection", err)
	}

	real := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(real, []byte(manifestJSON(t, baseFields())), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkRoot := t.TempDir()
	if err := os.Symlink(real, filepath.Join(symlinkRoot, "plugin.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(symlinkRoot); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("symlinked plugin.json = %v, want not-a-regular-file rejection", err)
	}
}

func TestLoadManifestOversized(t *testing.T) {
	root := t.TempDir()
	body := strings.Repeat(" ", maxManifestSize+1)
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized plugin.json = %v, want size rejection", err)
	}
}

func TestLoadResolvesRootSymlink(t *testing.T) {
	real := writePluginDir(t, filepath.Join(t.TempDir(), "real"), manifestJSON(t, baseFields()))
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := Load(link)
	if err != nil {
		t.Fatalf("Load through symlinked root: %v", err)
	}
	if want := resolved(t, real); got.Root != want {
		t.Errorf("Root = %q, want resolved %q", got.Root, want)
	}
}

func TestLoadRootErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing root = %v, want fs.ErrNotExist", err)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file root = %v, want not-a-directory rejection", err)
	}

	if _, err := Load(""); err == nil {
		t.Error("empty root accepted")
	}
}

func TestSchemaSelectionNamesVersions(t *testing.T) {
	dir := writePluginDir(t, filepath.Join(t.TempDir(), "future"),
		manifestJSON(t, with("$schema", "https://agent-plugins.org/schemas/2.0.0/plugin.schema.json")))
	_, err := Load(dir)
	if !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("error = %v, want ErrUnsupportedSchema", err)
	}
	for _, version := range []string{"2.0.0", "1.0.0"} {
		if !strings.Contains(err.Error(), version) {
			t.Errorf("error %q does not name version %s", err, version)
		}
	}
}

// TestNoNetworkingImports enforces the §5.2/§10 no-network rule by
// construction: the transport seam is that no file in this package imports
// a networking package, so Load cannot issue an HTTP request and no test
// can perform network I/O through it.
func TestNoNetworkingImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		checked++
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if importPath == "net" || importPath == "net/http" || strings.HasPrefix(importPath, "net/http/") {
				t.Errorf("%s imports %q: the plugin loader must stay offline", entry.Name(), importPath)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no package sources found to scan")
	}
}
