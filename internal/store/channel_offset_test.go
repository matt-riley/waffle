package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestChannelOffsetRoundTrips(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "offsets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cursor := NewChannelOffset(st.DB, "telegram")
	offset, err := cursor.Load(ctx)
	if err != nil {
		t.Fatalf("Load on an empty store: %v", err)
	}
	if offset != 0 {
		t.Fatalf("offset = %d, want 0 before anything is stored", offset)
	}

	if err := cursor.Save(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := cursor.Save(ctx, 43); err != nil {
		t.Fatalf("second Save (upsert): %v", err)
	}
	offset, err = cursor.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 43 {
		t.Fatalf("offset = %d, want the most recent save", offset)
	}

	// Cursors are per channel and must not read each other's position.
	other, err := NewChannelOffset(st.DB, "other").Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if other != 0 {
		t.Fatalf("other channel offset = %d, want 0", other)
	}
}

func TestChannelOffsetReportsStoreFailure(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "offsets-closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	cursor := NewChannelOffset(st.DB, "telegram")
	if _, err := cursor.Load(ctx); err == nil {
		t.Error("Load on a closed store returned no error")
	}
	if err := cursor.Save(ctx, 1); err == nil {
		t.Error("Save on a closed store returned no error")
	}
}
