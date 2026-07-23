package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
)

// NewMutationHandler adds request-bound idempotency to one exact dashboard
// mutation endpoint. Callers register its returned handler on that endpoint;
// it intentionally does not claim a route prefix.
func NewMutationHandler(security *Security, store *IdempotencyStore, maxBodyBytes int64, next http.Handler) http.Handler {
	return security.RequireMutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "request_body_too_large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid_request_body", http.StatusBadRequest)
			return
		}
		digest := sha256.Sum256(body)
		operation := r.Method + " " + r.URL.Path
		status, responseBody, doErr := store.Do(r.Context(), r.Header.Get("Idempotency-Key"), operation, hex.EncodeToString(digest[:]), func(ctx context.Context) (int, []byte) {
			r.Body = io.NopCloser(bytes.NewReader(body))
			recorder := newResponseCapture()
			next.ServeHTTP(recorder, r.WithContext(ctx))
			return recorder.status, recorder.body.Bytes()
		})
		if doErr != nil && status == 0 {
			http.Error(w, "mutation_unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(responseBody)
	}))
}

type responseCapture struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	wroteHeader bool
}

func newResponseCapture() *responseCapture {
	return &responseCapture{header: make(http.Header), status: http.StatusOK}
}

func (w *responseCapture) Header() http.Header {
	return w.header
}

func (w *responseCapture) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *responseCapture) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.body.Write(body)
}
