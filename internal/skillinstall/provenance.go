package skillinstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

const installProvenanceVersion = 1

// InstallProvenance is the durable evidence needed to finish recording an
// already-committed reviewed install after a process or database failure.
type InstallProvenance struct {
	StageID       string
	ContentDigest string
	Name          string
	Description   string
	SourceRef     string
}

type installProvenanceRecord struct {
	Version        int `json:"version"`
	Provenance     InstallProvenance
	InstalledFiles []FileEntry `json:"installed_files"`
}

// PrepareInstallProvenance durably journals reviewed provenance before the
// install commit. recovered is true when the exact inactive skill is already
// present and only provenance persistence remains.
func (i *Installer) PrepareInstallProvenance(stageID, digest string) (provenance InstallProvenance, recovered bool, err error) {
	if i == nil {
		return InstallProvenance{}, false, errors.New("skill installer required")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if !stageIDPattern.MatchString(stageID) || digest == "" {
		return InstallProvenance{}, false, ErrStageNotFound
	}
	if err := validateExistingRoot(i.StageRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return InstallProvenance{}, false, ErrStageNotFound
		}
		return InstallProvenance{}, false, err
	}

	record, present, err := i.readInstallProvenance(stageID)
	if err != nil {
		return InstallProvenance{}, false, err
	}
	if present {
		if record.Version != installProvenanceVersion ||
			record.Provenance.StageID != stageID ||
			record.Provenance.ContentDigest != digest {
			return InstallProvenance{}, false, ErrStageChanged
		}
		installed, verifyErr := i.installedProvenanceMatches(record)
		if verifyErr != nil {
			return InstallProvenance{}, false, verifyErr
		}
		if installed {
			return record.Provenance, true, nil
		}
		if _, statErr := os.Lstat(filepath.Join(i.StageRoot, stageID)); errors.Is(statErr, os.ErrNotExist) {
			return InstallProvenance{}, false, ErrStageNotFound
		} else if statErr != nil {
			return InstallProvenance{}, false, statErr
		}
		return record.Provenance, false, nil
	}

	stagePath := filepath.Join(i.StageRoot, stageID)
	stageInfo, err := os.Lstat(stagePath)
	if err != nil || !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
		return InstallProvenance{}, false, ErrStageNotFound
	}
	stage, err := readStageRecord(filepath.Join(stagePath, stageRecordName))
	if err != nil {
		return InstallProvenance{}, false, err
	}
	if stage.Version != 1 || stage.Manifest.StageID != stageID {
		return InstallProvenance{}, false, ErrStageChanged
	}
	if digest != stage.Manifest.ContentDigest {
		return InstallProvenance{}, false, ErrDigestMismatch
	}
	if !i.clock().Before(stage.Manifest.ExpiresAt) {
		return InstallProvenance{}, false, ErrStageExpired
	}
	tree, err := readReviewedTree(filepath.Join(stagePath, "content"))
	if err != nil {
		return InstallProvenance{}, false, fmt.Errorf("%w: %v", ErrStageChanged, err)
	}
	if !treeMatchesManifest(tree, stage.Manifest) {
		return InstallProvenance{}, false, ErrStageChanged
	}
	if err := ensureSkillAbsent(i.SkillsRoot, tree.name); err != nil {
		return InstallProvenance{}, false, err
	}
	finalFiles, err := filesWithInactiveStatus(tree.files)
	if err != nil {
		return InstallProvenance{}, false, err
	}
	installedEntries := make([]FileEntry, len(finalFiles))
	for index, file := range finalFiles {
		installedEntries[index] = file.entry
		installedEntries[index].Preview = ""
	}
	provenance = InstallProvenance{
		StageID:       stageID,
		ContentDigest: stage.Manifest.ContentDigest,
		Name:          stage.Manifest.Name,
		Description:   stage.Manifest.Description,
		SourceRef:     stage.Manifest.SourceRef,
	}
	if err := i.writeInstallProvenance(installProvenanceRecord{
		Version:        installProvenanceVersion,
		Provenance:     provenance,
		InstalledFiles: installedEntries,
	}); err != nil {
		return InstallProvenance{}, false, err
	}
	return provenance, false, nil
}

// CompleteInstallProvenance removes the private journal only after the durable
// skill status record has been written.
func (i *Installer) CompleteInstallProvenance(stageID, digest string) error {
	if i == nil {
		return errors.New("skill installer required")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	record, present, err := i.readInstallProvenance(stageID)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if record.Provenance.StageID != stageID || record.Provenance.ContentDigest != digest {
		return ErrStageChanged
	}
	return i.removeInstallProvenance(stageID)
}

// DiscardInstallProvenance removes a prepared journal when installation is
// known not to have committed.
func (i *Installer) DiscardInstallProvenance(stageID, digest string) error {
	if i == nil {
		return errors.New("skill installer required")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	record, present, err := i.readInstallProvenance(stageID)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if record.Provenance.StageID != stageID || record.Provenance.ContentDigest != digest {
		return ErrStageChanged
	}
	installed, err := i.installedProvenanceMatches(record)
	if err != nil {
		return err
	}
	if installed {
		return errors.New("refuse to discard committed skill install provenance")
	}
	return i.removeInstallProvenance(stageID)
}

func (i *Installer) installedProvenanceMatches(record installProvenanceRecord) (bool, error) {
	target := filepath.Join(i.SkillsRoot, record.Provenance.Name)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, ErrStageChanged
	}
	tree, err := readReviewedTreeBound(target, maxReviewBytes+maxInactiveGrowth)
	if err != nil {
		return false, ErrStageChanged
	}
	entries := tree.entries()
	for index := range entries {
		entries[index].Preview = ""
	}
	if tree.name != record.Provenance.Name ||
		tree.description != record.Provenance.Description ||
		!slices.Equal(entries, record.InstalledFiles) {
		return false, ErrStageChanged
	}
	return true, nil
}

func (i *Installer) installProvenancePath(stageID string) string {
	return filepath.Join(i.StageRoot, ".install-provenance-"+stageID+".json")
}

func (i *Installer) readInstallProvenance(stageID string) (installProvenanceRecord, bool, error) {
	if !stageIDPattern.MatchString(stageID) {
		return installProvenanceRecord{}, false, ErrStageNotFound
	}
	return readBoundedJSONFile[installProvenanceRecord](i.installProvenancePath(stageID), maxStageRecord)
}

func (i *Installer) writeInstallProvenance(record installProvenanceRecord) (retErr error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode reviewed install provenance: %w", err)
	}
	temporary, err := os.CreateTemp(i.StageRoot, ".install-provenance-write-*")
	if err != nil {
		return fmt.Errorf("create reviewed install provenance: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, temporary.Close())
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			retErr = errors.Join(retErr, removeErr)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if err := writeSyncClose(temporary, append(encoded, '\n')); err != nil {
		return fmt.Errorf("write reviewed install provenance: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryPath, i.installProvenancePath(record.Provenance.StageID)); err != nil {
		return fmt.Errorf("commit reviewed install provenance: %w", err)
	}
	if err := syncDirectory(i.StageRoot); err != nil {
		return fmt.Errorf("sync reviewed install provenance: %w", err)
	}
	return nil
}

func (i *Installer) removeInstallProvenance(stageID string) error {
	if err := os.Remove(i.installProvenancePath(stageID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(i.StageRoot)
}
