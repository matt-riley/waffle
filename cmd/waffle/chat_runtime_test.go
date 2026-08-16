package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/chatwire"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/dashboard"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/repopolicy"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	usagepkg "github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workset"
	"github.com/matt-riley/waffle/internal/workspace"
)

func TestChatRuntimeModelSelectionPersistsAndResumeRestoresIt(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := sessions.Get(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ModelAlias != "gpt" {
		t.Fatalf("saved = %+v", saved)
	}

	second := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	resumed, err := second.Open(ctx, chatpkg.OpenOptions{SessionID: state.SessionID})
	if err != nil || resumed.ModelAlias != "gpt" {
		t.Fatalf("resumed = %+v, %v", resumed, err)
	}
}

func TestChatRuntimeContinueLoadsModelAliasVersion(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}

	resumed := newRuntimeAgainstSameStore(t, configuredChatModels(), sessions)
	continued, err := resumed.Open(ctx, chatpkg.OpenOptions{Continue: true})
	if err != nil || continued.SessionID != state.SessionID || continued.ModelAlias != "gpt" {
		t.Fatalf("continued = %+v, %v", continued, err)
	}
	if _, err := resumed.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "claude"}, nil); err != nil {
		t.Fatalf("valid model change after continue: %v", err)
	}
	saved, err := sessions.Get(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ModelAlias != "claude" || saved.ModelAliasVersion != 2 {
		t.Fatalf("saved after continue = %+v", saved)
	}
}

func TestChatRuntimeStaleModelSelectionCannotReintroduceRemovedAlias(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}

	// Simulate the provider manager's durable SessionApply after this runtime
	// was opened. The runtime's config and cached session version are stale,
	// but the exact session transition still fences its next model write.
	beforeRemoval, err := sessions.Get(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	removal := session.ModelAliasChange{
		SessionID:            beforeRemoval.ID,
		OriginalAlias:        "gpt",
		ReplacementAlias:     "",
		OriginalVersion:      beforeRemoval.ModelAliasVersion,
		ReplacementVersion:   beforeRemoval.ModelAliasVersion + 1,
		OriginalUpdatedAt:    beforeRemoval.UpdatedAt.Format(time.RFC3339Nano),
		ReplacementUpdatedAt: "2026-07-25T22:00:00Z",
	}
	if err := sessions.ReplaceModelAliases(ctx, []session.ModelAliasChange{removal}); err != nil {
		t.Fatal(err)
	}

	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); !errors.Is(err, session.ErrModelAliasChanged) {
		t.Fatalf("stale model selection error = %v, want ErrModelAliasChanged", err)
	}
	afterAttempt, err := sessions.Get(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if afterAttempt.ModelAlias != "" || afterAttempt.ModelAliasVersion != removal.ReplacementVersion {
		t.Fatalf("stale model selection restored alias = %q at version %d, want empty alias at version %d", afterAttempt.ModelAlias, afterAttempt.ModelAliasVersion, removal.ReplacementVersion)
	}
}

func TestSkillsCommandAttachesIdempotentlyAndDetachesWithoutDeactivation(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	reviewerPath := writeRuntimeSkill(t, "reviewer", "review every change", skill.StatusActive, "Review every changed file.")
	writeRuntimeSkill(t, "alpha", "first skill", skill.StatusActive, "Start with the smallest risk.")
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	baseSystem := runtime.agent.System
	if got := skillRefNames(state.Skills); !reflect.DeepEqual(got, []string{"alpha", "reviewer"}) {
		t.Fatalf("open skills = %v", got)
	}

	listed, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if listed.State == nil || listed.Title != "Session skills" || !strings.Contains(listed.Text, "reviewer") {
		t.Fatalf("list result = %+v", listed)
	}

	var events []chatpkg.Event
	attached, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "attach reviewer"}, func(event chatpkg.Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantBlock := "\n<attached_skills>\n<attached_skill name=\"reviewer\">\nReview every changed file.\n</attached_skill>\n</attached_skills>"
	if runtime.agent.System != baseSystem+wantBlock {
		t.Fatalf("system = %q, want clean base plus exact block", runtime.agent.System)
	}
	if attached.State == nil || !attachedSkillRef(attached.State.Skills, "reviewer").Attached {
		t.Fatalf("attach state = %+v", attached.State)
	}
	if len(events) != 1 || events[0].Kind != chatpkg.EventState || events[0].State == nil {
		t.Fatalf("attach events = %+v", events)
	}

	firstSystem := runtime.agent.System
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "attach reviewer"}, nil); err != nil {
		t.Fatal(err)
	}
	if runtime.agent.System != firstSystem {
		t.Fatal("idempotent attach accumulated another system block")
	}

	before, err := os.ReadFile(reviewerPath)
	if err != nil {
		t.Fatal(err)
	}
	detached, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "detach reviewer"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(reviewerPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.agent.System != baseSystem || detached.State == nil || attachedSkillRef(detached.State.Skills, "reviewer").Attached {
		t.Fatalf("detach state=%+v system=%q", detached.State, runtime.agent.System)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("detach rewrote or deactivated SKILL.md")
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "detach reviewer"}, nil); err != nil {
		t.Fatalf("idempotent detach: %v", err)
	}
}

func TestSkillsCommandRejectsStaleRuntimeAttachAfterUninstall(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	writeRuntimeSkill(t, "reviewer", "review every change", skill.StatusActive, "Review every changed file.")
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	ws := memory.Workspace{Dir: filepath.Join(os.Getenv("WAFFLE_HOME"), "workspace", "main")}
	if err := skill.DeactivateSkill(ctx, runtime.st.DB, ws, "reviewer"); err != nil {
		t.Fatal(err)
	}
	guard := runtime.st.SkillLifecycleGuard()
	attachments := &skill.Attachments{DB: runtime.st.DB, Workspace: ws, Lifecycle: guard}
	if err := skill.UninstallSkill(ctx, runtime.st.DB, ws, "reviewer", attachments, guard); err != nil {
		t.Fatal(err)
	}

	_, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "attach reviewer"}, nil)
	if err == nil {
		t.Fatal("stale runtime attach succeeded after uninstall")
	}
	var count int
	if err := runtime.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_skills WHERE session_id = ? AND skill_name = ?`, runtime.current.ID, "reviewer").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale runtime created attachment rows = %d, want zero", count)
	}
}

func TestSkillsCommandNormalizesAttachmentNameBeforeRuntimePersistence(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	writeRuntimeSkill(t, "reviewer", "review every change", skill.StatusActive, "Review every changed file.")
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "attach reviewer "}, nil); err != nil {
		t.Fatal(err)
	}
	names, err := (&skill.Attachments{DB: runtime.st.DB}).List(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"reviewer"}) {
		t.Fatalf("runtime persisted attachments = %v, want [reviewer]", names)
	}
}

func TestSkillsCommandRejectsInactiveMissingAndOversizeAtomically(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	writeRuntimeSkill(t, "reviewer", "review changes", skill.StatusActive, "Review every changed file.")
	writeRuntimeSkill(t, "draft", "not ready", skill.StatusInactive, "Do not load.")
	writeRuntimeSkill(t, "huge", "too large", skill.StatusActive, strings.Repeat("x", maxAttachedSkillContextBytes+1))
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	baseSystem := runtime.agent.System
	attachments := &skill.Attachments{DB: runtime.st.DB}
	for _, name := range []string{"draft", "missing", "huge"} {
		_, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "attach " + name}, nil)
		if err == nil {
			t.Fatalf("attach %q succeeded", name)
		}
		if runtime.agent.System != baseSystem {
			t.Fatalf("attach %q mutated prompt", name)
		}
		got, listErr := attachments.List(ctx, state.SessionID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(got) != 0 {
			t.Fatalf("attach %q persisted rows %v", name, got)
		}
	}
}

func TestSkillsCommandPersistenceFailuresLeavePromptAndStateUnchanged(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	writeRuntimeSkill(t, "reviewer", "review changes", skill.StatusActive, "Review every changed file.")
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	baseSystem := runtime.agent.System
	if _, err := runtime.st.DB.ExecContext(ctx, `
		CREATE TRIGGER reject_session_skill_insert
		BEFORE INSERT ON session_skills
		BEGIN SELECT RAISE(ABORT, 'attachment write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "attach reviewer"}, nil); err == nil {
		t.Fatal("attach succeeded despite database trigger")
	}
	if runtime.agent.System != baseSystem || attachedSkillRef(runtime.stateLocked(nil).Skills, "reviewer").Attached {
		t.Fatal("failed attach mutated runtime state")
	}
	if _, err := runtime.st.DB.ExecContext(ctx, `DROP TRIGGER reject_session_skill_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "attach reviewer"}, nil); err != nil {
		t.Fatal(err)
	}
	attachedSystem := runtime.agent.System
	if _, err := runtime.st.DB.ExecContext(ctx, `
		CREATE TRIGGER reject_session_skill_delete
		BEFORE DELETE ON session_skills
		BEGIN SELECT RAISE(ABORT, 'attachment delete failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "detach reviewer"}, nil); err == nil {
		t.Fatal("detach succeeded despite database trigger")
	}
	if runtime.agent.System != attachedSystem || !attachedSkillRef(runtime.stateLocked(nil).Skills, "reviewer").Attached {
		t.Fatal("failed detach mutated runtime state")
	}
	names, err := (&skill.Attachments{DB: runtime.st.DB}).List(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"reviewer"}) {
		t.Fatalf("persisted attachments = %v", names)
	}
}

func TestAttachedSkillOpenResumeAndMissingState(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	writeRuntimeSkill(t, "reviewer", "review changes", skill.StatusActive, "Review every changed file.")
	current, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := sessions.Create(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	attachments := &skill.Attachments{DB: runtime.st.DB}
	if err := attachments.Attach(ctx, target.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	baseSystem := runtime.agent.System

	resumed, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State == nil || !attachedSkillRef(resumed.State.Skills, "reviewer").Attached ||
		strings.Count(runtime.agent.System, `<attached_skill name="reviewer">`) != 1 {
		t.Fatalf("resumed state=%+v system=%q", resumed.State, runtime.agent.System)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: current.SessionID}, nil); err != nil {
		t.Fatal(err)
	}
	if runtime.agent.System != baseSystem || attachedSkillRef(runtime.stateLocked(nil).Skills, "reviewer").Attached {
		t.Fatal("resuming original session retained target attachments")
	}

	reopened := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	state, err := reopened.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !attachedSkillRef(state.Skills, "reviewer").Attached || !strings.Contains(reopened.agent.System, "Review every changed file.") {
		t.Fatalf("reopened state=%+v system=%q", state, reopened.agent.System)
	}
	if err := skill.SetSkillStatus(ctx, runtime.st.DB, "reviewer", skill.StatusInactive, "test"); err != nil {
		t.Fatal(err)
	}
	missing := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	missingState, err := missing.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	ref := attachedSkillRef(missingState.Skills, "reviewer")
	if !ref.Attached || !ref.Missing || strings.Contains(missing.agent.System, "Review every changed file.") {
		t.Fatalf("missing state=%+v system=%q", missingState, missing.agent.System)
	}
	listed, err := missing.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed.Text, "unavailable") || !strings.Contains(listed.Text, "/skills detach reviewer") {
		t.Fatalf("missing guidance = %q", listed.Text)
	}
}

func TestAttachedSkillTransitionsReplacePromptAtomically(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	skillPath := writeRuntimeSkill(t, "reviewer", "review changes", skill.StatusActive, "Review every changed file.")
	opened, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "attach reviewer"}, nil); err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("request new = %+v, %v", result, err)
	}
	created, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: chatNewConfirmArg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.State == nil || attachedSkillRef(created.State.Skills, "reviewer").Attached ||
		strings.Contains(runtime.agent.System, "<attached_skills>") {
		t.Fatalf("new state=%+v system=%q", created.State, runtime.agent.System)
	}

	target, err := sessions.Create(ctx, "repo target")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&skill.Attachments{DB: runtime.st.DB}).Attach(ctx, target.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	runtime.profileAgentBuilder = func(context.Context, string) (*agent.Agent, func(), error) {
		return &agent.Agent{
			Provider: runtime.agent.Provider, Tools: tool.NewRegistry(runtimeNamedTool("host")),
			System: "clean repo profile", Model: "claude", Profile: "main",
		}, func() {}, nil
	}
	repoResult, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "repo", Repo: "owner/repo", Image: "test", SessionID: target.ID},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace")), client: &runtimeTestCloser{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if repoResult.State == nil || !attachedSkillRef(repoResult.State.Skills, "reviewer").Attached ||
		strings.Count(runtime.agent.System, `<attached_skill name="reviewer">`) != 1 ||
		!strings.HasPrefix(runtime.agent.System, "clean repo profile") {
		t.Fatalf("repo state=%+v system=%q", repoResult.State, runtime.agent.System)
	}

	oldSession, oldSystem := runtime.current, runtime.agent.System
	failedTarget, err := sessions.Create(ctx, "unreadable")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&skill.Attachments{DB: runtime.st.DB}).Attach(ctx, failedTarget.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(skillPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: failedTarget.ID}, nil); err == nil {
		t.Fatal("resume with unreadable attached skill succeeded")
	}
	if runtime.current != oldSession || runtime.agent.System != oldSystem || runtime.current.ID == opened.SessionID {
		t.Fatal("failed resume mutated session or prompt")
	}
}

func TestSkillsCommandSocketParity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	writeRuntimeSkill(t, "reviewer", "review changes", skill.StatusActive, "Review every changed file.")

	socketDir, err := os.MkdirTemp("/tmp", "waffle-chat-skills-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	listener, err := net.Listen("unix", filepath.Join(socketDir, "chat.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- chatwire.Serve(ctx, listener, func(context.Context) (chatpkg.Backend, error) { return runtime, nil }, nil)
	}()
	client, err := chatwire.Dial(ctx, filepath.Join(socketDir, "chat.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ready, err := client.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ref := attachedSkillRef(ready.Skills, "reviewer"); ref.Name == "" || ref.Attached {
		t.Fatalf("ready skills = %+v", ready.Skills)
	}
	var events []chatpkg.Event
	result, err := client.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandSkills, Args: "attach reviewer"}, func(event chatpkg.Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || !attachedSkillRef(result.State.Skills, "reviewer").Attached {
		t.Fatalf("socket result = %+v", result)
	}
	if len(events) != 1 || events[0].State == nil || !attachedSkillRef(events[0].State.Skills, "reviewer").Attached {
		t.Fatalf("socket events = %+v", events)
	}
	if strings.Count(runtime.agent.System, `<attached_skill name="reviewer">`) != 1 {
		t.Fatalf("socket runtime system = %q", runtime.agent.System)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = listener.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chatwire serve did not stop")
	}
}

func TestAttachedSkillOpenDoesNotHoldRuntimeLockDuringSkillRead(t *testing.T) {
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	path := filepath.Join(os.Getenv("WAFFLE_HOME"), "workspace", "main", "skills", "slow", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	openDone := make(chan error, 1)
	go func() {
		_, err := runtime.Open(context.Background(), chatpkg.OpenOptions{})
		openDone <- err
	}()
	select {
	case err := <-openDone:
		t.Fatalf("Open returned before blocked skill read: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	closeDone := make(chan error, 1)
	closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Second)
	defer cancelClose()
	go func() { closeDone <- runtime.Close(closeCtx) }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close during skill read: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			_, _ = writer.WriteString("---\nname: slow\nstatus: active\n---\n\nslow\n")
			_ = writer.Close()
		}
		<-openDone
		<-closeDone
		t.Fatal("Close blocked on runtime mutex during skill body read")
	}

	writer, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString("---\nname: slow\nstatus: active\n---\n\nslow\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-openDone; err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Open after concurrent Close error = %v", err)
	}
}

func TestAttachedSkillNewDoesNotHoldRuntimeLockDuringPersistence(t *testing.T) {
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	tx, err := runtime.st.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	resetDone := make(chan error, 1)
	go func() {
		_, err := runtime.resetSession(context.Background())
		resetDone <- err
	}()
	select {
	case err := <-resetDone:
		_ = tx.Rollback()
		t.Fatalf("reset returned before blocked persistence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancelDone := make(chan struct{})
	go func() {
		runtime.Cancel()
		close(cancelDone)
	}()
	select {
	case <-cancelDone:
	case <-time.After(250 * time.Millisecond):
		_ = tx.Rollback()
		<-resetDone
		<-cancelDone
		t.Fatal("Cancel blocked on runtime mutex during new-session persistence")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-resetDone; err != nil {
		t.Fatal(err)
	}
}

func TestChatRuntimeInvalidModelIsAtomic(t *testing.T) {
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(context.Background(), chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Command(context.Background(), chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "missing"}, nil)
	if err == nil || runtime.agent.Model != state.ModelAlias {
		t.Fatalf("model=%q err=%v", runtime.agent.Model, err)
	}
	saved, getErr := sessions.Get(context.Background(), state.SessionID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if saved.ModelAlias != "" {
		t.Fatalf("invalid model persisted %q", saved.ModelAlias)
	}
}

func TestChatRuntimeModelSelectionPreservesCapabilities(t *testing.T) {
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	runtime.capabilities = []string{"model-picker", "repo-workspaces"}

	result, err := runtime.Command(context.Background(), chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || !reflect.DeepEqual(result.State.Capabilities, runtime.capabilities) {
		t.Fatalf("model result capabilities = %+v, want %v", result.State, runtime.capabilities)
	}
}

func TestChatRuntimeConsecutiveRepoInstallsUseCleanProfileBaselines(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	runtime.agent.Model = "gpt"

	builds := 0
	cleanups := make([]int, 3)
	runtime.profileAgentBuilder = func(context.Context, string) (*agent.Agent, func(), error) {
		builds++
		index := builds
		return &agent.Agent{
			Provider: runtime.agent.Provider,
			Tools: tool.NewRegistry(
				runtimeNamedTool("host_keep"),
				runtimeNamedTool("repo_a_denied"),
			),
			System:  "clean profile baseline",
			Model:   "claude",
			Profile: "main",
		}, func() { cleanups[index]++ }, nil
	}

	targetA, err := sessions.Create(ctx, "repo a")
	if err != nil {
		t.Fatal(err)
	}
	targetB, err := sessions.Create(ctx, "repo b")
	if err != nil {
		t.Fatal(err)
	}
	clientA := &runtimeTestCloser{}
	if _, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "a", Repo: "owner/a", Image: "test", SessionID: targetA.ID},
		policy:    &repopolicy.Policy{Body: "REPO_A_POLICY", Tools: repopolicy.ToolFilter{Deny: []string{"repo_a_denied"}}},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_a")),
		client:    clientA,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if runtime.agent.Model != "claude" {
		t.Fatalf("repo A model = %q, want clean profile default claude", runtime.agent.Model)
	}
	if !strings.Contains(runtime.agent.System, "REPO_A_POLICY") || hasTool(runtime.agent.Tools, "repo_a_denied") {
		t.Fatalf("repo A system=%q tools=%v", runtime.agent.System, toolNames(runtime.agent.Tools))
	}

	clientB := &runtimeTestCloser{}
	if _, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "b", Repo: "owner/b", Image: "test", SessionID: targetB.ID},
		policy:    &repopolicy.Policy{Body: "REPO_B_POLICY"},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_b")),
		client:    clientB,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Fatalf("profile baseline builds = %d, want 2", builds)
	}
	if strings.Contains(runtime.agent.System, "REPO_A_POLICY") || !strings.Contains(runtime.agent.System, "REPO_B_POLICY") {
		t.Fatalf("repo B inherited repo A system prompt: %q", runtime.agent.System)
	}
	if !hasTool(runtime.agent.Tools, "repo_a_denied") || hasTool(runtime.agent.Tools, "workspace_a") || !hasTool(runtime.agent.Tools, "workspace_b") {
		t.Fatalf("repo B inherited repo A tools/policy: %v", toolNames(runtime.agent.Tools))
	}
	if cleanups[1] != 1 || cleanups[2] != 0 || clientA.closed != 1 || clientB.closed != 0 {
		t.Fatalf("cleanup after repo B: agents=%v clients=(%d,%d)", cleanups, clientA.closed, clientB.closed)
	}
}

func TestChatRuntimeRepoSwapCancellationRetainsOldResourcesUntilCloseRetry(t *testing.T) {
	ctx := context.Background()
	owners := newChatSessionOwners()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	runtime.sessionOwners = owners
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}

	originalCleanup := runtime.agentCleanupContext
	oldAgentCalls := 0
	liveCleanupAttempts := 0
	retryErr := errors.New("old agent cleanup needs retry")
	runtime.agentCleanupContext = func(cleanupCtx context.Context) error {
		oldAgentCalls++
		if err := cleanupCtx.Err(); err != nil {
			return err
		}
		liveCleanupAttempts++
		if liveCleanupAttempts == 1 {
			return retryErr
		}
		return originalCleanup(cleanupCtx)
	}
	swapCtx, cancelSwap := context.WithCancel(ctx)
	oldClient := &runtimeContextTestCloser{beforeClose: cancelSwap}
	runtime.wsClient = oldClient

	replacementCleanupCalls := 0
	runtime.profileAgentBuilder = func(context.Context, string) (*agent.Agent, func(), error) {
		return &agent.Agent{
			Provider: runtime.agent.Provider,
			Tools:    tool.NewRegistry(runtimeNamedTool("replacement")),
			System:   "replacement",
			Model:    "claude",
			Profile:  "main",
		}, func() { replacementCleanupCalls++ }, nil
	}
	target, err := sessions.Create(ctx, "repo target")
	if err != nil {
		t.Fatal(err)
	}
	newClient := &runtimeTestCloser{}
	if _, err := runtime.installRepo(swapCtx, repoInstall{
		workspace: &workspace.Workspace{ID: "target", Repo: "owner/target", Image: "test", SessionID: target.ID},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_target")),
		client:    newClient,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if oldClient.calls != 1 || oldClient.closed {
		t.Fatalf("cancelled immediate client cleanup calls=%d closed=%t, want 1 false", oldClient.calls, oldClient.closed)
	}
	if oldAgentCalls != 1 {
		t.Fatalf("cancelled immediate agent cleanup calls = %d, want 1", oldAgentCalls)
	}

	if err := runtime.Close(ctx); !errors.Is(err, retryErr) {
		t.Fatalf("first final Close error = %v, want retryable old cleanup error", err)
	}
	if oldClient.calls != 2 || !oldClient.closed {
		t.Fatalf("first final client cleanup calls=%d closed=%t, want 2 true", oldClient.calls, oldClient.closed)
	}
	if oldAgentCalls != 2 {
		t.Fatalf("first final agent cleanup calls = %d, want 2", oldAgentCalls)
	}
	if newClient.closed != 1 || replacementCleanupCalls != 1 {
		t.Fatalf("active replacement cleanup client=%d agent=%d, want 1 1", newClient.closed, replacementCleanupCalls)
	}

	contender := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	contender.sessionOwners = owners
	if _, err := contender.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID}); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("session ownership released before retired cleanup succeeded: %v", err)
	}

	if err := runtime.Close(ctx); err != nil {
		t.Fatalf("explicit final cleanup retry: %v", err)
	}
	if oldClient.calls != 2 {
		t.Fatalf("successful old client cleanup repeated: calls = %d, want 2", oldClient.calls)
	}
	if oldAgentCalls != 3 {
		t.Fatalf("old agent cleanup calls after retry = %d, want 3", oldAgentCalls)
	}
	reacquired := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	reacquired.sessionOwners = owners
	if _, err := reacquired.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID}); err != nil {
		t.Fatalf("reacquire after retired cleanup succeeded: %v", err)
	}
}

func TestChatRuntimeRepoSwapPartialRetiredCleanupDoesNotRepeatSuccess(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}

	oldClientCloses := 0
	runtime.wsClient = runtimeNonComparableCloser{&oldClientCloses}
	originalCleanup := runtime.agentCleanupContext
	oldAgentCalls := 0
	retryErr := errors.New("retry old agent cleanup")
	runtime.agentCleanupContext = func(cleanupCtx context.Context) error {
		oldAgentCalls++
		if oldAgentCalls == 1 {
			return retryErr
		}
		return originalCleanup(cleanupCtx)
	}
	runtime.profileAgentBuilder = func(context.Context, string) (*agent.Agent, func(), error) {
		return &agent.Agent{
			Provider: runtime.agent.Provider,
			Tools:    tool.NewRegistry(runtimeNamedTool("replacement")),
			System:   "replacement",
			Model:    "claude",
			Profile:  "main",
		}, func() {}, nil
	}
	target, err := sessions.Create(ctx, "repo target")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "target", Repo: "owner/target", Image: "test", SessionID: target.ID},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_target")),
		client:    &runtimeTestCloser{},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if oldClientCloses != 1 || oldAgentCalls != 1 {
		t.Fatalf("immediate retired cleanup client=%d agent=%d, want 1 1", oldClientCloses, oldAgentCalls)
	}

	if err := runtime.Close(ctx); err != nil {
		t.Fatalf("final Close retry: %v", err)
	}
	if oldClientCloses != 1 {
		t.Fatalf("completed retired client cleanup repeated: %d", oldClientCloses)
	}
	if oldAgentCalls != 2 {
		t.Fatalf("retired agent cleanup calls = %d, want 2", oldAgentCalls)
	}
	runtime.mu.Lock()
	if len(runtime.retiredCleanup) != 0 {
		retiredCount := len(runtime.retiredCleanup)
		runtime.mu.Unlock()
		t.Fatalf("completed retired cleanup entries = %d, want 0", retiredCount)
	}
	backing := runtime.retiredCleanup[:cap(runtime.retiredCleanup)]
	for i, cleanup := range backing {
		if cleanup != nil {
			runtime.mu.Unlock()
			t.Fatalf("completed retired cleanup retained in backing slot %d", i)
		}
	}
	runtime.mu.Unlock()
}

func TestChatRuntimeUnboundRepoUsesOriginalChatProfileAfterBoundRepo(t *testing.T) {
	ctx := context.Background()
	cfg := configuredChatModels()
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"chat-default": {Model: "claude", Tools: config.ToolPolicy{Deny: []string{"chat_denied"}}},
		"repo-a":       {Model: "gpt", Tools: config.ToolPolicy{Deny: []string{"repo_a_denied"}}},
	}
	runtime, sessions := newRuntimeFixture(t, cfg)
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{Profile: "chat-default"}); err != nil {
		t.Fatal(err)
	}

	var builtProfiles []string
	runtime.profileAgentBuilder = func(_ context.Context, profileName string) (*agent.Agent, func(), error) {
		builtProfiles = append(builtProfiles, profileName)
		model := "claude"
		if profileName == "repo-a" {
			model = "gpt"
		}
		return &agent.Agent{
			Provider: runtime.agent.Provider,
			Tools: tool.NewRegistry(
				runtimeNamedTool("host_keep"),
				runtimeNamedTool("chat_denied"),
				runtimeNamedTool("repo_a_denied"),
			),
			System:  "profile baseline: " + profileName,
			Model:   model,
			Profile: profileName,
		}, func() {}, nil
	}

	targetA, err := sessions.Create(ctx, "bound repo a")
	if err != nil {
		t.Fatal(err)
	}
	targetB, err := sessions.Create(ctx, "unbound repo b")
	if err != nil {
		t.Fatal(err)
	}
	resultA, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "a", Repo: "owner/a", Image: "test", SessionID: targetA.ID, Profile: "repo-a"},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_a")),
		client:    &runtimeTestCloser{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resultA.State == nil || resultA.State.Profile != "repo-a" || runtime.agent.Model != "gpt" || hasTool(runtime.agent.Tools, "repo_a_denied") {
		t.Fatalf("bound repo A state=%+v model=%q tools=%v", resultA.State, runtime.agent.Model, toolNames(runtime.agent.Tools))
	}

	resultB, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "b", Repo: "owner/b", Image: "test", SessionID: targetB.ID},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_b")),
		client:    &runtimeTestCloser{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(builtProfiles, []string{"repo-a", "chat-default"}) {
		t.Fatalf("built profiles = %v, want bound repo then original chat default", builtProfiles)
	}
	if resultB.State == nil || resultB.State.Profile != "chat-default" || runtime.agent.Model != "claude" {
		t.Fatalf("unbound repo B state=%+v model=%q", resultB.State, runtime.agent.Model)
	}
	if !strings.Contains(runtime.agent.System, "profile baseline: chat-default") || strings.Contains(runtime.agent.System, "profile baseline: repo-a") {
		t.Fatalf("unbound repo B system = %q", runtime.agent.System)
	}
	if hasTool(runtime.agent.Tools, "chat_denied") || !hasTool(runtime.agent.Tools, "repo_a_denied") || hasTool(runtime.agent.Tools, "workspace_a") {
		t.Fatalf("unbound repo B inherited repo A policy/tools: %v", toolNames(runtime.agent.Tools))
	}
}

func TestChatRuntimeFailedRepoInstallCleansProvisionalResources(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	oldAgent, oldSession := runtime.agent, runtime.current
	provisionalCleanups := 0
	runtime.profileAgentBuilder = func(context.Context, string) (*agent.Agent, func(), error) {
		return &agent.Agent{
			Provider: oldAgent.Provider,
			Tools:    tool.NewRegistry(runtimeNamedTool("host_keep")),
			System:   "provisional",
			Model:    "claude",
			Profile:  "main",
		}, func() { provisionalCleanups++ }, nil
	}
	client := &runtimeTestCloser{}

	_, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "missing", Repo: "owner/missing", Image: "test", SessionID: "missing-session"},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_missing")),
		client:    client,
	}, nil)
	if !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("install err = %v, want session not found", err)
	}
	if provisionalCleanups != 1 || client.closed != 1 {
		t.Fatalf("provisional cleanup: agent=%d client=%d", provisionalCleanups, client.closed)
	}
	if runtime.agent != oldAgent || runtime.current != oldSession || runtime.wsClient != nil {
		t.Fatal("failed repo install mutated active runtime state")
	}
}

func TestChatRuntimeFailedRepoAgentBuildCleansReturnedResources(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("profile agent build failed")
	provisionalCleanups := 0
	runtime.profileAgentBuilder = func(context.Context, string) (*agent.Agent, func(), error) {
		return nil, func() { provisionalCleanups++ }, wantErr
	}
	client := &runtimeTestCloser{}

	_, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "failed", Repo: "owner/failed", Image: "test", SessionID: "unused"},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_failed")),
		client:    client,
	}, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("install err = %v, want %v", err, wantErr)
	}
	if provisionalCleanups != 1 || client.closed != 1 {
		t.Fatalf("failed build cleanup: agent=%d client=%d", provisionalCleanups, client.closed)
	}
}

func TestChatRuntimeModelDatabaseFailureIsAtomic(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_model_alias_update
		BEFORE UPDATE OF model_alias ON sessions
		BEGIN SELECT RAISE(ABORT, 'model write failed'); END`); err != nil {
		t.Fatal(err)
	}

	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil)
	if err == nil || runtime.agent.Model != state.ModelAlias {
		t.Fatalf("model=%q err=%v", runtime.agent.Model, err)
	}
	saved, getErr := sessions.Get(ctx, state.SessionID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if saved.ModelAlias != "" {
		t.Fatalf("failed model write persisted %q", saved.ModelAlias)
	}
}

func TestChatRuntimeRemovedPersistedModelRequiresReplacement(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}

	reduced := configuredChatModels()
	delete(reduced.Models, "gpt")
	second := newRuntimeAgainstSameStore(t, reduced, sessions)
	resumed, err := second.Open(ctx, chatpkg.OpenOptions{SessionID: state.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ModelAlias != "gpt" || resumed.ModelError == "" {
		t.Fatalf("resumed = %+v, want unavailable gpt with model error", resumed)
	}
	if second.agent.Model != "claude" {
		t.Fatalf("agent silently selected %q, want configured default unchanged", second.agent.Model)
	}
	if len(resumed.Models) != 1 || resumed.Models[0].Alias != "claude" {
		t.Fatalf("picker models = %+v", resumed.Models)
	}
	if err := second.Turn(ctx, "must not run", nil); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("Turn while model unavailable err = %v", err)
	}
	result, err := second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "claude"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || result.State.ModelError != "" || result.State.ModelAlias != "claude" {
		t.Fatalf("replacement result = %+v", result)
	}
}

func TestChatRuntimeTurnEmitsHooksAndPersistsHistory(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{
		{response: llm.Response{
			StopReason: llm.StopToolUse,
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
				Type:    llm.BlockToolUse,
				ToolUse: &llm.ToolUse{ID: "call-1", Name: "runtime_test", Input: json.RawMessage(`{"ok":true}`)},
			}}},
			Usage: llm.Usage{InputTokens: 3, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30},
		}},
		{response: llm.Response{
			StopReason: llm.StopEndTurn,
			Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "done"}}},
			Usage:      llm.Usage{InputTokens: 2, OutputTokens: 7, CacheReadInputTokens: 40},
		}, stream: "done"},
	}}
	runtime.agent.Tools = tool.NewRegistry(runtimeTestTool{})

	var events []chatpkg.Event
	if err := runtime.Turn(ctx, "run the tool", func(event chatpkg.Event) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	wantKinds := []chatpkg.EventKind{
		chatpkg.EventToolStarted, chatpkg.EventToolFinished,
		chatpkg.EventTextDelta, chatpkg.EventTurnDone,
	}
	for _, want := range wantKinds {
		if !eventKinds(events)[want] {
			t.Fatalf("events = %+v, missing %s", events, want)
		}
	}
	var started, finished chatpkg.Event
	for _, event := range events {
		switch event.Kind {
		case chatpkg.EventToolStarted:
			started = event
		case chatpkg.EventToolFinished:
			finished = event
		}
	}
	if started.ToolCallID == "" || finished.ToolCallID != started.ToolCallID {
		t.Fatalf("tool events = start %+v finish %+v, want one stable opaque call ID", started, finished)
	}
	if finished.DurationMS < 0 {
		t.Fatalf("tool duration = %dms, want a non-negative server measurement", finished.DurationMS)
	}
	done := events[len(events)-1]
	if done.Kind != chatpkg.EventTurnDone || done.Usage != (llm.Usage{InputTokens: 5, OutputTokens: 12, CacheCreationInputTokens: 20, CacheReadInputTokens: 70}) {
		t.Fatalf("turn_done = %+v", done)
	}
	turns, err := sessions.Turns(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != len(runtime.history) || len(turns) != 4 || runtime.persisted != 4 {
		t.Fatalf("persisted turns=%d history=%d index=%d", len(turns), len(runtime.history), runtime.persisted)
	}
	saved, err := sessions.Get(ctx, state.SessionID)
	if err != nil || saved.Title != "run the tool" {
		t.Fatalf("title=%q err=%v", saved.Title, err)
	}
}

func TestChatRuntimeTurnPersistsUserMessageAndDoneEventOnRunError(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: errors.New("provider failed")}}}
	var events []chatpkg.Event
	err = runtime.Turn(ctx, "keep this", func(event chatpkg.Event) { events = append(events, event) })
	if err == nil || err.Error() != "provider failed" {
		t.Fatalf("Turn err = %v", err)
	}
	turns, loadErr := sessions.Turns(ctx, state.SessionID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(turns) != 1 || turns[0].Text() != "keep this" {
		t.Fatalf("persisted turns = %+v", turns)
	}
	if len(events) == 0 || events[len(events)-1].Kind != chatpkg.EventTurnDone || !events[len(events)-1].IsError {
		t.Fatalf("events = %+v", events)
	}
}

func TestChatRuntimeCancelOnlyCancelsActiveTurn(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	provider := &runtimeBlockingProvider{started: make(chan struct{})}
	runtime.agent.Provider = provider
	errCh := make(chan error, 1)
	go func() { errCh <- runtime.Turn(ctx, "wait", nil) }()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	if err := runtime.Turn(ctx, "overlap", nil); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("overlapping Turn err = %v", err)
	}
	runtime.Cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Turn err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active turn was not canceled")
	}
	runtime.Cancel()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.agentCancel != nil {
		t.Fatal("active cancellation was not cleared")
	}
}

func TestChatRuntimeActiveNewOneConfirmationPreservesOldHistory(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &runtimeBlockingProvider{started: make(chan struct{})}
	runtime.agent.Provider = provider
	turnDone := make(chan error, 1)
	go func() { turnDone <- runtime.Turn(ctx, "preserve this", nil) }()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil)
	if err != nil || !result.Confirm {
		t.Fatalf("active /new = %+v, %v", result, err)
	}
	result, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: chatNewConfirmArg}, nil)
	if err != nil {
		t.Fatalf("single confirmation: %v", err)
	}
	if err := <-turnDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("active turn error = %v", err)
	}
	if result.State == nil || result.State.SessionID == state.SessionID {
		t.Fatalf("new state = %+v", result.State)
	}
	turns, err := sessions.Turns(ctx, state.SessionID)
	if err != nil || len(turns) != 1 || turns[0].Text() != "preserve this" {
		t.Fatalf("old turns = %+v, %v", turns, err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: chatNewConfirmArg}, nil); err == nil {
		t.Fatal("stale confirmation succeeded")
	}
}

func TestChatRuntimeCommandResults(t *testing.T) {
	tests := []struct {
		name    string
		command chatpkg.ParsedCommand
		prepare func(*testing.T, *chatRuntime, *session.Store)
		check   func(*testing.T, chatpkg.Result, *chatRuntime)
	}{
		{
			name: "help", command: chatpkg.ParsedCommand{Name: chatpkg.CommandHelp},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Chat commands" || !reflect.DeepEqual(got.Commands, chatpkg.Commands()) {
					t.Fatalf("help result = %+v", got)
				}
			},
		},
		{
			name: "model picker", command: chatpkg.ParsedCommand{Name: chatpkg.CommandModel},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Choose a model" || aliases(got.Models) != "claude,gpt" {
					t.Fatalf("model picker = %+v", got)
				}
			},
		},
		{
			name: "models", command: chatpkg.ParsedCommand{Name: chatpkg.CommandModels},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Configured models" || aliases(got.Models) != "claude,gpt" || got.Models[0].Provider != "local" || got.Models[0].Upstream != "upstream-claude" {
					t.Fatalf("models result = %+v", got)
				}
			},
		},
		{
			name: "new", command: chatpkg.ParsedCommand{Name: chatpkg.CommandNew},
			prepare: func(t *testing.T, runtime *chatRuntime, _ *session.Store) {
				t.Helper()
				runtime.history = []llm.Message{llm.UserText("old history")}
			},
			check: func(t *testing.T, got chatpkg.Result, runtime *chatRuntime) {
				t.Helper()
				if !got.Confirm || got.State != nil || len(runtime.history) != 1 {
					t.Fatalf("new result = %+v history=%d", got, len(runtime.history))
				}
			},
		},
		{
			name: "new confirmed", command: chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"},
			prepare: func(t *testing.T, runtime *chatRuntime, _ *session.Store) {
				t.Helper()
				runtime.history = []llm.Message{llm.UserText("old history")}
				result, err := runtime.Command(context.Background(), chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil)
				if err != nil || !result.Confirm {
					t.Fatalf("request confirmation result=%+v err=%v", result, err)
				}
			},
			check: func(t *testing.T, got chatpkg.Result, runtime *chatRuntime) {
				t.Helper()
				if got.Confirm || got.State == nil || got.State.SessionID == "" || len(runtime.history) != 0 {
					t.Fatalf("confirmed new result = %+v history=%d", got, len(runtime.history))
				}
			},
		},
		{
			name: "sessions", command: chatpkg.ParsedCommand{Name: chatpkg.CommandSessions},
			prepare: func(t *testing.T, _ *chatRuntime, sessions *session.Store) {
				t.Helper()
				if _, err := sessions.Create(context.Background(), "second"); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Recent sessions" || len(got.Sessions) != 2 {
					t.Fatalf("sessions result = %+v", got)
				}
			},
		},
		{
			name: "resume picker", command: chatpkg.ParsedCommand{Name: chatpkg.CommandResume},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Resume a session" || len(got.Sessions) != 1 {
					t.Fatalf("resume picker = %+v", got)
				}
			},
		},
		{
			name: "status", command: chatpkg.ParsedCommand{Name: chatpkg.CommandStatus},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Chat status" || got.State == nil || got.State.ConnectionMode != "direct" || got.State.Profile != "main" || got.State.ProviderLabel != "local (openai)" {
					t.Fatalf("status result = %+v", got)
				}
			},
		},
		{
			name: "usage", command: chatpkg.ParsedCommand{Name: chatpkg.CommandUsage},
			prepare: func(t *testing.T, runtime *chatRuntime, sessions *session.Store) {
				t.Helper()
				ctx := context.Background()
				usageStore := usagepkg.New(runtime.st)
				if err := usageStore.AddRequestAt(ctx, runtime.current.ID, llm.Usage{InputTokens: 2, OutputTokens: 3}, time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
				other, err := sessions.Create(ctx, "other")
				if err != nil {
					t.Fatal(err)
				}
				if err := usageStore.AddRequestAt(ctx, other.ID, llm.Usage{InputTokens: 4, OutputTokens: 5}, time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				want := "Current session totals: requests=2 input=4 cache_write=0 cache_read=0 output=6 reserved=0 tunnel_bytes=0\nPersisted aggregate totals: requests=4 input=12 cache_write=0 cache_read=0 output=16 reserved=0 tunnel_bytes=0"
				if got.Title != "Usage" || len(got.Usage) != 6 || got.Text != want {
					t.Fatalf("usage result = %+v\ntext=%q", got, got.Text)
				}
			},
		},
		{
			name: "permissions", command: chatpkg.ParsedCommand{Name: chatpkg.CommandPermissions},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Effective permissions" || got.Permissions == nil || got.Permissions.SandboxMode != "host" {
					t.Fatalf("permissions result = %+v", got)
				}
			},
		},
		{
			name: "skill", command: chatpkg.ParsedCommand{Name: chatpkg.CommandSkill, Args: "audit fast"},
			prepare: func(t *testing.T, runtime *chatRuntime, _ *session.Store) {
				t.Helper()
				path := filepath.Join(t.TempDir(), "SKILL.md")
				if err := os.WriteFile(path, []byte("---\nname: audit\n---\nInspect carefully."), 0o600); err != nil {
					t.Fatal(err)
				}
				runtime.skills = []skill.Skill{{Name: "audit", Path: path}}
				runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{response: llm.Response{
					StopReason: llm.StopEndTurn,
					Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "audited"}}},
				}}}}
			},
			check: func(t *testing.T, got chatpkg.Result, runtime *chatRuntime) {
				t.Helper()
				if got.Text != "skill audit completed" || len(runtime.history) != 2 || !strings.Contains(runtime.history[0].Text(), "User arguments: fast") {
					t.Fatalf("skill result = %+v history=%+v", got, runtime.history)
				}
			},
		},
		{
			name: "workset", command: chatpkg.ParsedCommand{Name: chatpkg.CommandWorkset},
			prepare: func(t *testing.T, runtime *chatRuntime, _ *session.Store) {
				t.Helper()
				ws := &workset.Store{DB: runtime.st.DB}
				if _, err := ws.Add(context.Background(), runtime.current.ID, workset.KindGoal, "finish task", workset.SourceUser, true); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Working set" || len(got.Workset) != 1 || got.Workset[0].Text != "finish task" {
					t.Fatalf("workset result = %+v", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, sessions := newRuntimeFixture(t, configuredChatModels())
			if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
				t.Fatal(err)
			}
			if tt.prepare != nil {
				tt.prepare(t, runtime, sessions)
			}
			got, err := runtime.Command(context.Background(), tt.command, nil)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, got, runtime)
		})
	}
}

func TestChatRuntimeManageConversationCommands(t *testing.T) {
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	currentID := runtime.current.ID

	other, err := sessions.Create(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}

	// Rename with a bounded, space-containing title.
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandRename, Args: other.ID + " Release review"}, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := sessions.Get(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != "Release review" {
		t.Fatalf("renamed title = %q", loaded.Title)
	}

	// Pin sorts the conversation ahead of recents without touching recency.
	before, err := sessions.Get(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandPin, Args: other.ID}, nil); err != nil {
		t.Fatal(err)
	}
	list, err := sessions.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].ID != other.ID || !list[0].Pinned {
		t.Fatalf("pinned list head = %+v", list[0])
	}
	after, err := sessions.Get(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("pin changed updated_at: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandUnpin, Args: other.ID}, nil); err != nil {
		t.Fatal(err)
	}
	after, err = sessions.Get(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Pinned {
		t.Fatal("unpin did not persist")
	}

	// Delete the non-current conversation: no state swap, row is gone.
	got, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandDelete, Args: other.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Get(ctx, other.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("deleted session still readable: %v", err)
	}
	if got.State != nil {
		t.Fatalf("delete of another conversation returned state: %+v", got)
	}

	// Delete the current conversation: a fresh session replaces it.
	got, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandDelete, Args: currentID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == nil || got.State.SessionID == currentID || got.State.SessionID == "" {
		t.Fatalf("delete-current result = %+v", got)
	}

	// Usage and missing-target errors fail closed.
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandRename, Args: ""}, nil); err == nil {
		t.Fatal("rename without args should fail")
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandPin, Args: ""}, nil); err == nil {
		t.Fatal("pin without args should fail")
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandDelete, Args: ""}, nil); err == nil {
		t.Fatal("delete without args should fail")
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandDelete, Args: "missing-session"}, nil); err == nil {
		t.Fatal("delete of a missing session should fail")
	}
}

func TestChatRuntimeCommandUsageErrors(t *testing.T) {
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		command chatpkg.ParsedCommand
		want    string
	}{
		{chatpkg.ParsedCommand{Name: chatpkg.CommandSkill}, "usage: /skill <name> [args]"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandRepo}, "usage: /repo <owner/repo>"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandWorkset, Args: "replace only"}, "usage: /workset replace <id> <text>"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandWorkset, Args: "drop"}, "usage: /workset drop <id>"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandWorkset, Args: "clear extra"}, "usage: /workset clear"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandWorkset, Args: "wat"}, "usage: /workset [list|replace <id> <text>|drop <id>|clear]"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "now"}, "usage: /new"},
	} {
		_, err := runtime.Command(context.Background(), tt.command, nil)
		if err == nil || err.Error() != tt.want {
			t.Errorf("Command(%+v) err = %v, want %q", tt.command, err, tt.want)
		}
	}
}

func TestChatRuntimeNewConfirmationRejectsDirectAndStaleConfirmation(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"}, nil)
	if err == nil || err.Error() != "no pending /new confirmation" {
		t.Fatalf("direct confirmation err = %v", err)
	}
	if runtime.current.ID != state.SessionID {
		t.Fatal("direct confirmation changed the session")
	}
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("request confirmation result=%+v err=%v", result, err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"}, nil)
	if err == nil || err.Error() != "no pending /new confirmation" {
		t.Fatalf("stale confirmation after model change err = %v", err)
	}
	if runtime.current.ID != state.SessionID {
		t.Fatal("stale confirmation changed the session")
	}
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("second confirmation result=%+v err=%v", result, err)
	}
	runtime.Cancel()
	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"}, nil)
	if err == nil || err.Error() != "no pending /new confirmation" {
		t.Fatalf("stale confirmation after Cancel err = %v", err)
	}
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("third confirmation result=%+v err=%v", result, err)
	}
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{response: llm.Response{
		StopReason: llm.StopEndTurn,
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "changed"}}},
	}}}}
	if err := runtime.Turn(ctx, "intervening turn", nil); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"}, nil)
	if err == nil || err.Error() != "no pending /new confirmation" {
		t.Fatalf("stale confirmation after Turn err = %v", err)
	}
}

func TestChatRuntimeResumeLoadsBeforeMutatingStateAndRestoresModel(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	initial, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := sessions.Create(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetModelAlias(ctx, target.ID, "gpt"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.AppendTurn(ctx, target.ID, llm.UserText("earlier")); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || result.State.SessionID != target.ID || result.State.ModelAlias != "gpt" || runtime.agent.Model != "gpt" || len(runtime.history) != 1 {
		t.Fatalf("resume result = %+v agent=%q history=%d", result, runtime.agent.Model, len(runtime.history))
	}

	corrupt, err := sessions.Create(ctx, "corrupt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.DB().ExecContext(ctx, `
		INSERT INTO turns (session_id, seq, role, blocks, text, created_at)
		VALUES (?, 1, 'user', 'not json', '', ?)`, corrupt.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: corrupt.ID}, nil)
	if err == nil {
		t.Fatal("resume corrupt session succeeded")
	}
	if runtime.current.ID != target.ID || runtime.agent.Model != "gpt" || len(runtime.history) != 1 {
		t.Fatalf("failed resume mutated current=%s model=%s history=%d (initial=%s)", runtime.current.ID, runtime.agent.Model, len(runtime.history), initial.SessionID)
	}
}

func TestChatRuntimeExitWarnsOnReflectionFailureAndCleansUpOnce(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	runtime.history = []llm.Message{llm.UserText("question"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}}}
	runtime.persisted = 2
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: errors.New("reflection failed")}}}
	closed := 0
	runtime.wsClient = closeFunc(func() error { closed++; return nil })
	cleaned := 0
	runtime.agentCleanupContext = func(context.Context) error {
		cleaned++
		return nil
	}
	var events []chatpkg.Event
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandExit}, func(event chatpkg.Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if !result.ShouldClose || !strings.Contains(result.Text, "warning") || len(events) != 1 || events[0].Kind != chatpkg.EventNotice || !events[0].IsError {
		t.Fatalf("exit result=%+v events=%+v", result, events)
	}
	if closed != 1 || cleaned != 1 {
		t.Fatalf("cleanup counts client=%d agent=%d", closed, cleaned)
	}
	if err := runtime.Close(ctx); err == nil || !strings.Contains(err.Error(), "reflection failed") {
		t.Fatalf("Close after exit err = %v", err)
	}
	if closed != 1 || cleaned != 1 {
		t.Fatalf("cleanup repeated client=%d agent=%d", closed, cleaned)
	}
}

func TestChatRuntimeRedactsTurnFailureAndPreservesCause(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	const secret = "opaque-provider-canary-7391"
	providerErr := errors.New("provider failed with " + secret)
	runtime.agent.Redact = func(value string) string { return strings.ReplaceAll(value, secret, "[redacted:test]") }
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: providerErr}}}
	var events []chatpkg.Event
	err := runtime.Turn(ctx, "hello", func(event chatpkg.Event) { events = append(events, event) })
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted:test]") {
		t.Fatalf("Turn error = %v", err)
	}
	if !errors.Is(err, providerErr) {
		t.Fatalf("Turn error lost cause: %v", err)
	}
	for _, event := range events {
		if strings.Contains(event.Text, secret) {
			t.Fatalf("event leaked secret: %+v", event)
		}
	}
}

func TestRedactChatStateDoesNotMutateHistoryAndCoversToolPayloads(t *testing.T) {
	const secret = "opaque-history-canary-2551"
	toolUse := &llm.ToolUse{ID: secret, Name: "read", Input: json.RawMessage(`{"path":"` + secret + `"}`)}
	toolResult := &llm.ToolResult{ToolUseID: secret, Content: secret}
	history := []llm.Message{{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, Text: secret, ToolUse: toolUse}, {Type: llm.BlockToolResult, ToolResult: toolResult}}}}
	state := chatpkg.State{History: append([]llm.Message(nil), history...)}
	redacted := chatpkg.RedactState(state, func(value string) string { return strings.ReplaceAll(value, secret, "[redacted:test]") })
	if strings.Contains(fmt.Sprintf("%+v", redacted.History), secret) || strings.Contains(string(redacted.History[0].Blocks[0].ToolUse.Input), secret) {
		t.Fatalf("redacted history leaked secret: %+v", redacted.History)
	}
	if history[0].Blocks[0].Text != secret || history[0].Blocks[0].ToolUse.ID != secret || !strings.Contains(string(history[0].Blocks[0].ToolUse.Input), secret) || history[0].Blocks[1].ToolResult.Content != secret {
		t.Fatalf("redaction mutated runtime history: %+v", history)
	}
}

func TestAttachedSkillStateRedactionDoesNotMutateRuntimeRefs(t *testing.T) {
	const secret = "opaque-skill-description-canary-8264"
	state := chatpkg.State{Skills: []chatpkg.SkillRef{{Name: "reviewer", Description: secret, Attached: true}}}
	redacted := chatpkg.RedactState(state, func(value string) string {
		return strings.ReplaceAll(value, secret, "[redacted:test]")
	})
	if redacted.Skills[0].Description != "[redacted:test]" {
		t.Fatalf("redacted skills = %+v", redacted.Skills)
	}
	if state.Skills[0].Description != secret {
		t.Fatalf("redaction mutated source skills = %+v", state.Skills)
	}
}

func TestChatRuntimeRedactsReflectionWarning(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	const secret = "opaque-reflection-canary-4826"
	runtime.agent.Redact = func(value string) string { return strings.ReplaceAll(value, secret, "[redacted:test]") }
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: errors.New("reflection failed with " + secret)}}}
	runtime.history = []llm.Message{llm.UserText("question"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}}}
	runtime.persisted = len(runtime.history)
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("request /new = %+v, %v", result, err)
	}
	var events []chatpkg.Event
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: chatNewConfirmArg}, func(event chatpkg.Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	visible := result.Text
	for _, event := range events {
		visible += "\n" + event.Text
	}
	if strings.Contains(visible, secret) || !strings.Contains(visible, "[redacted:test]") {
		t.Fatalf("visible warning = %q", visible)
	}
}

func TestChatRuntimeSocketRedactsConfiguredReflectionCanary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	socketDir, err := os.MkdirTemp("/tmp", "waffle-chat-redact-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	path := filepath.Join(socketDir, "chat.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- chatwire.Serve(ctx, listener, func(context.Context) (chatpkg.Backend, error) { return runtime, nil }, nil)
	}()
	client, err := chatwire.Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	const secret = "opaque-socket-reflection-6017"
	runtime.agent.Redact = func(value string) string { return strings.ReplaceAll(value, secret, "[redacted:test]") }
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: errors.New("reflection failed with " + secret)}}}
	runtime.history = []llm.Message{llm.UserText("question"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}}}
	runtime.persisted = len(runtime.history)
	if result, err := client.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("request /new = %+v, %v", result, err)
	}
	var events []chatpkg.Event
	result, err := client.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: chatNewConfirmArg}, func(event chatpkg.Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	visible := result.Text
	for _, event := range events {
		visible += "\n" + event.Text
	}
	if strings.Contains(visible, secret) || !strings.Contains(visible, "[redacted:test]") {
		t.Fatalf("socket-visible warning = %q", visible)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = listener.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chatwire serve did not stop")
	}
}

func TestChatRuntimeSessionOwnershipBusySwitchCloseAndReacquire(t *testing.T) {
	ctx := context.Background()
	first, sessions := newRuntimeFixture(t, configuredChatModels())
	target, err := sessions.Create(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	owners := newChatSessionOwners()
	first.sessionOwners = owners
	if _, err := first.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID}); err != nil {
		t.Fatal(err)
	}

	second := newRuntimeAgainstSameStore(t, configuredChatModels(), sessions)
	second.sessionOwners = owners
	secondState, err := second.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("busy resume error = %v", err)
	}
	if second.current.ID != secondState.SessionID {
		t.Fatalf("busy resume changed session to %s", second.current.ID)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err != nil || result.State == nil || result.State.SessionID != target.ID {
		t.Fatalf("resume after release = %+v, %v", result, err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}

	third := newRuntimeAgainstSameStore(t, configuredChatModels(), sessions)
	third.sessionOwners = owners
	if _, err := third.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID}); err != nil {
		t.Fatalf("reacquire after switch close: %v", err)
	}
}

func TestChatRuntimeSessionOwnershipAllowsDifferentSessions(t *testing.T) {
	ctx := context.Background()
	first, sessions := newRuntimeFixture(t, configuredChatModels())
	owners := newChatSessionOwners()
	first.sessionOwners = owners
	second := newRuntimeAgainstSameStore(t, configuredChatModels(), sessions)
	second.sessionOwners = owners
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, runtime := range []*chatRuntime{first, second} {
		wg.Add(1)
		go func(runtime *chatRuntime) {
			defer wg.Done()
			_, err := runtime.Open(ctx, chatpkg.OpenOptions{})
			errs <- err
		}(runtime)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if first.current.ID == second.current.ID {
		t.Fatalf("different opens shared session %s", first.current.ID)
	}
}

func TestChatRuntimeSocketSessionOwnershipSwitchCloseAndReacquire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, sessions := newRuntimeFixture(t, configuredChatModels())
	target, err := sessions.Create(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	owners := newChatSessionOwners()
	factory := func(context.Context) (chatpkg.Backend, error) {
		runtime, runtimeErr := newChatRuntime(ctx, configuredChatModels(), &store.Store{DB: sessions.DB()})
		if runtimeErr == nil {
			runtime.sessionOwners = owners
		}
		return runtime, runtimeErr
	}
	socketDir, err := os.MkdirTemp("/tmp", "waffle-chat-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	listener, err := net.Listen("unix", filepath.Join(socketDir, "chat.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- chatwire.Serve(ctx, listener, factory, nil) }()
	dial := func() *chatwire.Client {
		client, dialErr := chatwire.Dial(ctx, listener.Addr().String())
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		return client
	}
	first := dial()
	if _, err := first.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID}); err != nil {
		t.Fatal(err)
	}
	second := dial()
	secondState, err := second.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	var remote *chatwire.RemoteError
	if !errors.As(err, &remote) || remote.Code != "session_active" || !strings.Contains(remote.Message, "already active") {
		t.Fatalf("busy socket resume = %#v", err)
	}
	status, err := second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandStatus}, nil)
	if err != nil || status.State == nil || status.State.SessionID != secondState.SessionID {
		t.Fatalf("state after busy resume = %+v, %v", status, err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, err := second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err != nil || resumed.State == nil || resumed.State.SessionID != target.ID {
		t.Fatalf("resume after close = %+v, %v", resumed, err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	third := dial()
	if _, err := third.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID}); err != nil {
		t.Fatalf("reacquire after switched owner close: %v", err)
	}
	if err := third.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = listener.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chatwire serve did not stop")
	}
}

func TestChatRuntimeRepoCommandCancelAndCloseOwnCommandContext(t *testing.T) {
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			runtime, _ := newRuntimeFixture(t, configuredChatModels())
			if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			runtime.repoOpener = func(ctx context.Context, _, _ string) (repoInstall, error) {
				close(started)
				<-ctx.Done()
				return repoInstall{}, ctx.Err()
			}
			commandDone := make(chan error, 1)
			go func() {
				_, err := runtime.Command(context.Background(), chatpkg.ParsedCommand{Name: chatpkg.CommandRepo, Args: "owner/repo"}, nil)
				commandDone <- err
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("repo command did not start")
			}
			if action == "cancel" {
				runtime.Cancel()
			} else {
				closeDone := make(chan error, 1)
				go func() { closeDone <- runtime.Close(context.Background()) }()
				select {
				case err := <-closeDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Close hung behind repo command")
				}
			}
			select {
			case err := <-commandDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("repo command error = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("repo command was not canceled")
			}
		})
	}
}

func TestChatRuntimeCloseBoundsUncooperativeRepoCommandAndRequiresExplicitCleanupRetry(t *testing.T) {
	ctx := context.Background()
	owners := newChatSessionOwners()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	runtime.sessionOwners = owners
	runtime.closeTimeout = 25 * time.Millisecond
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var cleanupCalls atomic.Int32
	originalCleanup := runtime.agentCleanupContext
	cleanupStarted := make(chan struct{})
	allowCleanup := make(chan struct{})
	runtime.agentCleanupContext = func(cleanupCtx context.Context) error {
		cleanupCalls.Add(1)
		close(cleanupStarted)
		<-allowCleanup
		return originalCleanup(cleanupCtx)
	}
	resourceDone := runtime.resourceCtx.Done()
	started := make(chan struct{})
	release := make(chan struct{})
	runtime.repoOpener = func(context.Context, string, string) (repoInstall, error) {
		close(started)
		<-release
		return repoInstall{}, context.Canceled
	}
	commandDone := make(chan error, 1)
	go func() {
		_, commandErr := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandRepo, Args: "owner/repo"}, nil)
		commandDone <- commandErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("repo command did not start")
	}

	for attempt := 1; attempt <= 2; attempt++ {
		before := time.Now()
		closeErr := runtime.Close(ctx)
		if closeErr == nil || !strings.Contains(closeErr.Error(), "wait for active chat command") {
			t.Fatalf("Close attempt %d error = %v", attempt, closeErr)
		}
		if elapsed := time.Since(before); elapsed > 250*time.Millisecond {
			t.Fatalf("Close attempt %d took %v", attempt, elapsed)
		}
		if cleanupCalls.Load() != 0 {
			t.Fatalf("cleanup ran while repo command was active: %d", cleanupCalls.Load())
		}
		select {
		case <-resourceDone:
			t.Fatal("shared resource context canceled while repo command was active")
		default:
		}
	}

	contender := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	contender.sessionOwners = owners
	if _, err := contender.Open(ctx, chatpkg.OpenOptions{SessionID: state.SessionID}); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("contender Open before cleanup retry error = %v", err)
	}

	close(release)
	select {
	case commandErr := <-commandDone:
		if !errors.Is(commandErr, context.Canceled) {
			t.Fatalf("repo command error = %v", commandErr)
		}
	case <-time.After(time.Second):
		t.Fatal("released repo command did not return")
	}
	select {
	case <-cleanupStarted:
		t.Fatal("timed-out Close started an untracked background finalizer")
	case <-time.After(50 * time.Millisecond):
	}
	lateCloseDone := make([]chan error, 2)
	for i := range lateCloseDone {
		lateCloseDone[i] = make(chan error, 1)
		go func(done chan<- error) { done <- runtime.Close(ctx) }(lateCloseDone[i])
	}
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("explicit Close retry did not start cleanup")
	}
	close(allowCleanup)
	for i, done := range lateCloseDone {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("concurrent Close %d after command exit = %v", i+1, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("concurrent Close %d did not observe explicit cleanup", i+1)
		}
	}
	select {
	case <-resourceDone:
	case <-time.After(time.Second):
		t.Fatal("explicit cleanup did not cancel shared resources")
	}
	if err := runtime.Close(ctx); err != nil {
		t.Fatalf("Close after explicit cleanup = %v", err)
	}
	if cleanupCalls.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls.Load())
	}

	reacquired := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	reacquired.sessionOwners = owners
	if _, err := reacquired.Open(ctx, chatpkg.OpenOptions{SessionID: state.SessionID}); err != nil {
		t.Fatalf("reacquire after explicit cleanup: %v", err)
	}
}

func TestChatRuntimeClosePreservesEarlierDeadlineWithoutBackgroundFinalizer(t *testing.T) {
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	runtime.closeTimeout = 250 * time.Millisecond
	if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}

	var cleanupCalls atomic.Int32
	originalCleanup := runtime.agentCleanupContext
	cleanupStarted := make(chan struct{}, 1)
	runtime.agentCleanupContext = func(cleanupCtx context.Context) error {
		cleanupCalls.Add(1)
		cleanupStarted <- struct{}{}
		return originalCleanup(cleanupCtx)
	}
	commandStarted := make(chan struct{})
	releaseCommand := make(chan struct{})
	runtime.repoOpener = func(context.Context, string, string) (repoInstall, error) {
		close(commandStarted)
		<-releaseCommand
		return repoInstall{}, context.Canceled
	}
	commandDone := make(chan error, 1)
	go func() {
		_, err := runtime.Command(context.Background(), chatpkg.ParsedCommand{Name: chatpkg.CommandRepo, Args: "owner/repo"}, nil)
		commandDone <- err
	}()
	<-commandStarted

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer closeCancel()
	started := time.Now()
	closeErr := runtime.Close(closeCtx)
	elapsed := time.Since(started)
	close(releaseCommand)
	if err := <-commandDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("command error = %v, want context canceled", err)
	}

	backgroundCleanup := false
	select {
	case <-cleanupStarted:
		backgroundCleanup = true
	case <-time.After(50 * time.Millisecond):
	}
	if closeErr == nil || !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want deadline exceeded", closeErr)
	}
	if elapsed > 120*time.Millisecond {
		t.Fatalf("Close discarded earlier caller deadline: %v", elapsed)
	}
	if backgroundCleanup {
		t.Fatal("timed-out Close launched an untracked background finalizer")
	}

	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("explicit cleanup retry: %v", err)
	}
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("explicit cleanup retry did not finalize runtime")
	}
	if cleanupCalls.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls.Load())
	}
}

func TestChatClientsShutdownBoundsRuntimeFinalCleanupAndRetriesOwnershipRelease(t *testing.T) {
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	owners := newChatSessionOwners()
	runtime.sessionOwners = owners
	clients := dashboard.NewChatClients(
		func(context.Context) (chatpkg.Backend, error) { return runtime, nil },
		nil,
	)
	id, state, err := clients.Open(context.Background(), chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}

	originalCleanup := runtime.agentCleanupContext
	cleanupStarted := make(chan struct{})
	var cleanupCalls atomic.Int32
	runtime.agentCleanupContext = func(ctx context.Context) error {
		if cleanupCalls.Add(1) == 1 {
			close(cleanupStarted)
			<-ctx.Done()
			return ctx.Err()
		}
		return originalCleanup(ctx)
	}
	resourceDone := runtime.resourceCtx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer shutdownCancel()
	shutdownDone := make(chan error, 1)
	started := time.Now()
	go func() { shutdownDone <- clients.Shutdown(shutdownCtx) }()
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not reach final agent/Docker-shaped cleanup")
	}
	err = <-shutdownDone
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("Shutdown exceeded the single global deadline: %v", elapsed)
	}
	select {
	case <-resourceDone:
		t.Fatal("failed final cleanup released runtime resources")
	default:
	}
	if calls := cleanupCalls.Load(); calls != 1 {
		t.Fatalf("cleanup calls after first Shutdown = %d, want 1", calls)
	}
	if err := clients.Turn(context.Background(), id, "must not run"); err == nil {
		t.Fatal("retiring client accepted a turn after failed cleanup")
	}

	contender := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	contender.sessionOwners = owners
	if _, err := contender.Open(context.Background(), chatpkg.OpenOptions{SessionID: state.SessionID}); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("session ownership released before successful cleanup retry: %v", err)
	}

	if err := clients.Shutdown(context.Background()); err != nil {
		t.Fatalf("explicit Shutdown retry: %v", err)
	}
	if calls := cleanupCalls.Load(); calls != 2 {
		t.Fatalf("cleanup calls after retry = %d, want 2", calls)
	}
	select {
	case <-resourceDone:
	default:
		t.Fatal("successful cleanup retry retained runtime resources")
	}
	reacquired := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	reacquired.sessionOwners = owners
	if _, err := reacquired.Open(context.Background(), chatpkg.OpenOptions{SessionID: state.SessionID}); err != nil {
		t.Fatalf("reacquire session after successful cleanup retry: %v", err)
	}
}

func TestChatRuntimeNewReflectsOldSessionAndRestoresProfileModel(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}
	runtime.history = []llm.Message{
		llm.UserText("question"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}},
	}
	for _, message := range runtime.history {
		if err := sessions.AppendTurn(ctx, state.SessionID, message); err != nil {
			t.Fatal(err)
		}
	}
	runtime.persisted = 2
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{response: llm.Response{
		StopReason: llm.StopEndTurn,
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "old session summary"}}},
	}}}}
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("request confirmation result=%+v err=%v", result, err)
	}
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := sessions.Get(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Summary != "old session summary" {
		t.Fatalf("old summary = %q", old.Summary)
	}
	if result.State == nil || result.State.SessionID == state.SessionID || result.State.ModelAlias != "claude" || runtime.agent.Model != "claude" {
		t.Fatalf("new result=%+v agent=%q", result, runtime.agent.Model)
	}
}

func TestChatRuntimeResumeReflectsSessionBeingLeft(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.history = []llm.Message{llm.UserText("question"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}}}
	for _, message := range runtime.history {
		if err := sessions.AppendTurn(ctx, state.SessionID, message); err != nil {
			t.Fatal(err)
		}
	}
	runtime.persisted = len(runtime.history)
	target, err := sessions.Create(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{response: llm.Response{
		StopReason: llm.StopEndTurn,
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "resume summary"}}},
	}}}}
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err != nil || result.State == nil || result.State.SessionID != target.ID {
		t.Fatalf("resume = %+v, %v", result, err)
	}
	old, err := sessions.Get(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Summary != "resume summary" {
		t.Fatalf("old summary = %q", old.Summary)
	}
}

func TestChatRuntimeResumeReflectionFailureWarnsRedactedAndStillSwitches(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	runtime.history = []llm.Message{llm.UserText("question"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}}}
	runtime.persisted = len(runtime.history)
	target, err := sessions.Create(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "opaque-resume-reflection-1934"
	runtime.agent.Redact = func(value string) string { return strings.ReplaceAll(value, secret, "[redacted:test]") }
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: errors.New("reflection failed with " + secret)}}}
	var events []chatpkg.Event
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, func(event chatpkg.Event) { events = append(events, event) })
	if err != nil || result.State == nil || result.State.SessionID != target.ID {
		t.Fatalf("resume = %+v, %v", result, err)
	}
	visible := result.Text
	for _, event := range events {
		visible += "\n" + event.Text
	}
	if strings.Contains(visible, secret) || !strings.Contains(visible, "[redacted:test]") || !strings.Contains(visible, "warning") {
		t.Fatalf("visible warning = %q", visible)
	}
}

func TestChatRuntimeStatusUsesProfileSandboxOverride(t *testing.T) {
	cfg := configuredChatModels()
	cfg.Sandbox.Mode = "docker"
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"safe": {Sandbox: "host"},
	}
	runtime, _ := newRuntimeFixture(t, cfg)
	state, err := runtime.Open(context.Background(), chatpkg.OpenOptions{Profile: "safe"})
	if err != nil {
		t.Fatal(err)
	}
	if state.SandboxMode != "host" || state.Profile != "safe" {
		t.Fatalf("profile state = %+v", state)
	}
}

func TestChatRuntimeClosedStateRejectsFurtherUse(t *testing.T) {
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	ctx := context.Background()
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Turn(ctx, "after close", nil); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Turn after Close err = %v", err)
	}
	second, err := newChatRuntime(ctx, runtime.cfg, runtime.st)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Open(ctx, chatpkg.OpenOptions{}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Open after Close err = %v", err)
	}
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

func aliases(models []chatpkg.Model) string {
	values := make([]string, len(models))
	for i, model := range models {
		values[i] = model.Alias
	}
	return strings.Join(values, ",")
}

type runtimeProviderStep struct {
	response llm.Response
	stream   string
	err      error
}

type runtimeScriptedProvider struct {
	mu        sync.Mutex
	responses []runtimeProviderStep
}

func (p *runtimeScriptedProvider) Complete(_ context.Context, _ llm.Request, onEvent llm.StreamFunc) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.responses) == 0 {
		return nil, errors.New("no scripted response")
	}
	step := p.responses[0]
	p.responses = p.responses[1:]
	if step.stream != "" && onEvent != nil {
		onEvent(llm.Event{Type: llm.EventTextDelta, Text: step.stream})
	}
	if step.err != nil {
		return nil, step.err
	}
	response := step.response
	return &response, nil
}

type runtimeBlockingProvider struct{ started chan struct{} }

func (p *runtimeBlockingProvider) Complete(ctx context.Context, _ llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	close(p.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

type runtimeTestTool struct{}

func (runtimeTestTool) Def() llm.Tool {
	return llm.Tool{Name: "runtime_test", Description: "runtime test", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (runtimeTestTool) Run(context.Context, json.RawMessage) (string, error) {
	return "tool output", nil
}

type runtimeNamedTool string

func (name runtimeNamedTool) Def() llm.Tool {
	return llm.Tool{Name: string(name), Description: "runtime named test tool", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (runtimeNamedTool) Run(context.Context, json.RawMessage) (string, error) {
	return "tool output", nil
}

type runtimeTestCloser struct{ closed int }

func (c *runtimeTestCloser) Close() error {
	c.closed++
	return nil
}

type runtimeContextTestCloser struct {
	beforeClose func()
	calls       int
	closed      bool
}

func (c *runtimeContextTestCloser) Close() error {
	return c.CloseContext(context.Background())
}

func (c *runtimeContextTestCloser) CloseContext(ctx context.Context) error {
	c.calls++
	if c.beforeClose != nil {
		beforeClose := c.beforeClose
		c.beforeClose = nil
		beforeClose()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.closed = true
	return nil
}

type runtimeNonComparableCloser []*int

func (c runtimeNonComparableCloser) Close() error {
	*c[0]++
	return nil
}

func hasTool(box tool.Toolbox, name string) bool {
	for _, def := range box.Defs() {
		if def.Name == name {
			return true
		}
	}
	return false
}

func toolNames(box tool.Toolbox) []string {
	defs := box.Defs()
	names := make([]string, len(defs))
	for i, def := range defs {
		names[i] = def.Name
	}
	return names
}

func eventKinds(events []chatpkg.Event) map[chatpkg.EventKind]bool {
	kinds := make(map[chatpkg.EventKind]bool, len(events))
	for _, event := range events {
		kinds[event.Kind] = true
	}
	return kinds
}

func configuredChatModels() config.Config {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderConnection{
		"local": {Type: "openai", BaseURL: "https://models.invalid/v1"},
	}
	cfg.Models = map[string]config.ModelTarget{
		"claude": {Provider: "local", Model: "upstream-claude"},
		"gpt":    {Provider: "local", Model: "upstream-gpt"},
	}
	cfg.Agent.DefaultModel = "claude"
	cfg.Agent.UtilityModel = ""
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	return cfg
}

func newRuntimeFixture(t *testing.T, cfg config.Config) (*chatRuntime, *session.Store) {
	t.Helper()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runtime, err := newChatRuntime(context.Background(), cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return runtime, session.New(st)
}

func newRuntimeAgainstSameStore(t *testing.T, cfg config.Config, sessions *session.Store) *chatRuntime {
	t.Helper()
	runtime, err := newChatRuntime(context.Background(), cfg, &store.Store{DB: sessions.DB()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return runtime
}

func writeRuntimeSkill(t *testing.T, name, description, status, body string) string {
	t.Helper()
	path := filepath.Join(os.Getenv("WAFFLE_HOME"), "workspace", "main", "skills", name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\nstatus: %s\n---\n\n%s\n", name, description, status, body)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func skillRefNames(refs []chatpkg.SkillRef) []string {
	names := make([]string, len(refs))
	for i, ref := range refs {
		names[i] = ref.Name
	}
	return names
}

func attachedSkillRef(refs []chatpkg.SkillRef, name string) chatpkg.SkillRef {
	for _, ref := range refs {
		if ref.Name == name {
			return ref
		}
	}
	return chatpkg.SkillRef{Name: name}
}

func TestChatRuntimeBranchCommandKeepsBoundariesAndNeverRewritesSource(t *testing.T) {
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source, err := sessions.Create(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	toolUse := llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
		Type:    llm.BlockToolUse,
		ToolUse: &llm.ToolUse{ID: "tu-1", Name: "read", Input: json.RawMessage(`{}`)},
	}}}
	toolResult := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{
		Type:       llm.BlockToolResult,
		ToolResult: &llm.ToolResult{ToolUseID: "tu-1", Content: "ok"},
	}}}
	steps := []llm.Message{
		llm.UserText("inspect"),
		toolUse,
		toolResult,
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "inspected"}}},
	}
	for _, msg := range steps {
		if err := sessions.AppendTurn(ctx, source.ID, msg); err != nil {
			t.Fatal(err)
		}
	}

	run := func(keep int) (chatpkg.Result, error) {
		return runtime.Command(ctx, chatpkg.ParsedCommand{
			Name: chatpkg.CommandBranch,
			Args: fmt.Sprintf("%s %d", source.ID, keep),
		}, nil)
	}

	// Mid-chain and tool-result boundaries fail closed.
	if _, err := run(2); err == nil {
		t.Fatal("branch after an unanswered tool use should fail")
	}
	if _, err := run(3); err == nil {
		t.Fatal("branch ending on a tool-result carrier should fail")
	}
	if _, err := run(4); err == nil {
		t.Fatal("branch at the assistant-final boundary should fail")
	}

	// Branch at the prompt: the new session keeps exactly that prefix.
	result, err := run(1)
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || result.State.SessionID == source.ID || result.State.SessionID == "" {
		t.Fatalf("branch state = %+v", result.State)
	}
	branchTurns, err := sessions.Turns(ctx, result.State.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(branchTurns) != 1 || branchTurns[0].Text() != "inspect" {
		t.Fatalf("branch turns = %+v", branchTurns)
	}
	sourceTurns, err := sessions.Turns(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceTurns) != len(steps) {
		t.Fatalf("source turns changed after branch: %d", len(sourceTurns))
	}

	// Empty prefix branches to a fresh conversation.
	result, err = run(0)
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || result.State.SessionID == "" {
		t.Fatalf("fresh branch state = %+v", result.State)
	}
	freshTurns, err := sessions.Turns(ctx, result.State.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(freshTurns) != 0 {
		t.Fatalf("fresh branch turns = %d", len(freshTurns))
	}

	// Usage errors.
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandBranch, Args: ""}, nil); err == nil {
		t.Fatal("branch without args should fail")
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandBranch, Args: source.ID + " nope"}, nil); err == nil {
		t.Fatal("branch with non-numeric keep should fail")
	}
}
