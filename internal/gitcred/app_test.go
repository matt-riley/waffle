package gitcred

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type appRoundTripper func(*http.Request) (*http.Response, error)

func (f appRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAppCredentialMintsRepoScopedTokenAndCaches(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	serverCalls := atomic.Int32{}
	current := time.Now().UTC()
	client := &http.Client{Transport: appRoundTripper(func(r *http.Request) (*http.Response, error) {
		serverCalls.Add(1)
		if r.URL.Path != "/app/installations/7/access_tokens" {
			t.Errorf("path = %s", r.URL.Path)
		}
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Errorf("JWT = %q", r.Header.Get("Authorization"))
		} else {
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			var claims map[string]any
			if err := json.Unmarshal(payload, &claims); err != nil {
				t.Fatal(err)
			}
			if claims["iss"] != float64(42) {
				t.Errorf("iss = %v", claims["iss"])
			}
			if exp, ok := claims["exp"].(float64); !ok || exp <= claims["iat"].(float64) || exp-claims["iat"].(float64) > 600 {
				t.Errorf("JWT lifetime claims = %#v", claims)
			}
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if strings.Join(body.Repositories, ",") != "waffle" || body.Permissions["contents"] != "write" {
			t.Errorf("body = %+v", body)
		}
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		jwtParts := strings.Split(jwt, ".")
		if len(jwtParts) != 3 {
			t.Errorf("JWT parts = %d", len(jwtParts))
		} else {
			unsigned, sig := jwtParts[0]+"."+jwtParts[1], jwtParts[2]
			decoded, err := base64.RawURLEncoding.DecodeString(sig)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(unsigned))
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hash[:], decoded); err != nil {
				t.Errorf("JWT signature: %v", err)
			}
		}
		data, _ := json.Marshal(map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		return &http.Response{StatusCode: 201, Status: "201 Created", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})}
	app, err := NewApp(42, 7, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), "http://github.test", client, func() time.Time { return current })
	if err != nil {
		t.Fatal(err)
	}
	u, p, err := app.Credential(context.Background(), "owner/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if u != "x-access-token" || p != "ghs_test" {
		t.Fatalf("credential = %q/%q", u, p)
	}
	_, _, err = app.Credential(context.Background(), "owner/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if serverCalls.Load() != 1 {
		t.Fatalf("API calls = %d, want 1", serverCalls.Load())
	}
	current = current.Add(time.Hour)
	if _, _, err := app.Credential(context.Background(), "owner/waffle"); err != nil {
		t.Fatal(err)
	}
	if serverCalls.Load() != 2 {
		t.Fatalf("expired token API calls = %d, want 2", serverCalls.Load())
	}
}

func TestAppCredentialRejectsInvalidKey(t *testing.T) {
	if _, err := NewApp(1, 1, []byte("bad"), "", http.DefaultClient, time.Now); err == nil {
		t.Fatal("invalid key accepted")
	}
}
