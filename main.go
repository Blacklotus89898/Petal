// Command petal is the control plane binary: API server, run registry,
// scheduler, and embedded web UI.
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Blacklotus89898/Petal/internal/api"
	"github.com/Blacklotus89898/Petal/internal/executor"
	"github.com/Blacklotus89898/Petal/internal/runs"
	"github.com/Blacklotus89898/Petal/internal/scheduler"
)

//go:embed all:web/dist
var webFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "data/petal.db", "SQLite database path")
	dataDir := flag.String("data", "", "data root for run logs and workspaces (default: db directory)")
	workers := flag.Int("workers", 2, "executor worker count")
	execName := flag.String("executor", "stub", "executor: stub or steps")
	workdir := flag.String("workdir", ".", "source workdir pipelines are loaded from (steps executor)")
	flag.Parse()
	if *dataDir == "" {
		*dataDir = filepath.Dir(*dbPath)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		logger.Error("create data dir", "err", err)
		os.Exit(1)
	}

	store, err := runs.Open(ctx, *dbPath)
	if err != nil {
		logger.Error("open run registry", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	var exec executor.Executor = executor.Stub{}
	if *execName == "steps" {
		exec = executor.NewSteps(store, *workdir, *dataDir, os.Getenv("DOCKER_HOST"), logger)
	}
	logger.Info("executor selected", "executor", exec.Name())

	pool := scheduler.NewPool(store, exec, *workers, logger)
	go pool.Start(ctx)
	go pool.RunCronLoop(ctx)

	mux := http.NewServeMux()
	api.Register(mux, store, api.Options{
		TriggerToken: os.Getenv("PETAL_TRIGGER_TOKEN"),
		APIToken:     os.Getenv("PETAL_API_TOKEN"),
		Workdir:      *workdir,
		DataDir:      *dataDir,
	})
	mux.Handle("/", spaHandler())

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("petal listening", "addr", *addr, "db", *dbPath, "workers", *workers,
		"api_writes", os.Getenv("PETAL_API_TOKEN") != "")

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		logger.Error("server error", "err", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// spaHandler serves the embedded frontend, falling back to index.html for
// client-side routes.
func spaHandler() http.Handler {
	dist, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		panic("embedded web/dist missing: " + err.Error())
	}
	fileServer := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(dist, p); err != nil {
			r2 := new(http.Request)
			*r2 = *r
			r2.URL = new(url.URL)
			*r2.URL = *r.URL
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
