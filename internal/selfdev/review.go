package selfdev

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/secret"
)

const SeverityBlocker = "blocker"

// Finding is one structured result from the self-development reviewer.
type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Summary  string `json:"summary"`
}

// ReviewRecord is the durable audit record for a reviewed candidate commit.
type ReviewRecord struct {
	CommitSHA string    `json:"commit_sha"`
	Approval  string    `json:"approval"`
	Findings  []Finding `json:"findings"`
	Reviewed  time.Time `json:"reviewed_at"`
}

// Reviewer asks a provider for a JSON array of findings.
type Reviewer struct {
	Provider llm.Provider
	Model    string
}

func (r Reviewer) Review(ctx context.Context, diff, task string) ([]Finding, error) {
	if r.Provider == nil || r.Model == "" {
		return nil, fmt.Errorf("reviewer is not configured")
	}
	prompt := "Review this self-development change. Check task fit, protected paths, and any weakened tests, verification, reviewer, eval, or doctor gate. Return only a JSON array of findings with severity blocker|warn|nit, file, and summary.\nTASK:\n" + task + "\nDIFF:\n" + diff
	resp, err := r.Provider.Complete(ctx, llm.Request{Model: r.Model, MaxTokens: 2048, Messages: []llm.Message{llm.UserText(prompt)}}, nil)
	if err != nil {
		return nil, fmt.Errorf("review candidate: %w", err)
	}
	var findings []Finding
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Message.Text())), &findings); err != nil {
		return nil, fmt.Errorf("parse reviewer findings: %w", err)
	}
	for i, finding := range findings {
		switch finding.Severity {
		case SeverityBlocker, "warn", "nit":
		default:
			return nil, fmt.Errorf("reviewer finding %d has invalid severity %q", i, finding.Severity)
		}
		if finding.File == "" || finding.Summary == "" {
			return nil, fmt.Errorf("reviewer finding %d requires file and summary", i)
		}
	}
	return findings, nil
}

func enforceReview(path string, record ReviewRecord) error {
	if record.Reviewed.IsZero() {
		record.Reviewed = time.Now().UTC()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create review log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open review log: %w", err)
	}
	w := bufio.NewWriter(f)
	encErr := json.NewEncoder(w).Encode(record)
	flushErr := w.Flush()
	closeErr := f.Close()
	if encErr != nil {
		return fmt.Errorf("write review log: %w", encErr)
	}
	if flushErr != nil {
		return fmt.Errorf("flush review log: %w", flushErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close review log: %w", closeErr)
	}
	for _, finding := range record.Findings {
		if finding.Severity == SeverityBlocker {
			return fmt.Errorf("review blocker stopped %s upgrade: %s: %s", record.Approval, finding.File, finding.Summary)
		}
	}
	return nil
}

// reviewExactCommit reviews the exact diff baseSHA..sha and persists the audit
// record bound to the candidate SHA (#413). The configured checkout is read
// only; the candidate is always an immutable resolved SHA, so a force-push
// after review cannot change what is verified and installed.
func reviewExactCommit(ctx context.Context, repoDir, baseSHA, sha, approval string) error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	reviewer, err := configuredReviewer(cfg)
	if err != nil {
		return err
	}
	diff, err := commandOutput(ctx, repoDir, "git", "diff", baseSHA, sha, "--")
	if err != nil {
		return fmt.Errorf("read candidate diff: %w", err)
	}
	findings, err := reviewer.Review(ctx, diff, "upgrade "+sha)
	if err != nil {
		return err
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	return enforceReview(filepath.Join(home, "selfdev-reviews.jsonl"), ReviewRecord{CommitSHA: sha, Approval: approval, Findings: findings})
}

func configuredReviewer(cfg config.Config) (Reviewer, error) {
	if cfg.ProviderRegistrySource() == config.ProviderRegistryExplicit {
		alias := strings.TrimSpace(cfg.Agent.UtilityModel)
		if alias == "" {
			alias = strings.TrimSpace(cfg.Agent.DefaultModel)
		}
		if alias == "" {
			return Reviewer{}, fmt.Errorf("reviewer is not configured: agent.default_model is empty")
		}
		provider, target, _, err := namedProvider(cfg, alias)
		if err != nil {
			return Reviewer{}, err
		}
		return Reviewer{Provider: provider, Model: target.UpstreamModel}, nil
	}
	model := reviewerModel(cfg.Provider)
	env, err := providerEnvName(cfg.Provider.Name)
	if err != nil {
		return Reviewer{}, err
	}
	key, err := secret.ResolveRef(cfg.Provider.APIKey, env)
	if err != nil {
		return Reviewer{}, err
	}
	provider, err := doctorProvider(cfg.Provider, key)
	if err != nil {
		return Reviewer{}, err
	}
	return Reviewer{Provider: provider, Model: model}, nil
}

func reviewerModel(provider config.Provider) string {
	if provider.UtilityModel != "" {
		return provider.UtilityModel
	}
	return provider.Model
}
