package tool_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/artifact"
	"github.com/matt-riley/waffle/internal/llm"
	sesspkg "github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

func newArtifactToolTestStore(t *testing.T) (*artifact.Store, *tool.WriteArtifact) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB.Exec(`INSERT INTO sessions (id, created_at, updated_at) VALUES ('session-a', '', '')`); err != nil {
		t.Fatal(err)
	}
	artifacts := artifact.New(st.DB)
	w := &tool.WriteArtifact{Store: artifacts, SessionID: sesspkg.IDFromContext}
	return artifacts, w
}

func TestWriteArtifactPersistsAndDeclaresOpaqueID(t *testing.T) {
	ctx := context.Background()
	artifacts, w := newArtifactToolTestStore(t)
	input, _ := json.Marshal(map[string]string{"name": "summary.md", "media_type": "text/markdown", "content": "# Summary"})
	text, blocks, err := w.RunBlocks(sesspkg.WithSession(ctx, "session-a"), input)
	if err != nil {
		t.Fatalf("RunBlocks: %v", err)
	}
	if !strings.Contains(text, "artifact created") {
		t.Fatalf("text = %q", text)
	}
	if len(blocks) != 1 || blocks[0].Type != llm.BlockArtifact || blocks[0].Artifact == nil {
		t.Fatalf("blocks = %+v", blocks)
	}
	ref := blocks[0].Artifact
	if ref.ID == "" || strings.ContainsAny(ref.ID, `/\`) {
		t.Fatalf("artifact id = %q, want opaque", ref.ID)
	}
	if ref.Name != "summary.md" || ref.Size != 9 || ref.Digest == "" {
		t.Fatalf("ref = %+v", ref)
	}
	got, err := artifacts.Get(ctx, "session-a", ref.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Payload) != "# Summary" {
		t.Fatalf("payload = %q", got.Payload)
	}
}

func TestWriteArtifactFailsClosedWithoutSession(t *testing.T) {
	_, w := newArtifactToolTestStore(t)
	input, _ := json.Marshal(map[string]string{"name": "a.txt", "media_type": "text/plain", "content": "x"})
	if _, err := w.Run(context.Background(), input); err == nil {
		t.Fatal("write_artifact without a session should fail closed")
	}
}
