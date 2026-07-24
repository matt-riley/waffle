package dashboard

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/workset"
)

const memoryMutationMaxBodyBytes = 64 << 10

// MemoryRouteConfig is the additive composition seam for the caller-owned
// Desk mux. It does not create another token, idempotency store, or event hub.
type MemoryRouteConfig struct {
	Operations  *Operations
	Workspace   memory.Workspace
	Security    *Security
	Idempotency *IdempotencyStore
	Events      *EventHub
}

// RegisterMemoryRoutes mounts only the exact Memory endpoints.
func RegisterMemoryRoutes(mux *http.ServeMux, config MemoryRouteConfig) {
	service := NewMemoryService(config.Operations, config.Workspace)
	if config.Events != nil {
		service.events = config.Events
	}
	mux.Handle("GET /api/v1/desk/memory", newMemorySearchHandler(service))
	if config.Security == nil || config.Idempotency == nil {
		return
	}
	mutation := func(next http.Handler) http.Handler {
		protected := NewMutationHandler(
			config.Security,
			config.Idempotency,
			memoryMutationMaxBodyBytes,
			next,
		)
		return preserveResponseType(protected)
	}
	mux.Handle("POST /api/v1/desk/memory/attach", mutation(newMemoryAttachHandler(service)))
	mux.Handle("POST /api/v1/desk/memory/{noteID}/forget-preview", mutation(newMemoryForgetPreviewHandler(service)))
	mux.Handle("POST /api/v1/desk/memory/{noteID}/forget", mutation(newMemoryForgetHandler(service)))
}

func newMemorySearchHandler(service *MemoryService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, err := url.ParseQuery(r.URL.RawQuery)
		query := values["query"]
		if err != nil || len(values) != 1 || len(query) != 1 || query[0] == "" {
			writeMemoryError(w, ErrMemoryInvalidQuery)
			return
		}
		hits, err := service.Search(r.Context(), query[0], MemorySearchLimit)
		if err != nil {
			writeMemoryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Hits []MemoryHit `json:"hits"`
		}{Hits: hits})
	})
}

func newMemoryAttachHandler(service *MemoryService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request MemoryAttachRequest
		if !decodeMemoryRequest(w, r, &request) {
			return
		}
		entry, err := service.Attach(r.Context(), request)
		if err != nil {
			writeMemoryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Entry *workset.Entry `json:"entry"`
		}{Entry: entry})
	})
}

func newMemoryForgetPreviewHandler(service *MemoryService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if !decodeMemoryRequest(w, r, &request) {
			return
		}
		preview, err := service.PreviewForget(r.Context(), r.PathValue("noteID"), request.Query)
		if err != nil {
			writeMemoryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, preview)
	})
}

func newMemoryForgetHandler(service *MemoryService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			PreviewToken string `json:"preview_token"`
		}
		if !decodeMemoryRequest(w, r, &request) {
			return
		}
		result, err := service.Forget(r.Context(), r.PathValue("noteID"), request.PreviewToken)
		if err != nil {
			writeMemoryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}

func decodeMemoryRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeStrictJSON(w, r, target, func(w http.ResponseWriter) {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Code: "invalid_request", Message: "memory request is invalid",
		})
	})
}

func writeMemoryError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	response := errorResponse{
		Code: "memory_unavailable", Message: "memory request could not be completed",
	}
	switch {
	case errors.Is(err, ErrMemoryInvalidQuery):
		status = http.StatusBadRequest
		response = errorResponse{Code: "invalid_query", Message: "memory query is invalid"}
	case errors.Is(err, ErrMemoryInvalidSource):
		status = http.StatusBadRequest
		response = errorResponse{Code: "invalid_source", Message: "memory source is invalid"}
	case errors.Is(err, ErrMemoryHitNotFound):
		status = http.StatusNotFound
		response = errorResponse{Code: "memory_not_found", Message: "memory result was not found"}
	case errors.Is(err, ErrMemorySessionNotFound):
		status = http.StatusNotFound
		response = errorResponse{Code: "session_not_found", Message: "target session was not found"}
	case errors.Is(err, ErrMemoryWorksetConflict):
		status = http.StatusConflict
		response = errorResponse{Code: "workset_full", Message: "session working set cannot accept this memory"}
	case errors.Is(err, ErrMemoryConfirmation):
		status = http.StatusConflict
		response = errorResponse{Code: "confirmation_invalid", Message: "forget confirmation is invalid or expired"}
	case errors.Is(err, ErrMemoryForgetUnavailable):
		response = errorResponse{Code: "forget_unavailable", Message: "memory note could not be forgotten"}
	}
	writeJSON(w, status, response)
}
