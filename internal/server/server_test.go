package server_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KumaTea/homir/internal/config"
	"github.com/KumaTea/homir/internal/server"
)

func newProxy(t *testing.T, upstreamURL string) *httptest.Server {
	t.Helper()
	return newProxyWithUpstream(t, config.Upstream{Primary: upstreamURL})
}

func newProxyWithUpstream(t *testing.T, upstream config.Upstream) *httptest.Server {
	t.Helper()
	app, err := server.New(context.Background(), config.Config{
		ListenAddress: "127.0.0.1:0",
		DataDirectory: t.TempDir(),
		Upstreams:     map[string]config.Upstream{"source": upstream},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return httptest.NewServer(app.Handler)
}

func newAPTProxy(t *testing.T, upstream config.Upstream) *httptest.Server {
	t.Helper()
	upstream.Kind = "apt"
	return newProxyWithUpstream(t, upstream)
}

func newAPKProxy(t *testing.T, upstream config.Upstream) *httptest.Server {
	t.Helper()
	upstream.Kind = "apk"
	return newProxyWithUpstream(t, upstream)
}

func newPyPIProxy(t *testing.T, upstream config.Upstream) *httptest.Server {
	t.Helper()
	upstream.Kind = "pypi"
	return newProxyWithUpstream(t, upstream)
}

func TestStreamsAndSharesAnInProgressDownload(t *testing.T) {
	body := bytes.Repeat([]byte("homir-"), 64*1024)
	firstChunkWritten := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", stringLength(body))
		_, _ = w.Write(body[:4096])
		w.(http.Flusher).Flush()
		close(firstChunkWritten)
		<-release
		_, _ = w.Write(body[4096:])
	}))
	defer upstream.Close()
	proxy := newProxy(t, upstream.URL)
	defer proxy.Close()

	first, err := http.Get(proxy.URL + "/v1/proxy/source/large.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	select {
	case <-firstChunkWritten:
	case <-time.After(time.Second):
		t.Fatal("upstream did not start")
	}
	initial := make([]byte, 16)
	if _, err := io.ReadFull(first.Body, initial); err != nil {
		t.Fatalf("first client did not receive live bytes: %v", err)
	}

	second, err := http.Get(proxy.URL + "/v1/proxy/source/large.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
	secondInitial := make([]byte, 16)
	if _, err := io.ReadFull(second.Body, secondInitial); err != nil {
		t.Fatalf("second client could not join live download: %v", err)
	}
	if !bytes.Equal(initial, secondInitial) {
		t.Fatal("joined client received different initial bytes")
	}
	close(release)

	firstRest, err := io.ReadAll(first.Body)
	if err != nil {
		t.Fatal(err)
	}
	secondRest, err := io.ReadAll(second.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(initial, firstRest...), body) || !bytes.Equal(append(secondInitial, secondRest...), body) {
		t.Fatal("clients did not receive the complete artifact")
	}
}

func TestCachesCompletedArtifactAndServesRange(t *testing.T) {
	body := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	proxy := newProxy(t, upstream.URL)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/v1/proxy/source/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("first response = %q", got)
	}

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/proxy/source/file.txt", nil)
	req.Header.Set("Range", "bytes=5-9")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(got) != "56789" {
		t.Fatalf("range response = %d %q", resp.StatusCode, got)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("cached range caused %d upstream requests, want 1", got)
	}
}

func TestDownloadsIndependentArtifactsInParallel(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	proxy := newProxy(t, upstream.URL)
	defer proxy.Close()

	results := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		go func(name string) {
			resp, err := http.Get(proxy.URL + "/v1/proxy/source/" + name)
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			results <- err
		}(name)
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("independent downloads did not reach upstream in parallel")
		}
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestUsesBackupAfterPrimaryServerFailure(t *testing.T) {
	var primaryRequests atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryRequests.Add(1)
		http.Error(w, "temporarily unavailable", http.StatusBadGateway)
	}))
	defer primary.Close()

	var backupRequests atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupRequests.Add(1)
		_, _ = w.Write([]byte("served by backup"))
	}))
	defer backup.Close()

	proxy := newProxyWithUpstream(t, config.Upstream{Primary: primary.URL, Backups: []string{backup.URL}})
	defer proxy.Close()
	response, err := http.Get(proxy.URL + "/v1/proxy/source/package.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "served by backup" {
		t.Fatalf("response = %q", body)
	}
	if primaryRequests.Load() != 1 || backupRequests.Load() != 1 {
		t.Fatalf("primary/backup requests = %d/%d, want 1/1", primaryRequests.Load(), backupRequests.Load())
	}
}

func TestAPTMetadataIsRelayedAndConditionallyRevalidated(t *testing.T) {
	const metadata = "signed repository metadata"
	var requests atomic.Int32
	var conditional atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/dists/bookworm-security/InRelease" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == "\"test-release\"" {
			conditional.Store(true)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", "\"test-release\"")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(metadata))
	}))
	defer upstream.Close()
	proxy := newAPTProxy(t, config.Upstream{Primary: upstream.URL, Security: true, MetadataTTL: "1ns"})
	defer proxy.Close()

	url := proxy.URL + "/apt/source/dists/bookworm-security/InRelease"
	for range 2 {
		response, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || string(body) != metadata {
			t.Fatalf("metadata response = %d %q", response.StatusCode, body)
		}
	}
	if requests.Load() != 2 || !conditional.Load() {
		t.Fatalf("requests/conditional = %d/%v, want 2/true", requests.Load(), conditional.Load())
	}
}

func TestAPTDebIsCachedAsAnArtifact(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/pool/main/h/homir/homir_1.0_amd64.deb" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("deb artifact"))
	}))
	defer upstream.Close()
	proxy := newAPTProxy(t, config.Upstream{Primary: upstream.URL, Security: true})
	defer proxy.Close()

	url := proxy.URL + "/apt/source/pool/main/h/homir/homir_1.0_amd64.deb"
	for range 2 {
		response, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "deb artifact" {
			t.Fatalf("artifact response = %q", body)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("artifact requests = %d, want 1", requests.Load())
	}
}

func TestAPKIndexIsRelayedAndConditionallyRevalidated(t *testing.T) {
	const index = "signed APKINDEX contents"
	var requests atomic.Int32
	var conditional atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/x86_64/APKINDEX.tar.gz" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == "\"apk-index\"" {
			conditional.Store(true)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", "\"apk-index\"")
		_, _ = w.Write([]byte(index))
	}))
	defer upstream.Close()
	proxy := newAPKProxy(t, config.Upstream{Primary: upstream.URL, MetadataTTL: "1ns"})
	defer proxy.Close()

	url := proxy.URL + "/apk/source/x86_64/APKINDEX.tar.gz"
	for range 2 {
		response, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || string(body) != index {
			t.Fatalf("index response = %d %q", response.StatusCode, body)
		}
	}
	if requests.Load() != 2 || !conditional.Load() {
		t.Fatalf("requests/conditional = %d/%v, want 2/true", requests.Load(), conditional.Load())
	}
}

func TestAPKArtifactIsCached(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/x86_64/homir-1.0-r0.apk" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("apk artifact"))
	}))
	defer upstream.Close()
	proxy := newAPKProxy(t, config.Upstream{Primary: upstream.URL})
	defer proxy.Close()

	url := proxy.URL + "/apk/source/x86_64/homir-1.0-r0.apk"
	for range 2 {
		response, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "apk artifact" {
			t.Fatalf("artifact response = %q", body)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("artifact requests = %d, want 1", requests.Load())
	}
}

func TestPyPISimplePageRewritesAndCachesArtifactLinks(t *testing.T) {
	var artifactRequests atomic.Int32
	artifact := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactRequests.Add(1)
		if r.URL.Path != "/packages/demo-1.0-py3-none-any.whl" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("wheel artifact"))
	}))
	defer artifact.Close()

	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/simple/demo/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-PyPI-Last-Serial", "123")
		_, _ = fmt.Fprintf(w, "<html><body><a href=%q data-dist-info-metadata=\"sha256=abc\">demo</a></body></html>", artifact.URL+"/packages/demo-1.0-py3-none-any.whl#sha256=abc")
	}))
	defer index.Close()
	proxy := newPyPIProxy(t, config.Upstream{Primary: index.URL})
	defer proxy.Close()

	response, err := http.Get(proxy.URL + "/pypi/source/simple/demo/")
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("X-PyPI-Last-Serial") != "123" {
		t.Fatal("PyPI serial header was not preserved")
	}
	if bytes.Contains(page, []byte("data-dist-info-metadata")) {
		t.Fatal("optional PEP 658 sidecar metadata hint was not removed")
	}
	match := regexp.MustCompile("href=\\\"([^\\\"]+)\\\"").FindSubmatch(page)
	if len(match) != 2 {
		t.Fatalf("rewritten link not found in %q", page)
	}
	rewritten := string(match[1])
	if !strings.HasPrefix(rewritten, "/pypi/source/files/") || !strings.HasSuffix(rewritten, "#sha256=abc") {
		t.Fatalf("unexpected rewritten link %q", rewritten)
	}

	for range 2 {
		artifactResponse, err := http.Get(proxy.URL + rewritten)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(artifactResponse.Body)
		artifactResponse.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if artifactResponse.StatusCode != http.StatusOK || string(body) != "wheel artifact" {
			t.Fatalf("artifact response = %d %q", artifactResponse.StatusCode, body)
		}
	}
	if artifactRequests.Load() != 1 {
		t.Fatalf("artifact requests = %d, want 1", artifactRequests.Load())
	}
}

func stringLength(value []byte) string {
	return fmt.Sprintf("%d", len(value))
}
