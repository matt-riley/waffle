package gitcred

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/store"
)

// TestHelperAgainstBroker drives the full flow: git-style request in,
// broker consults its GitCredentialFunc, credential lines out.
func TestHelperAgainstBroker(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test teardown

	b := broker.New(st, nil)
	b.GitCredential = func(ctx context.Context, sessionID, host, path string) (string, string, error) {
		if host != "github.com" {
			t.Errorf("host = %q", host)
		}
		if sessionID != "ws-session" {
			t.Errorf("session = %q", sessionID)
		}
		return "x-access-token", "ghp_short_lived", nil
	}
	srv := httptest.NewServer(b)
	defer srv.Close()
	token, err := b.Mint(ctx, "ws-session")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// What git writes to a credential helper on stdin.
	request := "protocol=https\nhost=github.com\npath=matt-riley/waffle.git\n"
	out, err := Get(ctx, srv.URL, token, strings.NewReader(request))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(out, "username=x-access-token") || !strings.Contains(out, "password=ghp_short_lived") {
		t.Errorf("out = %q", out)
	}

	// Wrong token → forbidden, no credential.
	if _, err := Get(ctx, srv.URL, "wk_wrong", strings.NewReader(request)); err == nil {
		t.Error("bad token got a credential")
	}

	// Audit trail: mint + git-credential + denied.
	var actions []string
	rows, err := st.DB.Query(`SELECT action FROM broker_audit ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // test teardown
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, a)
	}
	want := []string{"mint", "git-credential", "denied"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Errorf("audit = %v, want %v", actions, want)
	}
}

func TestRunRequiresEnv(t *testing.T) {
	t.Setenv(EnvBroker, "")
	t.Setenv(EnvToken, "")
	err := Run(context.Background(), "get", strings.NewReader(""), nil)
	if err == nil || !strings.Contains(err.Error(), "WAFFLE_BROKER") {
		t.Errorf("err = %v", err)
	}

	// Non-get operations are silent no-ops.
	if err := Run(context.Background(), "store", strings.NewReader(""), nil); err != nil {
		t.Errorf("store op = %v", err)
	}
}

func TestRunGet(t *testing.T) {
	b := broker.New(nil, nil)
	b.GitCredential = func(ctx context.Context, sessionID, host, path string) (string, string, error) {
		return "u", "p", nil
	}
	srv := httptest.NewServer(b)
	defer srv.Close()
	token, err := b.Mint(context.Background(), "s")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	t.Setenv(EnvBroker, srv.URL)
	t.Setenv(EnvToken, token)

	var out strings.Builder
	if err := Run(context.Background(), "get", strings.NewReader("host=github.com\n"), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "password=p") {
		t.Errorf("out = %q", out.String())
	}
}
