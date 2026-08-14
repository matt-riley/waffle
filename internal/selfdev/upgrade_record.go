package selfdev

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/flock"
)

// UpgradeRecord is the durable audit record binding one installed artifact to
// the exact commit and tree it was built from (#413). Every field is resolved
// before install, and the record is appended on failure as well as success so
// a misbehaving upgrade is observable. With approval=ci the CI evidence is
// persisted too (#415).
type UpgradeRecord struct {
	BaseSHA        string      `json:"base_sha"`
	CandidateSHA   string      `json:"candidate_sha"`
	TreeHash       string      `json:"tree_hash"`
	ArtifactSHA256 string      `json:"artifact_sha256,omitempty"`
	Approval       string      `json:"approval"`
	Verify         bool        `json:"verify"`
	Verification   string      `json:"verification"` // ok | skipped | failed
	Version        string      `json:"version,omitempty"`
	Error          string      `json:"error,omitempty"`
	InstalledAt    time.Time   `json:"installed_at,omitempty"`
	RequiredChecks []string    `json:"required_checks,omitempty"`
	CIEvidence     *CIEvidence `json:"ci_evidence,omitempty"`
	CIVerified     bool        `json:"ci_verified"`
}

// persistUpgradeRecord appends one JSON audit line to
// <home>/selfdev-upgrades.jsonl. Appends are serialized across processes with
// an advisory flock on a sidecar lockfile, written as a single payload, and
// fsynced before close so a crash cannot lose or interleave the audit line
// (#413 review). Failures are surfaced to the caller.
func persistUpgradeRecord(r UpgradeRecord) error {
	home, err := config.Home()
	if err != nil {
		return fmt.Errorf("resolve home for upgrade audit: %w", err)
	}
	path := filepath.Join(home, "selfdev-upgrades.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create upgrade audit directory: %w", err)
	}
	// Serialize appends across concurrent `waffle upgrade` processes; the
	// sidecar lock lives beside the audit file (flock_other holds a process
	// mutex too, so same-process callers are covered on every platform).
	unlock, err := flock.Acquire(path+".lock", "selfdev upgrade audit", 5*time.Second)
	if err != nil {
		return fmt.Errorf("lock upgrade audit: %w", err)
	}
	defer func() { _ = unlock() }()

	payload, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode upgrade audit: %w", err)
	}
	payload = append(payload, '\n')

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open upgrade audit: %w", err)
	}
	// Single write, then fsync before close: a crash mid-write can leave a
	// truncated final line, but it can never interleave or vanish silently.
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		return fmt.Errorf("write upgrade audit: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync upgrade audit: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close upgrade audit: %w", err)
	}
	return nil
}
