package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"weaver/internal/api"
	"weaver/internal/store"
)

// The API server is the front door: it turns HTTP requests into store operations
// and nothing more, so all the reliability guarantees stay in the database and the
// worker/scheduler loops rather than here.
func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	addr := os.Getenv("API_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// The built frontend is read from disk rather than embedded in the binary, so
	// `npm run dev` rebuilds are picked up by a browser refresh with no Go rebuild.
	webDir := os.Getenv("WEB_DIR")
	if webDir == "" {
		webDir = "./web"
	}

	// Cancel on Ctrl-C or SIGTERM so a shutdown drains in-flight requests instead
	// of dropping them.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer st.Close()

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewServer(st, webDir).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Serve in the background so main can wait on the shutdown signal.
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("api: listening on %s", addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("api: serve: %v", err)
		}
	case <-ctx.Done():
		log.Printf("api: shutting down")
		// Give in-flight requests a short grace period to finish.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("api: graceful shutdown failed: %v", err)
		}
	}
}
