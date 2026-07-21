package cache

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KumaTea/homir/internal/config"
	"github.com/KumaTea/homir/internal/store"
)

func newLifecycleManager(t *testing.T, lifecycle config.LifecycleSettings, body string) (*Manager, *store.Store, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	db, err := store.Open(filepath.Join(t.TempDir(), "homir.db"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(t.TempDir(), map[string]config.Upstream{
		"source": {Primary: upstream.URL},
	}, lifecycle, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manager.Serve(w, r, "source", r.URL.Path[1:])
	}))
	t.Cleanup(func() {
		proxy.Close()
		upstream.Close()
		_ = db.Close()
	})
	return manager, db, proxy
}

func TestCleanupRemovesInactiveTrackedArtifacts(t *testing.T) {
	manager, db, proxy := newLifecycleManager(t, config.LifecycleSettings{
		MaxSize: 1000, InactivityTTL: time.Second, CleanupInterval: time.Hour,
	}, "artifact")
	response, err := http.Get(proxy.URL + "/package.deb")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "artifact" {
		t.Fatalf("artifact response = %d %q", response.StatusCode, body)
	}
	waitFor(t, func() bool {
		_, found, err := db.Get(cacheKey("artifact", "source", "package.deb"))
		return err == nil && found
	})
	artifact, _, _ := db.Get(cacheKey("artifact", "source", "package.deb"))
	if !artifact.Tracked {
		t.Fatalf("artifact was not marked tracked: %+v", artifact)
	}
	// The persisted access timestamp is second-granularity; cross two second
	// boundaries so the one-second policy is unambiguously expired.
	time.Sleep(2100 * time.Millisecond)

	result, err := manager.Cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedEntries != 1 || result.ReleasedBytes != int64(len("artifact")) {
		t.Fatalf("cleanup result = %+v", result)
	}
	if _, found, err := db.Get(cacheKey("artifact", "source", "package.deb")); err != nil || found {
		t.Fatalf("artifact remains after cleanup: found=%v err=%v", found, err)
	}
}

func TestCleanupKeepsUntrackedMetadataUntilCapacityRequiresEviction(t *testing.T) {
	manager, db, proxy := newLifecycleManager(t, config.LifecycleSettings{
		MaxSize: 1000, InactivityTTL: time.Nanosecond, CleanupInterval: time.Hour,
	}, "metadata")
	request, _ := http.NewRequest(http.MethodGet, proxy.URL+"/dists/stable/InRelease", nil)
	recorder := httptest.NewRecorder()
	manager.ServeWithPolicy(recorder, request, "source", "dists/stable/InRelease", Policy{
		Namespace: "apt-metadata", RefreshAfter: time.Hour,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("metadata response = %d", recorder.Code)
	}

	result, err := manager.Cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedEntries != 0 {
		t.Fatalf("untracked metadata was removed: %+v", result)
	}
	if _, found, err := db.Get(cacheKey("apt-metadata", "source", "dists/stable/InRelease")); err != nil || !found {
		t.Fatalf("metadata missing after cleanup: found=%v err=%v", found, err)
	}
}

func TestCleanupEvictsLeastRecentlyUsedAtCapacity(t *testing.T) {
	manager, db, proxy := newLifecycleManager(t, config.LifecycleSettings{
		MaxSize: 5, InactivityTTL: time.Hour, CleanupInterval: time.Hour,
	}, "1234")
	for _, path := range []string{"first.deb", "second.deb"} {
		response, err := http.Get(proxy.URL + "/" + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	waitFor(t, func() bool {
		size, err := db.TotalSize()
		return err == nil && size == 8
	})

	result, err := manager.Cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedEntries != 1 || result.ReleasedBytes != 4 {
		t.Fatalf("cleanup result = %+v", result)
	}
	if size, err := db.TotalSize(); err != nil || size != 4 {
		t.Fatalf("cache size = %d, err=%v; want 4", size, err)
	}
}

func TestLifecycleWorkerStopsWithContext(t *testing.T) {
	manager, _, _ := newLifecycleManager(t, config.LifecycleSettings{
		MaxSize: 1000, InactivityTTL: time.Hour, CleanupInterval: time.Millisecond,
	}, "unused")
	ctx, cancel := context.WithCancel(context.Background())
	manager.StartCleanup(ctx)
	cancel()
}

func TestWatchRefreshesOnlyRequestedArtifacts(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte("artifact"))
	}))
	defer upstream.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "homir.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager, err := New(t.TempDir(), map[string]config.Upstream{"source": {Primary: upstream.URL}}, config.LifecycleSettings{}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/requested.deb", nil)
	manager.Serve(recorder, request, "source", "requested.deb")
	if recorder.Code != http.StatusOK {
		t.Fatalf("artifact response = %d", recorder.Code)
	}

	result, err := manager.RefreshWatches(time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if result.Due != 1 || result.Started != 1 || result.Expired != 0 {
		t.Fatalf("watch refresh result = %+v", result)
	}
	waitFor(t, func() bool { return requests.Load() == 2 })
	if _, found, err := db.Get(cacheKey("artifact", "source", "requested.deb")); err != nil || !found {
		t.Fatalf("watched artifact missing: found=%v err=%v", found, err)
	}
}

func TestCancelsIdlePartialDownload(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		_, _ = w.Write([]byte("first bytes"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "homir.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager, err := New(dir, map[string]config.Upstream{"source": {Primary: upstream.URL}}, config.LifecycleSettings{PartialTTL: 30 * time.Millisecond}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manager.Serve(w, r, "source", "large.deb")
	}))
	defer proxy.Close()
	response, err := http.Get(proxy.URL + "/large.deb")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream did not start")
	}
	response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("idle transfer was not cancelled")
	}
	waitFor(t, func() bool {
		entries, err := os.ReadDir(manager.partial)
		return err == nil && len(entries) == 0
	})
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
