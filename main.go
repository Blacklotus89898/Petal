// Command petal is the control plane binary: API server, run registry,
// scheduler, and embedded web UI.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
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
	"github.com/Blacklotus89898/Petal/internal/repos"
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
	reposPath := flag.String("repos", "", "config YAML: multi-repo allowlist (ADR 0004) + declared schedules; missing file = self repo only")
	secretsDir := flag.String("secrets", "", "directory of secret files (one per secret; the same path must exist in the dind container) — ADR 0006; empty = no secrets")
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
	if n, err := store.FailOrphaned(ctx); err != nil {
		logger.Error("close orphaned runs", "err", err)
		os.Exit(1)
	} else if n > 0 {
		logger.Warn("closed runs orphaned by the previous process", "count", n)
	}

	cfg, err := repos.LoadConfig(*reposPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
	allow := cfg.Repos
	if err := syncDeclaredSchedules(ctx, store, cfg.Schedules); err != nil {
		logger.Error("sync declared schedules", "err", err)
		os.Exit(1)
	}

	var exec executor.Executor = executor.Stub{}
	var repoPipes api.RepoPipelinesFunc
	if *execName == "steps" {
		steps := executor.NewSteps(store, *workdir, *dataDir, os.Getenv("DOCKER_HOST"), allow, logger)
		steps.ConfigureSecrets(*secretsDir, cfg.SelfSecrets)
		exec, repoPipes = steps, steps.RepoPipelines
	}
	logger.Info("executor selected", "executor", exec.Name())

	pool := scheduler.NewPool(store, exec, *workers, logger)
	go pool.Start(ctx)
	go pool.RunCronLoop(ctx)

	mux := http.NewServeMux()
	api.Register(mux, store, api.Options{
		TriggerToken:  os.Getenv("PETAL_TRIGGER_TOKEN"),
		APIToken:      os.Getenv("PETAL_API_TOKEN"),
		Workdir:       *workdir,
		DataDir:       *dataDir,
		Repos:         allow,
		RepoPipelines: repoPipes,
		CancelRunning: pool.Cancel,
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
		"api_writes", os.Getenv("PETAL_API_TOKEN") != "", "repos", len(allow), "declared_schedules", len(cfg.Schedules))

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
// syncDeclaredSchedules makes the config file's schedules the decl-* rows of
// the registry. A bad cron expression in git fails startup loudly rather than
// being silently skipped.
func syncDeclaredSchedules(ctx context.Context, store *runs.Store, decl []repos.Schedule) error {
	now := time.Now()
	out := make([]runs.Declared, 0, len(decl))
	for _, d := range decl {
		next, err := scheduler.NextRun(d.Cron, now)
		if err != nil {
			return fmt.Errorf("schedule %q: invalid cron %q: %w", d.ID, d.Cron, err)
		}
		args := "{}"
		if d.Args != nil {
			b, err := json.Marshal(d.Args)
			if err != nil {
				return fmt.Errorf("schedule %q: args: %w", d.ID, err)
			}
			args = string(b)
		}
		out = append(out, runs.Declared{
			ID: d.ID,
			NewSchedule: runs.NewSchedule{
				Pipeline: d.Pipeline, ArgsJSON: args, Cron: d.Cron, Repo: d.Repo, Ref: d.Ref,
			},
			NextRunAt: next.UTC().Format(time.RFC3339Nano),
		})
	}
	return store.SyncDeclaredSchedules(ctx, out)
}

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
