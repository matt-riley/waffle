package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/artifact"
)

const (
	artifactMutationMaxBodyBytes = 16 << 10
	artifactPreviewTTL           = 60 * time.Second
	artifactContentOperation     = "artifact-content"
	artifactMaxPreviewBytes      = 512 * 1024
)

// ArtifactView is the safe metadata + payload-mode projection returned by the
// preview endpoint. Content carries inline payload for text types; ContentURL
// is a short-lived, one-time content route for images; everything else is
// download-only.
type ArtifactView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	MediaType  string `json:"media_type,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Digest     string `json:"digest,omitempty"`
	State      string `json:"state,omitempty"`
	Mode       string `json:"mode"` // inline | content | download_only
	Content    string `json:"content,omitempty"`
	ContentURL string `json:"content_url,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// ArtifactsService serves owner-authorized artifact previews and downloads
// (#480). Ownership is the active browser chat lease: the requesting client
// must be the live owner of the session that produced the artifact. Digest
// and size are verified before any payload leaves the process.
type ArtifactsService struct {
	clients   *ChatClients
	artifacts *artifact.Store
	previews  *PreviewStore
}

// NewArtifactsService wires the artifact surface. A nil preview store
// disables the image content route (still download-only).
func NewArtifactsService(clients *ChatClients, artifacts *artifact.Store, previews *PreviewStore) *ArtifactsService {
	return &ArtifactsService{clients: clients, artifacts: artifacts, previews: previews}
}

// Preview validates ownership and returns the safe projection plus inline
// content or a short-lived content URL for previewable media types.
func (s *ArtifactsService) Preview(ctx context.Context, artifactID, clientID string) (ArtifactView, error) {
	a, err := s.owned(ctx, artifactID, clientID)
	if err != nil {
		return ArtifactView{}, err
	}
	view := ArtifactView{
		ID: a.ID, Name: a.Name, MediaType: a.MediaType,
		Size: a.Size, Digest: a.Digest, State: a.State,
	}
	if a.State != artifact.StateAvailable {
		view.Mode = "download_only"
		view.Reason = "This artifact is no longer available."
		return view, nil
	}
	switch {
	case isTextArtifact(a.MediaType) && len(a.Payload) <= artifactMaxPreviewBytes:
		view.Mode = "inline"
		view.Content = string(a.Payload)
	case isImageArtifact(a.MediaType) && s.previews != nil && len(a.Payload) <= artifactMaxPreviewBytes:
		token := s.previews.Issue(artifactContentOperation, a.ID, artifactPreviewTTL)
		view.Mode = "content"
		view.ContentURL = "/api/v1/desk/artifacts/" + url.PathEscape(a.ID) + "/content?token=" + url.QueryEscape(token)
	default:
		view.Mode = "download_only"
		view.Reason = "This artifact type is available for download only."
	}
	return view, nil
}

// Download streams the verified payload as an attachment. Fixed safe types,
// nosniff, and an encoded filename keep the response inert.
func (s *ArtifactsService) Download(ctx context.Context, w http.ResponseWriter, artifactID, clientID string) error {
	a, err := s.owned(ctx, artifactID, clientID)
	if err != nil {
		return err
	}
	if a.State != artifact.StateAvailable {
		return artifact.ErrNotFound
	}
	if err := s.artifacts.VerifyDigest(ctx, a.ID); err != nil {
		return err
	}
	contentType := safeContentType(a.MediaType)
	filename := mime.FormatMediaType("attachment", map[string]string{"filename": a.Name})
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", filename)
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Payload)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(a.Payload)
	return err
}

// Content serves a preview payload for one-time short-lived tokens (images
// referenced by <img>). Consuming the token invalidates it.
func (s *ArtifactsService) Content(ctx context.Context, w http.ResponseWriter, artifactID, token string) error {
	if s.previews == nil {
		return ErrPreviewUnknown
	}
	if err := s.previews.Consume(token, artifactContentOperation, artifactID); err != nil {
		return err
	}
	a, err := s.artifacts.Get(ctx, "", artifactID)
	if err != nil {
		return err
	}
	if a.State != artifact.StateAvailable {
		return artifact.ErrNotFound
	}
	if err := s.artifacts.VerifyDigest(ctx, a.ID); err != nil {
		return err
	}
	w.Header().Set("Content-Type", safeContentType(a.MediaType))
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Payload)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(a.Payload)
	return err
}

// owned enforces the active owner lease and session ownership, verifies the
// payload digest, and refreshes the artifact's lifecycle state in the view.
func (s *ArtifactsService) owned(ctx context.Context, artifactID, clientID string) (*artifact.Artifact, error) {
	if s == nil || s.artifacts == nil || s.clients == nil {
		return nil, ErrOperationsDependencyUnavailable
	}
	sessionID := s.clients.ActiveSessionID(clientID)
	if sessionID == "" {
		return nil, errChatClientNotFound
	}
	a, err := s.artifacts.Get(ctx, sessionID, artifactID)
	if err != nil {
		return nil, err
	}
	// A changed or missing payload must never be served; mark it stale and
	// refuse instead of streaming tampered bytes.
	if err := s.artifacts.VerifyDigest(ctx, a.ID); err != nil {
		_ = s.artifacts.SetState(ctx, a.ID, artifact.StateStale)
		return nil, artifact.ErrNotFound
	}
	return a, nil
}

func isTextArtifact(mediaType string) bool {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "text/plain", "text/markdown", "text/csv":
		return true
	default:
		return false
	}
}

func isImageArtifact(mediaType string) bool {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// safeContentType fixes the response type so a hostile or wrong media type
// can never turn a preview into an executable context.
func safeContentType(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "text/plain", "text/markdown", "text/csv":
		return "text/plain; charset=utf-8"
	case "image/png":
		return "image/png"
	case "image/jpeg":
		return "image/jpeg"
	case "image/gif":
		return "image/gif"
	case "image/webp":
		return "image/webp"
	case "application/pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

// ArtifactRouteConfig is the additive artifact integration seam for the
// caller-owned Desk mux (#480).
type ArtifactRouteConfig struct {
	Clients     *ChatClients
	Artifacts   *artifact.Store
	Previews    *PreviewStore
	Security    *Security
	Idempotency *IdempotencyStore
}

// RegisterArtifactRoutes mounts the exact artifact endpoints. Preview and
// download are guarded mutations (owner lease check inside); the image
// content route consumes a short-lived one-time token.
func RegisterArtifactRoutes(mux *http.ServeMux, routeConfig ArtifactRouteConfig) {
	if routeConfig.Clients == nil || routeConfig.Artifacts == nil {
		return
	}
	service := NewArtifactsService(routeConfig.Clients, routeConfig.Artifacts, routeConfig.Previews)
	if routeConfig.Security != nil {
		// Preview and download are read-like owner-authorized operations with
		// payloads up to artifact.MaxBytes. The mutation handler's response
		// cache would retain those payloads in memory until TTL and replay
		// cached bytes without re-verification, so only the request guard is
		// applied here (no idempotency response caching) (#480 review).
		requireMutation := routeConfig.Security.RequireMutation
		mux.Handle("POST /api/v1/desk/artifacts/{id}/preview", requireMutation(http.MaxBytesHandler(newArtifactPreviewHandler(service), artifactMutationMaxBodyBytes)))
		mux.Handle("POST /api/v1/desk/artifacts/{id}/download", requireMutation(http.MaxBytesHandler(newArtifactDownloadHandler(service), artifactMutationMaxBodyBytes)))
	}
	mux.Handle("GET /api/v1/desk/artifacts/{id}/content", newArtifactContentHandler(service))
}

func newArtifactPreviewHandler(service *ArtifactsService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ClientID string `json:"client_id"`
		}
		if !decodeArtifactRequest(w, r, &request) || request.ClientID == "" {
			return
		}
		view, err := service.Preview(r.Context(), r.PathValue("id"), request.ClientID)
		if err != nil {
			writeArtifactError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
}

func newArtifactDownloadHandler(service *ArtifactsService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ClientID string `json:"client_id"`
		}
		if !decodeArtifactRequest(w, r, &request) || request.ClientID == "" {
			return
		}
		if err := service.Download(r.Context(), w, r.PathValue("id"), request.ClientID); err != nil {
			if w.Header().Get("Content-Type") != "" {
				return // headers already committed; the stream is done
			}
			writeArtifactError(w, err)
			return
		}
	})
}

func newArtifactContentHandler(service *ArtifactsService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := service.Content(r.Context(), w, r.PathValue("id"), r.URL.Query().Get("token")); err != nil {
			writeArtifactError(w, err)
			return
		}
	})
}

func decodeArtifactRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeArtifactError(w, errInvalidArtifactRequest)
		return false
	}
	return true
}

var errInvalidArtifactRequest = errors.New("invalid artifact request")

func writeArtifactError(w http.ResponseWriter, err error) {
	status := http.StatusNotFound
	code, message := "artifact_not_found", "artifact was not found"
	switch {
	case errors.Is(err, errChatClientNotFound):
		status, code, message = http.StatusNotFound, "chat_client_not_found", "chat client was not found"
	case errors.Is(err, artifact.ErrNotOwned):
		status, code, message = http.StatusNotFound, "artifact_not_found", "artifact was not found"
	case errors.Is(err, artifact.ErrNotFound):
		status, code, message = http.StatusNotFound, "artifact_not_found", "artifact was not found or is no longer available"
	case errors.Is(err, ErrPreviewExpired), errors.Is(err, ErrPreviewEvicted), errors.Is(err, ErrPreviewMismatch), errors.Is(err, ErrPreviewUnknown), errors.Is(err, ErrPreviewUsed):
		status, code, message = http.StatusForbidden, "preview_invalid", "artifact preview is invalid or expired"
	case errors.Is(err, errInvalidArtifactRequest):
		status, code, message = http.StatusBadRequest, "invalid_request", "artifact request is invalid"
	case errors.Is(err, ErrOperationsDependencyUnavailable):
		status, code, message = http.StatusServiceUnavailable, "artifact_unavailable", "artifact services are unavailable"
	}
	writeJSON(w, status, errorResponse{Code: code, Message: message})
}
