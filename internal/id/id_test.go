package id

import (
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	id, err := New("ws-")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(id, "ws-") {
		t.Errorf("prefix: %s", id)
	}
	if len(id) != 3+8 {
		t.Errorf("len: %d", len(id))
	}
}

func TestNewSession(t *testing.T) {
	id, err := NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !strings.Contains(id, "-") {
		t.Errorf("format: %s", id)
	}
	parts := strings.Split(id, "-")
	if len(parts) != 3 || len(parts[2]) != 8 {
		t.Errorf("format: %s", id)
	}
}

func TestNewBytes(t *testing.T) {
	id, err := NewBytes(4)
	if err != nil {
		t.Fatalf("NewBytes(4): %v", err)
	}
	if len(id) != 8 {
		t.Errorf("len: %d", len(id))
	}

	_, err = NewBytes(0)
	if err == nil || !strings.Contains(err.Error(), "n>0") {
		t.Errorf("NewBytes(0) err: %v", err)
	}
	_, err = NewBytes(-1)
	if err == nil || !strings.Contains(err.Error(), "n>0") {
		t.Errorf("NewBytes(-1) err: %v", err)
	}
}

func TestNewPairingCode(t *testing.T) {
	code, err := NewPairingCode()
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	if len(code) != 6 {
		t.Errorf("len: %d", len(code))
	}
	for _, c := range code {
		if strings.ContainsRune("0O1I", c) {
			t.Errorf("ambiguous char: %c", c)
		}
	}
}
