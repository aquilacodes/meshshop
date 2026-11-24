// Package platform holds the cross-cutting plumbing every service shares:
// HTTP server lifecycle, health endpoints, logging, error encoding, and a
// downstream client that propagates tracing headers through the mesh.
//
// It must not contain business or domain logic.
package platform

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// Service is a running HTTP microservice.
type Service struct {
	Name  string
	Layer string // bff | business | domain
	Mux   *http.ServeMux
	Log   *slog.Logger // use the *Context methods so records carry trace_id/span_id

	ready         atomic.Bool
	edge          bool
	shutdownTrace func(context.Context) error
}

// Edge marks the service as internet-facing (a BFF): inbound x-user-id headers
// are ignored, because there the caller's identity comes from a verified token
// (see WithUser), never from a header the client could set.
func (s *Service) Edge() *Service {
	s.edge = true
	return s
}

// NewService creates a service with /healthz and /readyz already registered,
// JSON logging, and OpenTelemetry tracing (see initTracing).
func NewService(name, layer string) *Service {
	log := slog.New(traceLogHandler{slog.NewJSONHandler(os.Stdout, nil)}).With("service", name, "layer", layer)
	s := &Service{Name: name, Layer: layer, Mux: http.NewServeMux(), Log: log}
	s.shutdownTrace = initTracing(name, layer)

	s.Mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.Mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			WriteError(w, &Error{Status: http.StatusServiceUnavailable, Code: "not_ready", Message: "shutting down"})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	return s
}

// Handler returns the service's full middleware stack, useful in tests:
// security headers -> recover -> server span -> request log -> header propagation -> routing.
func (s *Service) Handler() http.Handler {
	return secureHeaders(s.recoverer(traceServer(s.Mux, func(next http.Handler) http.Handler {
		return s.logRequests(propagate(next, s.edge))
	})))
}

// secureHeaders sets response headers that are right for a JSON API carrying
// personal data: no MIME sniffing, no caching, no framing, no cross-origin reads.
// HSTS is added at the TLS-terminating gateway.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// Run serves on $PORT (default 8080) until SIGINT/SIGTERM, then drains.
func (s *Service) Run() {
	addr := ":" + Env("PORT", "8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		s.Log.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.Log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()
	s.ready.Store(true)

	<-ctx.Done()
	s.ready.Store(false) // fail readiness so the endpoint is removed before we stop
	s.Log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		s.Log.Error("shutdown", "err", err)
	}
	if err := s.shutdownTrace(shutdownCtx); err != nil { // flush buffered spans
		s.Log.Error("trace shutdown", "err", err)
	}
}

func (s *Service) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.Log.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", r.Header.Get("x-request-id"),
		)
	})
}

func (s *Service) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.Log.ErrorContext(r.Context(), "panic", "value", v, "path", r.URL.Path)
				WriteError(w, ErrInternal("internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Env returns the environment variable or a fallback.
func Env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
