package skillinstall

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var pinnedCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type GitFetcher interface {
	Fetch(context.Context, string, string, string) error
}

type sourceSpec struct {
	localPath string
	gitURL    string
	commit    string
	sourceRef string
}

func validateStageRequest(request StageRequest, importRoots, gitHosts []string) (sourceSpec, error) {
	hasLocal := request.LocalPath != ""
	hasGit := request.GitURL != "" || request.Commit != ""
	if hasLocal == hasGit {
		return sourceSpec{}, ErrInvalidRequest
	}
	if hasLocal {
		canonical, err := validateLocalSource(request.LocalPath, importRoots)
		if err != nil {
			return sourceSpec{}, err
		}
		label := filepath.Base(canonical)
		if !skillNamePattern.MatchString(label) {
			return sourceSpec{}, fmt.Errorf("%w: local source directory name is not a safe label", ErrSourceNotAllowed)
		}
		return sourceSpec{
			localPath: canonical,
			sourceRef: "local:" + label,
		}, nil
	}
	canonicalURL, err := validateGitSource(request.GitURL, request.Commit, gitHosts)
	if err != nil {
		return sourceSpec{}, err
	}
	return sourceSpec{
		gitURL:    canonicalURL,
		commit:    request.Commit,
		sourceRef: "git:" + canonicalURL + "@" + request.Commit,
	}, nil
}

func validateLocalSource(source string, importRoots []string) (string, error) {
	if !cleanAbsolutePath(source) {
		return "", fmt.Errorf("%w: local source must be an absolute clean path", ErrSourceNotAllowed)
	}
	canonicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("%w: resolve local source", ErrSourceNotAllowed)
	}
	sourceInfo, err := os.Lstat(canonicalSource)
	if err != nil || !sourceInfo.IsDir() || sourceInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: local source is not a real directory", ErrSourceNotAllowed)
	}
	for _, root := range importRoots {
		if !cleanAbsolutePath(root) {
			continue
		}
		canonicalRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rootInfo, err := os.Lstat(canonicalRoot)
		if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		relative, err := filepath.Rel(canonicalRoot, canonicalSource)
		if err != nil {
			continue
		}
		if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return canonicalSource, nil
		}
	}
	return "", fmt.Errorf("%w: local source is outside configured import roots", ErrSourceNotAllowed)
}

func cleanAbsolutePath(value string) bool {
	return value != "" && !strings.ContainsRune(value, 0) && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func validateGitSource(rawURL, commit string, allowedHosts []string) (string, error) {
	if !pinnedCommitPattern.MatchString(commit) {
		return "", ErrCommitRequired
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.Host == "" || parsed.Port() != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawPath != "" || strings.ContainsRune(parsed.Path, 0) ||
		parsed.Path == "" || parsed.Path == "/" || path.Clean(parsed.Path) != parsed.Path {
		return "", fmt.Errorf("%w: Git URL must be credential-free HTTPS", ErrSourceNotAllowed)
	}
	host := strings.ToLower(parsed.Hostname())
	allowed := false
	for _, configured := range allowedHosts {
		if configured == host && configured == strings.ToLower(configured) &&
			!strings.ContainsAny(configured, "/:@") {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("%w: %s", ErrGitHostNotAllowed, host)
	}
	parsed.Scheme = "https"
	parsed.Host = host
	return parsed.String(), nil
}

type commandGitFetcher struct{}

func (commandGitFetcher) Fetch(ctx context.Context, gitURL, commit, destination string) error {
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("git fetch destination already exists")
		}
		return fmt.Errorf("inspect Git fetch destination: %w", err)
	}
	commands := [][]string{
		{
			"-c", "credential.helper=", "clone", "--no-checkout", "--filter=blob:none",
			"--depth=1", "--no-tags", "--recurse-submodules=no", gitURL, destination,
		},
		{
			"-C", destination, "-c", "credential.helper=", "fetch", "--depth=1",
			"--no-tags", "origin", commit,
		},
		{
			"-C", destination, "-c", "advice.detachedHead=false", "checkout",
			"--detach", "--force", commit,
		},
	}
	for _, arguments := range commands {
		command := exec.CommandContext(ctx, "git", arguments...)
		command.Env = gitEnvironment()
		if err := command.Run(); err != nil {
			return fmt.Errorf("fetch pinned Git skill: %w", err)
		}
	}
	headCommand := exec.CommandContext(ctx, "git", "-C", destination, "rev-parse", "--verify", "HEAD")
	headCommand.Env = gitEnvironment()
	head, err := headCommand.Output()
	if err != nil {
		return fmt.Errorf("verify fetched Git skill: %w", err)
	}
	if strings.TrimSpace(string(head)) != commit {
		return fmt.Errorf("%w: got %q", ErrCommitMismatch, strings.TrimSpace(string(head)))
	}
	if err := os.RemoveAll(filepath.Join(destination, ".git")); err != nil {
		return fmt.Errorf("remove private Git metadata: %w", err)
	}
	return nil
}

func gitEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+6)
	for _, item := range os.Environ() {
		key, _, found := strings.Cut(item, "=")
		if !found || strings.HasPrefix(key, "GIT_") || key == "SSH_ASKPASS" {
			continue
		}
		environment = append(environment, item)
	}
	return append(environment,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS="+os.DevNull,
		"SSH_ASKPASS="+os.DevNull,
	)
}
