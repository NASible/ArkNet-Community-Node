package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 * 1024,
	}
}

func Run(ctx context.Context, listener net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	httpServer := newHTTPServer(handler)
	serveResult := make(chan error, 1)
	go func() { serveResult <- httpServer.Serve(listener) }()

	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()
		<-serveResult
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveResult; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
