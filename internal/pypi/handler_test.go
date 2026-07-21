package pypi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KumaTea/homir/internal/cache"
	"github.com/KumaTea/homir/internal/config"
	"github.com/KumaTea/homir/internal/store"
)

func TestRefreshPackageWatchesPrefetchesRecentCompactArtifacts(t *testing.T) {
	var oldRequests, universalRequests, sdistRequests, platformRequests atomic.Int32
	artifacts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/demo-1.0.tar.gz":
			oldRequests.Add(1)
		case "/demo-2.0-py3-none-any.whl":
			universalRequests.Add(1)
		case "/demo-2.0-cp312-manylinux.whl":
			platformRequests.Add(1)
		case "/demo-3.0.tar.gz":
			sdistRequests.Add(1)
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("artifact"))
	}))
	defer artifacts.Close()

	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pypi/demo/json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"releases":{"1.0":[{"filename":"demo-1.0.tar.gz","url":"` + artifacts.URL + `/demo-1.0.tar.gz","packagetype":"sdist","upload_time_iso_8601":"2026-01-01T00:00:00Z"}],"2.0":[{"filename":"demo-2.0-cp312-manylinux.whl","url":"` + artifacts.URL + `/demo-2.0-cp312-manylinux.whl","packagetype":"bdist_wheel","upload_time_iso_8601":"2026-02-01T00:00:00Z"},{"filename":"demo-2.0-py3-none-any.whl","url":"` + artifacts.URL + `/demo-2.0-py3-none-any.whl","packagetype":"bdist_wheel","upload_time_iso_8601":"2026-02-01T00:00:00Z"}],"3.0":[{"filename":"demo-3.0.tar.gz","url":"` + artifacts.URL + `/demo-3.0.tar.gz","packagetype":"sdist","upload_time_iso_8601":"2026-03-01T00:00:00Z"}]}}`))
	}))
	defer index.Close()

	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "homir.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager, err := cache.New(dir, map[string]config.Upstream{"pypi": {Kind: "pypi", Primary: index.URL}}, config.LifecycleSettings{}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(manager, db, map[string]config.Upstream{"pypi": {Kind: "pypi", Primary: index.URL}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WatchPackage("pypi", "pypi", "demo"); err != nil {
		t.Fatal(err)
	}

	result, err := handler.RefreshPackageWatches(time.Hour, time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Packages != 1 || result.Artifacts != 2 {
		t.Fatalf("prefetch result = %+v", result)
	}
	waitFor(t, func() bool { return universalRequests.Load() == 1 && sdistRequests.Load() == 1 })
	if oldRequests.Load() != 0 || platformRequests.Load() != 0 {
		t.Fatalf("unexpected artifacts requested: old=%d platform=%d", oldRequests.Load(), platformRequests.Load())
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
