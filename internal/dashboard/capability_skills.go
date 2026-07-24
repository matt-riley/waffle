package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/skillinstall"
)

// WorkspaceCapabilitySkills reconciles the existing skill library,
// per-session attachments, reviewed installer, and persisted provenance.
type WorkspaceCapabilitySkills struct {
	DB          *sql.DB
	Workspace   memory.Workspace
	Attachments *skill.Attachments
	Installer   *skillinstall.Installer

	mu      sync.Mutex
	staged  map[string]skillinstall.Manifest
	pending map[string]skill.StatusRecord
}

func (s *WorkspaceCapabilitySkills) List(ctx context.Context, sessionID string) ([]CapabilitySkill, error) {
	if s == nil {
		return nil, ErrCapabilitiesUnavailable
	}
	all, err := skill.Discover(s.Workspace.SkillsDir())
	if err != nil {
		return nil, err
	}
	active, err := skill.DiscoverActive(s.Workspace.SkillsDir(), s.DB)
	if err != nil {
		return nil, err
	}
	activeNames := make(map[string]struct{}, len(active))
	for _, item := range active {
		activeNames[item.Name] = struct{}{}
	}
	attachedNames := make(map[string]struct{})
	if sessionID != "" {
		if s.Attachments == nil {
			return nil, ErrCapabilitiesUnavailable
		}
		names, err := s.Attachments.List(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			attachedNames[name] = struct{}{}
		}
	}
	result := make([]CapabilitySkill, 0, len(all)+len(attachedNames))
	installed := make(map[string]struct{}, len(all))
	for _, item := range all {
		_, isActive := activeNames[item.Name]
		_, isAttached := attachedNames[item.Name]
		result = append(result, CapabilitySkill{
			Name:        item.Name,
			Description: item.Description,
			Active:      isActive,
			Attached:    isAttached,
		})
		installed[item.Name] = struct{}{}
	}
	for name := range attachedNames {
		if _, ok := installed[name]; ok {
			continue
		}
		result = append(result, CapabilitySkill{Name: name, Attached: true, Missing: true})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *WorkspaceCapabilitySkills) Attach(ctx context.Context, sessionID, name string) error {
	if s == nil || s.Attachments == nil {
		return ErrCapabilitiesUnavailable
	}
	active, err := skill.DiscoverActive(s.Workspace.SkillsDir(), s.DB)
	if err != nil {
		return err
	}
	if _, ok := skill.Find(active, strings.TrimSpace(name)); !ok {
		return ErrCapabilitySkillNotFound
	}
	return s.Attachments.Attach(ctx, sessionID, name)
}

func (s *WorkspaceCapabilitySkills) Detach(ctx context.Context, sessionID, name string) error {
	if s == nil || s.Attachments == nil {
		return ErrCapabilitiesUnavailable
	}
	return s.Attachments.Detach(ctx, sessionID, name)
}

func (s *WorkspaceCapabilitySkills) Stage(ctx context.Context, request skillinstall.StageRequest) (skillinstall.Manifest, error) {
	if s == nil || s.Installer == nil {
		return skillinstall.Manifest{}, ErrCapabilitiesUnavailable
	}
	manifest, err := s.Installer.Stage(ctx, request)
	if err != nil {
		return skillinstall.Manifest{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.staged == nil {
		s.staged = make(map[string]skillinstall.Manifest)
	}
	s.staged[manifest.StageID] = manifest
	return manifest, nil
}

func (s *WorkspaceCapabilitySkills) Install(ctx context.Context, stageID, digest string) (CapabilitySkill, error) {
	if s == nil || s.Installer == nil {
		return CapabilitySkill{}, ErrCapabilitiesUnavailable
	}
	key := stageID + "\x00" + digest
	s.mu.Lock()
	if pending, ok := s.pending[key]; ok {
		s.mu.Unlock()
		if err := skill.SetSkillStatusRecord(ctx, s.DB, pending); err != nil {
			return CapabilitySkill{}, err
		}
		s.mu.Lock()
		delete(s.pending, key)
		delete(s.staged, stageID)
		s.mu.Unlock()
		return CapabilitySkill{Name: pending.Name, Active: false}, nil
	}
	manifest, ok := s.staged[stageID]
	s.mu.Unlock()
	if !ok || manifest.ContentDigest != digest {
		return CapabilitySkill{}, skillinstall.ErrStageNotFound
	}

	result, err := s.Installer.InstallReviewed(ctx, stageID, digest)
	if err != nil {
		return CapabilitySkill{}, err
	}
	if !result.Committed {
		return CapabilitySkill{}, errors.New("reviewed skill install did not commit")
	}
	record := skill.StatusRecord{
		Name:          result.Skill.Name,
		Status:        skill.StatusInactive,
		Source:        "dashboard",
		SourceRef:     manifest.SourceRef,
		ContentDigest: manifest.ContentDigest,
	}
	if err := skill.SetSkillStatusRecord(ctx, s.DB, record); err != nil {
		s.mu.Lock()
		if s.pending == nil {
			s.pending = make(map[string]skill.StatusRecord)
		}
		s.pending[key] = record
		s.mu.Unlock()
		return CapabilitySkill{}, err
	}
	s.mu.Lock()
	delete(s.staged, stageID)
	s.mu.Unlock()
	return CapabilitySkill{
		Name:        result.Skill.Name,
		Description: result.Skill.Description,
		Active:      false,
	}, nil
}

func (s *WorkspaceCapabilitySkills) Activate(ctx context.Context, name string) error {
	if s == nil {
		return ErrCapabilitiesUnavailable
	}
	all, err := skill.Discover(s.Workspace.SkillsDir())
	if err != nil {
		return err
	}
	if _, ok := skill.Find(all, strings.TrimSpace(name)); !ok {
		return ErrCapabilitySkillNotFound
	}
	return skill.ActivateSkill(ctx, s.DB, s.Workspace, name)
}

var _ CapabilitySkills = (*WorkspaceCapabilitySkills)(nil)
