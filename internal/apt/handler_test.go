package apt

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

func TestParsePackagesCatalogsArtifactRecords(t *testing.T) {
	records, err := parsePackages(strings.NewReader(`Package: demo
Version: 2:1.10-3
Filename: pool/main/d/demo/demo_1.10-3_amd64.deb
Description: a package
 continued description

Package: source-only
Version: 1.0

Package: second
Version: 3.0
Filename: pool/main/s/second/second_3.0_all.deb
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Package != "demo" || records[0].Version != "2:1.10-3" || records[1].ArtifactPath != "pool/main/s/second/second_3.0_all.deb" {
		t.Fatalf("records = %#v", records)
	}
}

func TestSelectRecordsUsesArchitectureAndDebianVersionOrder(t *testing.T) {
	records := []store.APTRecord{
		{ArtifactPath: "arm.deb", Version: "9.0", Architecture: "arm64"},
		{ArtifactPath: "old.deb", Version: "1.9", Architecture: "amd64"},
		{ArtifactPath: "middle.deb", Version: "1.10", Architecture: "amd64"},
		{ArtifactPath: "new.deb", Version: "2:1.0-1", Architecture: "amd64"},
		{ArtifactPath: "same-all.deb", Version: "3.0", Architecture: "all"},
		{ArtifactPath: "same-amd.deb", Version: "3.0", Architecture: "amd64"},
	}
	selected := selectRecords(records, "amd64", 2)
	if len(selected) != 2 || selected[0].ArtifactPath != "new.deb" || selected[1].ArtifactPath != "same-amd.deb" {
		t.Fatalf("selected records = %#v", selected)
	}
}

func TestRefreshPackageWatchesPrefetchesRequestedArchitecture(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/pool/demo_2.0_amd64.deb" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("deb"))
	}))
	defer upstream.Close()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "homir.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager, err := cache.New(dir, map[string]config.Upstream{"source": {Kind: "apt", Primary: upstream.URL}}, config.LifecycleSettings{}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(manager, db, map[string]config.Upstream{"source": {Kind: "apt", Primary: upstream.URL}})
	if err := db.ReplaceAPTCatalog("source", "dists/test/main/binary-amd64/Packages", []store.APTRecord{
		{Package: "demo", Version: "1.0", Architecture: "arm64", ArtifactPath: "pool/demo_1.0_arm64.deb"},
		{Package: "demo", Version: "2.0", Architecture: "amd64", ArtifactPath: "pool/demo_2.0_amd64.deb"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.WatchPackageVariant("apt", "source", "demo", "amd64"); err != nil {
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
