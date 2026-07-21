package apk

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KumaTea/homir/internal/cache"
	"github.com/KumaTea/homir/internal/config"
	"github.com/KumaTea/homir/internal/store"
)

func TestParseIndexCatalogsArtifactRecords(t *testing.T) {
	records, err := parseIndex(strings.NewReader(`C:Q1
P:busybox
V:1.36.1-r2
A:x86_64

P:ca-certificates
V:20250619-r0
A:noarch
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ArtifactPath != "busybox-1.36.1-r2.apk" || records[1].Architecture != "noarch" {
		t.Fatalf("records = %#v", records)
	}
}

func TestSelectRecordsUsesArchitectureAndVersionOrder(t *testing.T) {
	records := []store.APKRecord{
		{ArtifactPath: "arm.apk", Version: "9.0-r0", Architecture: "aarch64"},
		{ArtifactPath: "old.apk", Version: "1.9-r0", Architecture: "x86_64"},
		{ArtifactPath: "middle.apk", Version: "1.10-r0", Architecture: "x86_64"},
		{ArtifactPath: "new.apk", Version: "2.0-r1", Architecture: "x86_64"},
	}
	selected := selectRecords(records, "x86_64", 2)
	if len(selected) != 2 || selected[0].ArtifactPath != "new.apk" || selected[1].ArtifactPath != "middle.apk" {
		t.Fatalf("selected records = %#v", selected)
	}
}

func TestRefreshPackageWatchesPrefetchesRequestedArchitecture(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/demo-2.0-r0-x86_64.apk" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("apk"))
	}))
	defer upstream.Close()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "homir.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager, err := cache.New(dir, map[string]config.Upstream{"source": {Kind: "apk", Primary: upstream.URL}}, config.LifecycleSettings{}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(manager, db, map[string]config.Upstream{"source": {Kind: "apk", Primary: upstream.URL}})
	if err := db.ReplaceAPKCatalog("source", "APKINDEX.tar.gz", []store.APKRecord{
		{Package: "demo", Version: "1.0-r0", Architecture: "aarch64", ArtifactPath: "demo-1.0-r0-aarch64.apk"},
		{Package: "demo", Version: "2.0-r0", Architecture: "x86_64", ArtifactPath: "demo-2.0-r0-x86_64.apk"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.WatchPackageVariant("apk", "source", "demo", "x86_64"); err != nil {
		t.Fatal(err)
	}
	result, err := handler.RefreshPackageWatches(time.Hour, time.Hour, 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Packages != 1 || result.Artifacts != 1 {
		t.Fatalf("prefetch result = %+v", result)
	}
	deadline := time.Now().Add(time.Second)
	for requests.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if requests.Load() != 1 {
		t.Fatalf("prefetch requests = %d", requests.Load())
	}
}
