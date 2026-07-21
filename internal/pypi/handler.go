// Package pypi implements the PyPI Simple API-facing part of Homir.
package pypi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/net/html"

	"github.com/KumaTea/homir/internal/cache"
	"github.com/KumaTea/homir/internal/config"
)

type Handler struct {
	cache     *cache.Manager
	upstreams map[string]config.Upstream
	secret    []byte
	client    *http.Client
}

func NewHandler(manager *cache.Manager, upstreams map[string]config.Upstream, dataDirectory string) (*Handler, error) {
	secret, err := loadSecret(filepath.Join(dataDirectory, "pypi-link-signing.key"))
	if err != nil {
		return nil, err
	}
	return &Handler{cache: manager, upstreams: upstreams, secret: secret, client: http.DefaultClient}, nil
}

func loadSecret(filename string) ([]byte, error) {
	if secret, err := os.ReadFile(filename); err == nil {
		if len(secret) != 32 {
			return nil, fmt.Errorf("invalid PyPI link-signing key length")
		}
		return secret, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read PyPI link-signing key: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("create PyPI link-signing key: %w", err)
	}
	if err := os.WriteFile(filename, secret, 0o600); err != nil {
		return nil, fmt.Errorf("write PyPI link-signing key: %w", err)
	}
	return secret, nil
}

// Serve handles /pypi/{upstream}/simple/{project}/ and signed artifact URLs.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, upstreamName, path string) {
	upstream, ok := h.upstreams[upstreamName]
	if !ok || upstream.Kind != "pypi" {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(path, "simple/") {
		h.serveSimple(w, r, upstreamName, upstream, path)
		return
	}
	if strings.HasPrefix(path, "files/") {
		h.serveArtifact(w, r, upstreamName, path)
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) serveSimple(w http.ResponseWriter, _ *http.Request, upstreamName string, upstream config.Upstream, path string) {
	if !validSimplePath(path) {
		http.NotFound(w, nil)
		return
	}
	response, err := h.fetchSimple(upstream, path)
	if err != nil {
		http.Error(w, "PyPI upstream unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	document, err := html.Parse(response.Body)
	if err != nil {
		http.Error(w, "invalid PyPI Simple response", http.StatusBadGateway)
		return
	}
	h.rewriteLinks(document, response.Request.URL, upstreamName)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if serial := response.Header.Get("X-PyPI-Last-Serial"); serial != "" {
		w.Header().Set("X-PyPI-Last-Serial", serial)
	}
	if err := html.Render(w, document); err != nil {
		return
	}
}

func (h *Handler) serveArtifact(w http.ResponseWriter, r *http.Request, upstreamName, path string) {
	filename := strings.TrimPrefix(path, "files/")
	if filename == "" || strings.Contains(filename, "/") {
		http.NotFound(w, r)
		return
	}
	token := r.URL.Query().Get("token")
	sourceURL, ok := h.verify(token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.cache.ServeURL(w, r, upstreamName, sourceURL, sourceURL, cache.ArtifactPolicy)
}

func (h *Handler) fetchSimple(upstream config.Upstream, path string) (*http.Response, error) {
	urls := append([]string{upstream.Primary}, upstream.Backups...)
	var lastErr error
	for _, base := range urls {
		target, err := joinURL(base, path)
		if err != nil {
			return nil, err
		}
		request, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		// Requesting HTML keeps the link-rewriting behavior consistent even
		// when a modern pip client advertises the JSON Simple API variant.
		request.Header.Set("Accept", "text/html")
		response, err := h.client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s returned %s", target, response.Status)
			response.Body.Close()
			continue
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, fmt.Errorf("%s returned %s", target, response.Status)
		}
		return response, nil
	}
	return nil, fmt.Errorf("all PyPI upstreams failed: %w", lastErr)
}

func (h *Handler) rewriteLinks(node *html.Node, base *url.URL, upstreamName string) {
	if node.Type == html.ElementNode && node.Data == "a" {
		// PEP 658 sidecar metadata URLs are derived by pip from the artifact
		// URL. Signed query URLs are intentionally opaque, so omit the optional
		// hint and let pip retrieve the wheel/sdist itself.
		attributes := node.Attr[:0]
		for _, attribute := range node.Attr {
			if attribute.Key == "data-dist-info-metadata" || attribute.Key == "data-core-metadata" {
				continue
			}
			attributes = append(attributes, attribute)
		}
		node.Attr = attributes
		for index := range node.Attr {
			if node.Attr[index].Key != "href" {
				continue
			}
			target, err := base.Parse(node.Attr[index].Val)
			if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
				continue
			}
			fragment := target.Fragment
			target.Fragment = ""
			filename := pathBase(target.Path)
			if filename == "" {
				continue
			}
			node.Attr[index].Val = "/pypi/" + upstreamName + "/files/" + filename + "?token=" + h.sign(target.String())
			if fragment != "" {
				node.Attr[index].Val += "#" + fragment
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		h.rewriteLinks(child, base, upstreamName)
	}
}

func pathBase(value string) string {
	trimmed := strings.TrimSuffix(value, "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	return parts[len(parts)-1]
}

func (h *Handler) sign(sourceURL string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(sourceURL))
	mac := hmac.New(sha256.New, h.secret)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *Handler) verify(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, h.secret)
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	sourceURL := string(decoded)
	parsed, err := url.Parse(sourceURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Fragment != "" {
		return "", false
	}
	return sourceURL, true
}

func validSimplePath(path string) bool {
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	return len(parts) == 2 && parts[0] == "simple" && parts[1] != "" && parts[1] != "." && parts[1] != ".."
}

func joinURL(base, path string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/" + path
	return parsed.String(), nil
}
