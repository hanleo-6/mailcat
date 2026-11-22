package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/hanleo-6/mailcat/internal/config"
	"github.com/hanleo-6/mailcat/internal/store"
)

// Notifier sends desktop notifications for completed transfers.
type Notifier interface {
	Notify(summary, body, path string)
}

// Converter queues published HEIC files for background conversion.
type Converter interface {
	Enqueue(filename string) bool
}

// Server is the mailcat HTTP server.
type Server struct {
	cfg       *config.Config
	mux       *http.ServeMux
	logger    *log.Logger
	notifier  Notifier
	converter Converter
}

// New constructs a Server with all routes registered.
func New(cfg *config.Config, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{
		cfg:    cfg,
		mux:    http.NewServeMux(),
		logger: logger,
	}

	auth := BearerAuth(cfg.Token)

	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	notImplemented := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusNotImplemented, "not implemented")
	})
	s.mux.Handle("POST /upload", auth(http.HandlerFunc(s.handleUpload)))
	s.mux.Handle("GET /outbox", auth(notImplemented))
	s.mux.Handle("GET /outbox/{id}", auth(notImplemented))
	s.mux.Handle("POST /outbox/{id}/ack", auth(notImplemented))

	return s
}

// SetNotifier configures the desktop notifier used after transfers complete.
func (s *Server) SetNotifier(n Notifier) {
	s.notifier = n
}

// SetConverter configures the HEIC conversion pool. Pass nil to disable conversion.
func (s *Server) SetConverter(c Converter) {
	s.converter = c
}

// Handler returns the HTTP handler for testing or embedding.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Run binds every configured listen address and serves until ctx is cancelled
// or an unexpected listener error occurs.
func (s *Server) Run(ctx context.Context) error {
	if err := os.MkdirAll(s.cfg.InboxDir, 0o755); err != nil {
		return fmt.Errorf("create inbox_dir: %w", err)
	}
	if removed, err := store.CleanPartials(s.cfg.InboxDir); err != nil {
		return fmt.Errorf("clean stale partials: %w", err)
	} else if removed > 0 {
		s.logger.Printf("removed %d stale partial file(s)", removed)
	}

	listeners := make([]net.Listener, 0, len(s.cfg.Listen))
	for _, addr := range s.cfg.Listen {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		listeners = append(listeners, ln)
		s.logger.Printf("listening on %s", ln.Addr())
	}

	httpSrv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, len(listeners))
	var wg sync.WaitGroup
	for _, ln := range listeners {
		wg.Add(1)
		go func(l net.Listener) {
			defer wg.Done()
			if err := httpSrv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("serve %s: %w", l.Addr(), err)
			}
		}(ln)
	}

	var runErr error
	select {
	case <-ctx.Done():
		s.logger.Printf("shutting down")
	case err := <-errCh:
		runErr = err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shutdown: %w", err)
	}

	wg.Wait()
	return runErr
}
