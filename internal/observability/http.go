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
	return mux
}

// ServeListener serves the local status API until ctx is canceled.
func ServeListener(ctx context.Context, listener net.Listener, service *Service) error {
	server := &http.Server{
		Handler:           NewHandler(service),
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
