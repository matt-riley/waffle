package observability

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"
)

// NewHandler returns the local HTTP status API handler.
func NewHandler(service *Service) http.Handler {
	mux := http.NewServeMux()
	RegisterRoutes(mux, service)
	return mux
}

// RegisterRoutes adds the local HTTP status API to mux.
func RegisterRoutes(mux *http.ServeMux, service *Service) {
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		snapshot, err := service.Snapshot(r.Context())
		if err != nil {
			http.Error(w, "status unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		health, err := service.HealthSnapshot(r.Context(), 2*time.Minute)
		if err != nil {
			http.Error(w, "health unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !health.Healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(health)
	})
}

// ServeListener serves the local status API until ctx is canceled.
func ServeListener(ctx context.Context, listener net.Listener, service *Service) error {
	return ServeHandler(ctx, listener, NewHandler(service))
}

// ServeHandler serves handler until ctx is canceled.
func ServeHandler(ctx context.Context, listener net.Listener, handler http.Handler) error {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
