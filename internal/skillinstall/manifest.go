package skillinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"
)

const (
	maxReviewFiles    = 64
	maxReviewBytes    = 1 << 20
	maxReviewEntries  = 256
	maxStageRecord    = 8 * maxReviewBytes
	maxInactiveGrowth = 64
	stageLifetime     = 10 * time.Minute
	stageIDBytes      = 16
)

var (
	ErrInvalidRequest    = errors.New("invalid skill stage request")
	ErrSourceNotAllowed  = errors.New("skill source not allowed")
	ErrCommitRequired    = errors.New("exact pinned Git commit required")
	ErrGitHostNotAllowed = errors.New("git host not allowed")
	ErrCommitMismatch    = errors.New("fetched git commit does not match requested commit")
	ErrUnsafeTree        = errors.New("unsafe skill source tree")
	ErrTreeTooLarge      = errors.New("skill source exceeds review bounds")
	ErrAuditFailed       = errors.New("skill audit failed")
	ErrSkillExists       = errors.New("skill already installed")
	ErrStageNotFound     = errors.New("skill install stage not found")
	ErrStageExpired      = errors.New("skill install stage expired")
	ErrDigestMismatch    = errors.New("review digest mismatch")
	ErrStageChanged      = errors.New("staged skill changed after review")
)

type StageRequest struct {
	LocalPath string
	GitURL    string
	Commit    string
}

type Manifest struct {
	Name          string      `json:"name"`
	Description   string      `json:"description"`
	SourceRef     string      `json:"source_ref"`
	ContentDigest string      `json:"content_digest"`
	Files         []FileEntry `json:"files"`
	Audit         AuditView   `json:"audit"`
	StageID       string      `json:"stage_id"`
	ExpiresAt     time.Time   `json:"expires_at"`
}

type FileEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Preview string `json:"preview,omitempty"`
}

type AuditView struct {
	Passed bool     `json:"passed"`
	Flags  []string `json:"flags"`
}

type persistedStage struct {
	Version  int      `json:"version"`
	Manifest Manifest `json:"manifest"`
}

type reviewedFile struct {
	entry FileEntry
	data  []byte
}

type reviewedTree struct {
	name        string
	description string
	files       []reviewedFile
	audit       AuditView
}

func (t reviewedTree) entries() []FileEntry {
	entries := make([]FileEntry, len(t.files))
	for index := range t.files {
		entries[index] = t.files[index].entry
	}
	return entries
}

func contentDigest(entries []FileEntry) string {
	var encoded bytes.Buffer
	for _, entry := range entries {
		writeDigestString(&encoded, entry.Path)
		_ = binary.Write(&encoded, binary.BigEndian, uint64(entry.Size))
		writeDigestString(&encoded, entry.SHA256)
	}
	sum := sha256.Sum256(encoded.Bytes())
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeDigestString(encoded *bytes.Buffer, value string) {
	_ = binary.Write(encoded, binary.BigEndian, uint64(len(value)))
	_, _ = encoded.WriteString(value)
}
