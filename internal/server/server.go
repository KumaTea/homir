package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/KumaTea/homir/internal/admin"
	"github.com/KumaTea/homir/internal/apk"
	"github.com/KumaTea/homir/internal/apt"
	"github.com/KumaTea/homir/internal/cache"
	"github.com/KumaTea/homir/internal/config"
	"github.com/KumaTea/homir/internal/pypi"
	"github.com/KumaTea/homir/internal/store"
)

type Server struct {
	*http.Server
	store  *store.Store
	cache  *cache.Manager
	cancel context.CancelFunc
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Server, error) {
	return NewWithConfigPath(ctx, cfg, logger, "")
}

func NewWithConfigPath(ctx context.Context, cfg config.Config, logger *slog.Logger, configPath string) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDirectory, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	db, err := store.Open(filepath.Join(cfg.DataDirectory, "homir.db"))
	if err != nil {
		return nil, err
	}
	lifecycle, err := cfg.Cache.Lifecycle()
	if err != nil {
		db.Close()
		return nil, err
	}
	manager, err := cache.New(cfg.DataDirectory, cfg.Upstreams, lifecycle, db, logger)
	if err != nil {
		db.Close()
		return nil, err
	}
	runContext, cancel := context.WithCancel(ctx)
	manager.StartCleanup(runContext)
	manager.StartWatchRefresh(runContext, lifecycle.WatchInterval, lifecycle.InactivityTTL)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Homir</title><style>body{font:16px system-ui,sans-serif;max-width:46rem;margin:10vh auto;padding:0 1rem}code{background:#f3f3f3;padding:.1rem .25rem}</style><h1>Homir</h1><p>A self-hosted, on-demand package mirror for APT, APK, and PyPI.</p><p><a href="/admin/">Administration dashboard</a></p><p>Service health: <a href="/healthz"><code>/healthz</code></a></p><p>See the project README for package-manager configuration examples.</p></html>`)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /v1/proxy/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1/proxy/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		manager.Serve(w, r, parts[0], parts[1])
	})
	aptHandler := apt.NewHandler(manager, db, cfg.Upstreams)
	aptHandler.StartPrefetch(runContext, lifecycle.WatchInterval, lifecycle.InactivityTTL, lifecycle.PrefetchVersions)
	mux.HandleFunc("GET /apt/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/apt/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		aptHandler.Serve(w, r, parts[0], parts[1])
	})
	apkHandler := apk.NewHandler(manager, db, cfg.Upstreams)
	apkHandler.StartPrefetch(runContext, lifecycle.WatchInterval, lifecycle.InactivityTTL, lifecycle.PrefetchVersions)
	mux.HandleFunc("GET /apk/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/apk/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		apkHandler.Serve(w, r, parts[0], parts[1])
	})
	pypiHandler, err := pypi.NewHandler(manager, db, cfg.Upstreams, cfg.DataDirectory)
	if err != nil {
		cancel()
		db.Close()
		return nil, err
	}
	pypiHandler.StartPrefetch(runContext, lifecycle.WatchInterval, lifecycle.InactivityTTL, lifecycle.PrefetchVersions)
	mux.HandleFunc("GET /pypi/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/pypi/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		pypiHandler.Serve(w, r, parts[0], parts[1])
	})
	adminAuth, err := admin.NewAuth(cfg.Admin, os.Getenv("HOMIR_ADMIN_PASSWORD"))
	if err != nil {
		cancel()
		db.Close()
		return nil, err
	}
	mux.Handle("/admin/", admin.NewHandler(adminAuth, db.Stats, cfg.Upstreams, configPath))

	return &Server{Server: &http.Server{Addr: cfg.ListenAddress, Handler: mux}, store: db, cache: manager, cancel: cancel}, nil
}

func (s *Server) Cleanup() (cache.CleanupResult, error) { return s.cache.Cleanup() }

func (s *Server) Close() error {
	s.cancel()
	return s.store.Close()
}
