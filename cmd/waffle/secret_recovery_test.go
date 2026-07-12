package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/secret"
	"github.com/zalando/go-keyring"
)

func TestIdentityExportImportRoundTripDecryptsExistingStore(t *testing.T) {
	t.Setenv(secret.EnvIdentity, "")
	keyring.MockInit()

	original, err := secret.InitIdentity(false)
	if err != nil {
		t.Fatalf("InitIdentity: %v", err)
	}
	path := filepath.Join(t.TempDir(), "secrets.age")
	secrets := secret.OpenFile(path, original)
	if err := secrets.Set("recovery/token", "survives-keychain-loss"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var exported bytes.Buffer
	if err := secretCmd([]string{"export-identity", "--yes"}, strings.NewReader(""), &exported, &bytes.Buffer{}); err != nil {
		t.Fatalf("export-identity: %v", err)
	}
	if err := keyring.Delete("waffle", "age-identity"); err != nil {
		t.Fatalf("wipe keyring: %v", err)
	}
	if _, err := secret.LoadIdentity(); err == nil {
		t.Fatal("LoadIdentity succeeded after keyring wipe")
	}
	if err := secretCmd([]string{"import-identity", "--yes"}, strings.NewReader(exported.String()), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("import-identity: %v", err)
	}

	recovered, err := secret.LoadIdentity()
	if err != nil {
		t.Fatalf("LoadIdentity after import: %v", err)
	}
	if recovered.String() != original.String() {
		t.Fatalf("recovered identity differs from exported identity")
	}
	got, err := secret.OpenFile(path, recovered).Get("recovery/token")
	if err != nil {
		t.Fatalf("decrypt existing store: %v", err)
	}
	if got != "survives-keychain-loss" {
		t.Fatalf("secret = %q", got)
	}
}
