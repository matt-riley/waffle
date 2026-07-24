package skillinstall

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var pinnedCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type GitFetcher interface {
	Fetch(context.Context, string, string, string) error
}

type sourceSpec struct {
	local     *localSourceSpec
	gitURL    string
	commit    string
	sourceRef string
}

type localSourceSpec struct {
	rootPath   string
	relative   string
	sourcePath string
	rootInfo   os.FileInfo
	sourceInfo os.FileInfo
}

func validateStageRequest(request StageRequest, importRoots, gitHosts []string) (sourceSpec, error) {
	hasLocal := request.LocalPath != ""
	hasGit := request.GitURL != "" || request.Commit != ""
	if hasLocal == hasGit {
		return sourceSpec{}, ErrInvalidRequest
	}
	if hasLocal {
		local, err := validateLocalSource(request.LocalPath, importRoots)
		if err != nil {
			return sourceSpec{}, err
		}
		label := filepath.Base(local.sourcePath)
		if !skillNamePattern.MatchString(label) {
			return sourceSpec{}, fmt.Errorf("%w: local source directory name is not a safe label", ErrSourceNotAllowed)
		}
		return sourceSpec{
			local:     &local,
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

func validateLocalSource(source string, importRoots []string) (localSourceSpec, error) {
	if !cleanAbsolutePath(source) {
		return localSourceSpec{}, fmt.Errorf("%w: local source must be an absolute clean path", ErrSourceNotAllowed)
	}
	canonicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return localSourceSpec{}, fmt.Errorf("%w: resolve local source", ErrSourceNotAllowed)
	}
	sourceInfo, err := os.Lstat(canonicalSource)
	if err != nil || !sourceInfo.IsDir() || sourceInfo.Mode()&os.ModeSymlink != 0 {
		return localSourceSpec{}, fmt.Errorf("%w: local source is not a real directory", ErrSourceNotAllowed)
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
			return localSourceSpec{
				rootPath:   canonicalRoot,
				relative:   relative,
				sourcePath: canonicalSource,
				rootInfo:   rootInfo,
				sourceInfo: sourceInfo,
			}, nil
		}
	}
	return localSourceSpec{}, fmt.Errorf("%w: local source is outside configured import roots", ErrSourceNotAllowed)
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
	privateHome := filepath.Join(filepath.Dir(destination), ".git-home")
	if err := os.Mkdir(privateHome, 0o700); err != nil {
		return fmt.Errorf("create private Git home: %w", err)
	}
	defer func() { _ = os.RemoveAll(privateHome) }()

	command := exec.CommandContext(ctx, "git",
		"-c", "credential.helper=",
		"-c", "credential.interactive=false",
		"archive", "--format=tar", "--remote="+gitURL, commit,
	)
	command.Env = gitEnvironment(privateHome)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open bounded Git archive: %w", err)
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("%w: start Git archive: %v", ErrBoundedGitUnsupported, err)
	}
	archive, readErr := io.ReadAll(io.LimitReader(stdout, maxGitArchiveBytes+1))
	if int64(len(archive)) > maxGitArchiveBytes {
		_ = command.Process.Kill()
		_ = command.Wait()
		return ErrTreeTooLarge
	}
	waitErr := command.Wait()
	if readErr != nil {
		return fmt.Errorf("read bounded Git archive: %w", readErr)
	}
	if waitErr != nil {
		return fmt.Errorf("%w: fetch exact commit: %v", ErrBoundedGitUnsupported, waitErr)
	}
	fetchedCommit, err := gitArchiveCommit(ctx, archive, privateHome)
	if err != nil {
		return err
	}
	if fetchedCommit != commit {
		return fmt.Errorf("%w: got %q", ErrCommitMismatch, fetchedCommit)
	}
	tree, err := reviewedTreeFromArchive(archive)
	if err != nil {
		return err
	}
	if err := writeReviewedTree(destination, tree.files); err != nil {
		return fmt.Errorf("materialize bounded Git archive: %w", err)
	}
	return nil
}

func gitArchiveCommit(ctx context.Context, archive []byte, privateHome string) (string, error) {
	command := exec.CommandContext(ctx, "git", "get-tar-commit-id")
	command.Env = gitEnvironment(privateHome)
	command.Stdin = bytes.NewReader(archive)
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("%w: Git archive omitted exact commit identity", ErrCommitMismatch)
	}
	commit := strings.TrimSpace(string(output))
	if !pinnedCommitPattern.MatchString(commit) {
		return "", fmt.Errorf("%w: invalid archive commit identity", ErrCommitMismatch)
	}
	return commit, nil
}

func reviewedTreeFromArchive(archive []byte) (reviewedTree, error) {
	reader := tar.NewReader(bytes.NewReader(archive))
	files := make([]reviewedFile, 0)
	seen := make(map[string]struct{})
	entriesSeen := 0
	var totalBytes int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return reviewedTree{}, fmt.Errorf("%w: read Git archive", ErrUnsafeTree)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		entriesSeen++
		if entriesSeen > maxReviewEntries {
			return reviewedTree{}, fmt.Errorf("%w: more than %d filesystem entries", ErrTreeTooLarge, maxReviewEntries)
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !safeArchivePath(name) || len(name) > maxReviewPathBytes {
			return reviewedTree{}, fmt.Errorf("%w: invalid archive path", ErrUnsafeTree)
		}
		if _, duplicate := seen[name]; duplicate {
			return reviewedTree{}, fmt.Errorf("%w: duplicate archive path %q", ErrUnsafeTree, name)
		}
		seen[name] = struct{}{}
		if hasVCSComponent(name) {
			return reviewedTree{}, fmt.Errorf("%w: hidden VCS state %q", ErrUnsafeTree, name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return reviewedTree{}, fmt.Errorf("%w: special archive entry %q", ErrUnsafeTree, name)
		}
		if header.Mode&0o111 != 0 {
			return reviewedTree{}, fmt.Errorf("%w: executable file %q", ErrUnsafeTree, name)
		}
		if len(files)+1 > maxReviewFiles {
			return reviewedTree{}, fmt.Errorf("%w: more than %d files", ErrTreeTooLarge, maxReviewFiles)
		}
		if header.Size < 0 || header.Size > maxReviewBytes-totalBytes {
			return reviewedTree{}, fmt.Errorf("%w: more than %d bytes", ErrTreeTooLarge, maxReviewBytes)
		}
		data, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
		if err != nil || int64(len(data)) != header.Size {
			return reviewedTree{}, fmt.Errorf("%w: truncated archive file %q", ErrUnsafeTree, name)
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return reviewedTree{}, fmt.Errorf("%w: binary or NUL content in %q", ErrUnsafeTree, name)
		}
		totalBytes += int64(len(data))
		files = append(files, newReviewedFile(name, data))
	}
	return reviewedTreeFromFiles(files)
}

func safeArchivePath(name string) bool {
	return name != "" && name != "." && !path.IsAbs(name) && path.Clean(name) == name &&
		name != ".." && !strings.HasPrefix(name, "../") && !strings.ContainsRune(name, 0)
}

func gitEnvironment(privateHome string) []string {
	environment := make([]string, 0, len(os.Environ())+6)
	for _, item := range os.Environ() {
		key, _, found := strings.Cut(item, "=")
		if !found || strings.HasPrefix(key, "GIT_") || key == "SSH_ASKPASS" ||
			key == "HOME" || key == "XDG_CONFIG_HOME" {
			continue
		}
		environment = append(environment, item)
	}
	return append(environment,
		"HOME="+privateHome,
		"XDG_CONFIG_HOME="+privateHome,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS="+os.DevNull,
		"SSH_ASKPASS="+os.DevNull,
	)
}
