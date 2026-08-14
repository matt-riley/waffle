package selfdev

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/matt-riley/waffle/internal/config"
)

// UpgradeRecord is the durable audit record binding one installed artifact to
// the exact commit and tree it was built from (#413). Every field is resolved
// before install, and the record is appended on failure as well as success so
// a misbehaving upgrade is observable.
type UpgradeRecord struct {
	BaseSHA        string    `json:"base_sha"`
	CandidateSHA   string    `json:"candidate_sha"`
	TreeHash       string    `json:"tree_hash"`
	ArtifactSHA256 string    `json:"artifact_sha256,omitempty"`
	Approval       string    `json:"approval"`
	Verify         bool      `json:"verify"`
	Verification   string    `json:"verification"` // ok | skipped | failed
	Version        string    `json:"version,omitempty"`
	Error          string    `json:"error,omitempty"`
	InstalledAt    time.Time `json:"installed_at,omitempty"`
}

// persistUpgradeRecord appends one audit line to
// <home>/selfdev-upgrades.jsonl. Failures to write the audit are surfaced so
// the upgrade is not silently unaccountable.
func persistUpgradeRecord(r UpgradeRecord) error {
	home, err := config.Home()
	if err != nil {
		return fmt.Errorf("resolve home for upgrade audit: %w", err)
	}
	path := filepath.Join(home, "selfdev-upgrades.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create upgrade audit directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open upgrade audit: %w", err)
	}
	w := bufio.NewWriter(f)
	encErr := json.NewEncoder(w).Encode(r)
	flushErr := w.Flush()
	closeErr := f.Close()
	if encErr != nil {
		return fmt.Errorf("write upgrade audit: %w", encErr)
	}
	if flushErr != nil {
		return fmt.Errorf("flush upgrade audit: %w", flushErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close upgrade audit: %w", closeErr)
	}
	return nil
}
