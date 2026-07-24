# Guided Dashboard Setup: Skills Onboarding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator discover, review, install inactive, and separately activate one or more skills from a public GitHub URL, bounded zip upload, or labeled host-approved folder without typing a commit hash or server path.

**Architecture:** Extend `internal/skillinstall` with source discovery that resolves GitHub refs to exact commits, finds a root or direct child skills, and stages a chosen candidate through the existing bounded review/install pipeline. Add a separate safe zip reader and sanitized local-root catalogue, expose focused Desk endpoints, and replace raw manifest JSON with an accessible review task.

**Tech Stack:** Go 1.25.12, `net/http`, `archive/zip`, existing `skillinstall` audit/provenance engine, templ, embedded CSS/JavaScript, Node's built-in test runner.

## Global Constraints

- V1 GitHub discovery supports credential-free public HTTPS GitHub repositories only.
- Every Git source is resolved to and installed from an exact lowercase 40-character commit.
- Private skill repositories never reuse provider credentials or repository-scoped workspace credentials implicitly.
- Review remains bounded to 64 files and 1 MiB of reviewed content.
- Upload body, compressed archive, entry count, path length, and expanded content have separate hard limits.
- Reject absolute paths, traversal, symlinks, hard links, devices, sockets, duplicate paths, excessive compression, excessive files, and excessive expanded bytes.
- Installation is always inactive; activation is a separate explicit Waffle-wide action and session attachment remains a separate Today control.
- Hosts retain deny-by-default source policy; Desk receives only capability booleans and sanitized labels, never raw paths.
- Name collisions never overwrite an installed skill.
- Desk security, request-token, idempotency, CSP, no-store, loopback, focus, keyboard, and reduced-motion behavior remain enforced.

---

### Task 1: Add public GitHub URL parsing and exact-commit resolution

**Files:**
- Create: `internal/skillinstall/github.go`
- Create: `internal/skillinstall/github_test.go`
- Modify: `internal/skillinstall/source.go`
- Modify: `internal/skillinstall/manifest.go`
- Modify: `internal/skillinstall/installer.go`
- Modify: `internal/skillinstall/installer_test.go`

**Interfaces:**
- Consumes: the existing no-redirect bounded codeload fetcher and
  `reviewedTreeFromGitHubArchive`.
- Produces:

```go
type GitHubRequest struct {
	URL      string
	Ref      string
	Commit   string
}

type SourceCandidate struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
}

type Discovery struct {
	ID         string            `json:"id"`
	Source     string            `json:"source"`
	Repository string            `json:"repository,omitempty"`
	Commit     string            `json:"commit,omitempty"`
	Candidates []SourceCandidate `json:"candidates"`
}

type GitHubResolver interface {
	Resolve(context.Context, GitHubRequest) (repository, commit, subdir string, err error)
}

func (i *Installer) DiscoverGitHub(context.Context, GitHubRequest) (Discovery, error)
func (i *Installer) StageGitHub(context.Context, Discovery, candidateID string) (Manifest, error)
```

`URL` accepts `https://github.com/<owner>/<repo>` or
`https://github.com/<owner>/<repo>/tree/<ref>/<path>`. A branch containing `/`
must use the repository URL plus the separate advanced `Ref` field so URL
parsing is unambiguous. `Commit`, when supplied, must be a full commit and
takes precedence over `Ref`. Default `Ref` is the repository's default branch.

- [ ] **Step 1: Write URL, resolution, and discovery tests**

Cover repository URLs, `.git` suffix normalization, folder URLs, separate refs,
commit overrides, credentials/query/fragment/port rejection, non-GitHub host
rejection, redirects, 404 private/missing classification, response-size bounds,
default-branch lookup, and exact commit validation.

Use a fake resolver response:

```go
resolver := &fakeGitHubResolver{
	repository: "https://github.com/acme/skills",
	commit:     strings.Repeat("a", 40),
	subdir:     "skills",
}
```

Archive fixtures must prove discovery returns a root `SKILL.md` or direct child
directories only, returns deterministic candidate IDs
`sha256(commit + "\x00" + path)[:16]`, and rejects no-skill or nested-beyond-one
level sources.

- [ ] **Step 2: Run discovery tests and verify they fail**

```sh
go test ./internal/skillinstall -run 'Test(GitHub|Discover)' -count=1
```

Expected: FAIL because discovery and ref resolution do not exist.

- [ ] **Step 3: Implement the GitHub parser and resolver**

Use a dedicated HTTP client with proxy disabled, ten-second timeout, disabled
automatic compression, and redirects rejected. Resolve through GitHub's public
API:

```text
GET https://api.github.com/repos/{owner}/{repo}
GET https://api.github.com/repos/{owner}/{repo}/commits/{url.PathEscape(ref)}
```

Send `Accept: application/vnd.github+json` and a fixed Waffle `User-Agent`.
Read at most 64 KiB per JSON response. Map 401/403/404 to
`ErrGitHubPrivateOrMissing`; do not include upstream bodies in returned errors.

- [ ] **Step 4: Implement candidate discovery and exact-commit staging**

Fetch the exact commit through the existing bounded codeload path into private
staging. Inspect the selected subtree without materializing unrelated
candidates into the review stage. Candidate selection is bound to repository,
commit, and path; reject a client-supplied candidate not present in the
server-created discovery record.

Persist discovery records below `StageRoot` with 0600 mode, random 32-hex IDs,
ten-minute expiry, and the same durable JSON writer used for manifests. The
client receives an opaque discovery ID, never a staging path.

The staged manifest provenance is exactly:

```text
git:<canonical-repository>@<commit>#<skill-path>
```

- [ ] **Step 5: Run all installer tests**

```sh
go test ./internal/skillinstall -count=1
```

Expected: PASS, including all existing exact-commit, redirection, archive,
TOCTOU, audit, provenance, and atomic-install tests.

- [ ] **Step 6: Commit GitHub skill discovery**

```sh
git add internal/skillinstall
git commit -m "feat: discover skills from github"
```

---

### Task 2: Add bounded zip upload staging

**Files:**
- Create: `internal/skillinstall/zip.go`
- Create: `internal/skillinstall/zip_test.go`
- Modify: `internal/skillinstall/installer.go`
- Modify: `internal/skillinstall/manifest.go`

**Interfaces:**
- Consumes: `reviewedTreeFromFiles`, the existing audit flags, content digest,
  install provenance, and stage lifetime.
- Produces:

```go
const (
	MaxUploadBodyBytes       = 2 << 20
	MaxUploadCompressedBytes = 1 << 20
	MaxUploadExpandedBytes   = 1 << 20
	MaxUploadFiles           = 64
	MaxUploadEntries         = 256
)

func (i *Installer) DiscoverZip(
	ctx context.Context,
	name string,
	archive io.ReaderAt,
	size int64,
) (Discovery, error)

func (i *Installer) StageZip(
	ctx context.Context,
	discoveryID, candidateID string,
) (Manifest, error)
```

- [ ] **Step 1: Write adversarial zip tests**

Generate fixtures in memory for: one root skill, several direct child skills,
absolute and `..` paths, backslash traversal, NUL, symlink mode, hard-link-like
duplicates, device/special modes, duplicate canonical names, encrypted
entries, data descriptors, too many entries, too many skill files, oversized
compressed input, oversized expanded input, high compression ratio, invalid
UTF-8, malformed central directory, cancellation, and cleanup after every
failure.

The happy-path assertion is:

```go
discovery, err := installer.DiscoverZip(ctx, "skills.zip", readerAt, int64(len(raw)))
if err != nil || len(discovery.Candidates) != 2 {
	t.Fatalf("discovery = %#v, err = %v", discovery, err)
}
manifest, err := installer.StageZip(ctx, discovery.ID, discovery.Candidates[0].ID)
if err != nil || !manifest.Audit.Passed {
	t.Fatalf("manifest = %#v, err = %v", manifest, err)
}
```

- [ ] **Step 2: Run zip tests and verify they fail**

```sh
go test ./internal/skillinstall -run 'TestZip' -count=1
```

Expected: FAIL because the zip source does not exist.

- [ ] **Step 3: Implement bounded zip discovery**

Reject `size <= 0` or `size > MaxUploadCompressedBytes` before constructing
`zip.NewReader`. Normalize every entry with slash semantics, reject all
non-regular entries and duplicate normalized paths, sum declared and actual
expanded bytes, cap reads with `io.LimitedReader`, and enforce the existing
review path/file bounds. Copy accepted bytes into a private discovery
directory; never retain the raw uploaded zip.

- [ ] **Step 4: Reuse the common candidate and stage path**

Factor GitHub and zip candidate enumeration into:

```go
func discoverCandidates(files []reviewedFile, selectedRoot string) ([]SourceCandidate, error)
func stageCandidate(root string, candidate SourceCandidate) (reviewedTree, error)
```

Both sources must produce identical manifests, audit flags, digests, inactive
install behavior, and provenance shape. Zip source provenance is
`upload:sha256:<archive-digest>#<skill-path>` and contains no client filename.

- [ ] **Step 5: Run installer tests**

```sh
go test ./internal/skillinstall -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit zip skill discovery**

```sh
git add internal/skillinstall
git commit -m "feat: stage skills from bounded zip uploads"
```

---

### Task 3: Add sanitized approved-folder discovery

**Files:**
- Create: `internal/skillinstall/local_catalog.go`
- Create: `internal/skillinstall/local_catalog_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.toml`
- Modify: `cmd/waffle/dashboard_wiring.go`
- Modify: `cmd/waffle/dashboard_wiring_test.go`

**Interfaces:**
- Consumes: existing `validateLocalSource`, `os.Root` traversal protection, and
  host-configured import roots.
- Produces:

```go
type ImportRoot struct {
	Label string
	Path  string
}

type PublicImportRoot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func ParseImportRoots(values []string) ([]ImportRoot, error)
func (i *Installer) ImportRoots() []PublicImportRoot
func (i *Installer) DiscoverLocal(context.Context, rootID string) (Discovery, error)
```

The preferred configuration value is `label=/absolute/path`. Existing absolute
path values remain accepted and receive a sanitized basename label; duplicate
derived labels fail startup. Labels are unique safe slugs and paths remain
host-only.

- [ ] **Step 1: Write config and local-catalog tests**

Test malformed explicit labels, duplicate and duplicate-derived labels,
backward-compatible absolute path values, relative paths, nonexistent roots,
symlink roots, root swap, unsafe child directories, stable opaque root IDs,
alphabetical labels/candidates, and JSON that contains labels but not paths.

- [ ] **Step 2: Run tests and verify they fail**

```sh
go test ./internal/config ./internal/skillinstall ./cmd/waffle -run 'Test(ImportRoot|LocalCatalog)' -count=1
```

Expected: FAIL because labeled roots are not supported.

- [ ] **Step 3: Implement labeled root parsing and child discovery**

Keep `Dashboard.SkillImportRoots []string` for TOML compatibility, parse it
once during dashboard wiring, accept legacy absolute values, and fail startup
on malformed explicit labels or ambiguous duplicate labels.
Use `os.Root` and pre/post `FileInfo` checks already used by local staging.
List only direct child directories containing a valid root `SKILL.md`.

- [ ] **Step 4: Update example configuration**

Document:

```toml
[dashboard]
# skill_import_roots = ["team-skills=/srv/waffle/imports"]
```

Explain that Desk shows `team-skills`, never `/srv/waffle/imports`.

- [ ] **Step 5: Run config, installer, and wiring tests**

```sh
go test ./internal/config ./internal/skillinstall ./cmd/waffle -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit approved-folder discovery**

```sh
git add internal/config internal/skillinstall cmd/waffle/dashboard_wiring.go cmd/waffle/dashboard_wiring_test.go config.example.toml
git commit -m "feat: label approved skill folders"
```

---

### Task 4: Expose focused skill discovery, upload, review, install, and activation routes

**Files:**
- Create: `internal/skillsetup/service.go`
- Create: `internal/skillsetup/service_test.go`
- Create: `internal/dashboard/setup_skills.go`
- Create: `internal/dashboard/setup_skills_test.go`
- Modify: `internal/dashboard/setup.go`
- Modify: `internal/dashboard/setup_test.go`
- Modify: `internal/dashboard/capabilities.go`
- Modify: `internal/dashboard/capabilities_test.go`
- Modify: `internal/dashboard/capability_skills.go`
- Modify: `internal/dashboard/router.go`

**Interfaces:**
- Consumes: the three `skillinstall` discovery paths,
  existing skill status/provenance persistence, and mutation middleware.
- Produces these routes:

```text
POST /api/v1/desk/setup/skills/github/discover
POST /api/v1/desk/setup/skills/github/stage
POST /api/v1/desk/setup/skills/archive/discover
POST /api/v1/desk/setup/skills/archive/stage
POST /api/v1/desk/setup/skills/local/discover
POST /api/v1/desk/setup/skills/local/stage
POST /api/v1/desk/setup/skills/install
POST /api/v1/desk/setup/skills/{name}/activate
```

The archive discovery route is streaming multipart with the standard Desk
mutation middleware set to `MaxUploadBodyBytes`; all other routes use 16 KiB
strict JSON.

The shared service used by both Desk and CLI is:

```go
type Service struct {
	DB        *sql.DB
	Workspace memory.Workspace
	Installer *skillinstall.Installer
}

type SourceCapabilities struct {
	GitHub     bool                            `json:"github"`
	Archive    bool                            `json:"archive"`
	LocalRoots []skillinstall.PublicImportRoot `json:"local_roots"`
}

func (s *Service) Sources(context.Context) (SourceCapabilities, error)
func (s *Service) DiscoverGitHub(context.Context, skillinstall.GitHubRequest) (skillinstall.Discovery, error)
func (s *Service) DiscoverZip(context.Context, string, io.ReaderAt, int64) (skillinstall.Discovery, error)
func (s *Service) DiscoverLocal(context.Context, string) (skillinstall.Discovery, error)
func (s *Service) Stage(context.Context, source, discoveryID, candidateID string) (skillinstall.Manifest, error)
func (s *Service) Install(context.Context, stageID, digest string) (skill.Skill, error)
func (s *Service) Activate(context.Context, name string) error
```

- [ ] **Step 1: Write HTTP tests for every source and stable error**

Test request token/idempotency, unknown JSON fields, multipart content type,
missing or multiple files, body truncation, source capability enforcement,
opaque discovery/candidate binding, inactive install, explicit activation,
collision, expiry, digest mismatch, and redaction.

The error table must map:

```go
var skillErrors = []struct {
	err    error
	status int
	code   string
}{
	{skillinstall.ErrSourceNotAllowed, http.StatusForbidden, "skill_source_unavailable"},
	{skillinstall.ErrGitHostNotAllowed, http.StatusForbidden, "skill_git_host_not_allowed"},
	{skillinstall.ErrCommitRequired, http.StatusBadRequest, "skill_commit_resolution_failed"},
	{skillinstall.ErrSkillNotFound, http.StatusNotFound, "skill_not_found_in_source"},
	{skillinstall.ErrUnsafeTree, http.StatusBadRequest, "skill_archive_unsafe"},
	{skillinstall.ErrAuditFailed, http.StatusUnprocessableEntity, "skill_review_failed"},
	{skillinstall.ErrSkillExists, http.StatusConflict, "skill_already_installed"},
}
```

Add `skillinstall.ErrSkillNotFound` for a source with no root/direct-child
skill and for a candidate removed between discovery and staging.

- [ ] **Step 2: Run route tests and verify they fail**

```sh
go test ./internal/dashboard -run 'TestSetupSkill' -count=1
```

Expected: FAIL because the focused routes do not exist.

- [ ] **Step 3: Add the shared skill setup service and typed routes**

Move reviewed install provenance/status orchestration from
`dashboard.WorkspaceCapabilitySkills.Install` into `skillsetup.Service`.
`WorkspaceCapabilitySkills` delegates install and activation to this service;
HTTP receives only the service interface, never `*skillinstall.Installer`.
Remove the generic
`capability_failed` mapping for these routes; messages describe the next safe
action without paths, commits from upstream bodies, or error chains.

- [ ] **Step 4: Extend the Setup snapshot source flags**

Return:

```go
type SkillSourceCapabilities struct {
	GitHub    bool               `json:"github"`
	Archive   bool               `json:"archive"`
	LocalRoots []PublicImportRoot `json:"local_roots"`
}
```

Public `github.com` discovery is built in and does not require a pre-edited
`skill_git_hosts` allowlist. The legacy allowlist continues governing the
advanced exact-Git staging/recovery API but is not exposed as the normal Desk
path. Archive is true when the private stage root is available; local roots
include only valid sanitized labels. Do not render an unavailable source as a
usable form.

- [ ] **Step 5: Run dashboard tests**

```sh
go test ./internal/skillsetup ./internal/dashboard ./internal/skillinstall -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit skill setup routes**

```sh
git add internal/skillsetup internal/dashboard internal/skillinstall
git commit -m "feat: expose guided skill setup"
```

---

### Task 5: Build the accessible Skills task and readable review

**Files:**
- Modify: `internal/dashboard/ui/setup.templ`
- Modify: `internal/dashboard/ui/setup_templ.go`
- Modify: `internal/dashboard/ui/assets/setup.js`
- Modify: `internal/dashboard/ui/assets/setup.css`
- Modify: `internal/dashboard/ui/setup_ui_test.go`
- Modify: `internal/dashboard/ui/setup_client_test.mjs`

**Interfaces:**
- Consumes: Setup `skill_sources`, discovery candidates, `skillinstall.Manifest`,
  install result, and activation result.
- Produces: focused source chooser, candidate chooser, review, inactive success,
  and separate activation states in `#setup-task`.

- [ ] **Step 1: Write client tests for each task state**

Use mocked fetch responses and assert:

- only available source methods render;
- pasting a GitHub folder URL never requires a commit;
- multiple candidates require explicit selection;
- single candidates preselect;
- archive input resets after every outcome;
- review renders name, description, provenance, commit, digest, expiry, audit,
  every file path/size/hash, and bounded text previews as text nodes;
- Install inactive and Activate never share one click;
- Back clears staged state and restores focus;
- errors appear beside the active control; and
- no manifest is inserted with `innerHTML`.

- [ ] **Step 2: Run UI tests and verify they fail**

```sh
go test ./internal/dashboard/ui -run 'TestSetupSkills' -count=1
node --test --test-name-pattern='skill' internal/dashboard/ui/setup_client_test.mjs
```

Expected: FAIL because the new Skills task is not implemented.

- [ ] **Step 3: Implement source and candidate task states**

Render GitHub URL with Advanced Ref/Commit, zip file input, or labeled root
radios according to snapshot capabilities. Submit archive data using
`FormData`; do not set `Content-Type` manually. Disable only the active task
while pending and announce candidate counts in a polite live region.

- [ ] **Step 4: Implement structured review and explicit actions**

Build review nodes with `document.createElement`/`textContent`. Use a table for
file path, size, and SHA-256; use `<details>` for bounded previews. Show the
exact Git commit where applicable. After Install inactive, refresh Setup and
show a canonical installed-inactive success with a separate **Activate**
button. After activation/restart, show **Active** and leave session attachment
to Today.

- [ ] **Step 5: Run UI and end-to-end HTTP tests**

```sh
go test ./internal/dashboard/ui ./internal/dashboard ./internal/skillinstall -count=1
node --test internal/dashboard/ui/setup_client_test.mjs
```

Expected: PASS.

- [ ] **Step 6: Commit the Skills task**

```sh
git add internal/dashboard/ui
git commit -m "feat: guide skill review and activation"
```

---

### Task 6: Add equivalent reviewed skill installation to the CLI

**Files:**
- Create: `cmd/waffle/skills_add_cmd.go`
- Create: `cmd/waffle/skills_add_cmd_test.go`
- Modify: `cmd/waffle/skills_cmd.go`
- Modify: `cmd/waffle/skills_cmd_test.go`
- Modify: `cmd/waffle/completion_cmd.go`
- Modify: `docs/chat.md`

**Interfaces:**
- Consumes: `skillsetup.Service`, the same discovery/manifests used by Desk,
  and the existing `skills activate` behavior.
- Produces:

```text
waffle skills add github <url> [--ref REF] [--commit COMMIT] [--skill NAME] [--activate] [--yes]
waffle skills add archive <zip-file> [--skill NAME] [--activate] [--yes]
waffle skills add folder <root-label> [--skill NAME] [--activate] [--yes]
```

Without `--skill`, a single candidate is selected and multiple candidates are
printed for selection. Without `--yes`, the command prints the complete bounded
manifest and requires an explicit `Install inactive? [y/N]` confirmation.
`--activate` is honored only after inactive installation succeeds and is called
out as a second action in output.

- [ ] **Step 1: Write CLI parsing, review, and redaction tests**

Cover each source, help/usage, unknown flags, ambiguous candidates, explicit
selection, full commit display, manifest file/hash/preview output, decline,
`--yes`, inactive default, explicit activation, collision, unsafe archive,
private repository, missing labeled root, piped archive path, and JSON/error
redaction. Assert GitHub credentials and raw host paths never appear.

- [ ] **Step 2: Run CLI tests and verify they fail**

```sh
go test ./cmd/waffle -run 'TestSkillsAdd' -count=1
```

Expected: FAIL because `skills add` does not exist.

- [ ] **Step 3: Construct one CLI skillsetup service**

Open the existing store/workspace once, construct the same installer
configuration as dashboard wiring, and call `skillsetup.Service`. Keep argument
parsing and terminal confirmation in `cmd/waffle`; do not duplicate discovery,
review, install, provenance, or activation logic.

- [ ] **Step 4: Render a complete text review and explicit activation**

Print name, description, sanitized source, exact commit when applicable,
content digest, expiry, audit flags, and every file path/size/hash/preview.
Install only after confirmation. Print `installed inactive: <name>`, then call
Activate only when `--activate` was explicitly supplied.

- [ ] **Step 5: Run CLI and shared service tests**

```sh
go test ./cmd/waffle ./internal/skillsetup ./internal/skillinstall -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit CLI skill onboarding**

```sh
git add cmd/waffle internal/skillsetup docs/chat.md
git commit -m "feat: add reviewed skills from cli"
```

---

### Task 7: Verify the Skills implementation slice

**Files:**
- Modify only for scoped verification fixes.

**Interfaces:**
- Consumes: Tasks 1–6.
- Produces: an independently reviewable Skills setup slice.

- [ ] **Step 1: Run focused and repository gates**

```sh
mise run fmt
go test ./internal/config ./internal/skillinstall ./internal/skillsetup ./internal/dashboard ./internal/dashboard/ui ./cmd/waffle -count=1
node --test internal/dashboard/ui/*_client_test.mjs
mise run dashboard-check
mise run vet
mise run lint
mise run test
mise run build
git diff --check
```

Expected: every command passes.

- [ ] **Step 2: Perform Safari acceptance with three real sources**

Through the loopback/forwarded managed Desk in Safari:

1. paste a public repository containing multiple skills;
2. select one, verify the resolved exact commit and readable review;
3. install inactive, activate after the distinct confirmation, and attach it
   from Today;
4. repeat discovery with a bounded local zip;
5. when a labeled host root is configured, choose a child without seeing or
   typing its host path;
6. submit an invalid path, private repo, unsafe zip, expired stage, and name
   collision and verify each actionable error.

- [ ] **Step 3: Commit any verification-only fixes**

If verification changed files:

```sh
git add internal/config internal/skillinstall internal/skillsetup internal/dashboard cmd/waffle config.example.toml
git commit -m "fix: harden setup skill onboarding"
```

Otherwise record the commands and Safari evidence in the execution report.
