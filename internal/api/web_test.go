package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func webFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<!doctype html><title>PVN</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("console.log('PVN')"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestApplicationHandlerServesAPIIndexAndAssets(t *testing.T) {
	apiHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"data":{"status":"ok"}}`))
	})
	handler, err := NewApplicationHandler(apiHandler, WebOptions{Root: webFixture(t), FrameAncestors: []string{"https://pve.example.test:8006"}})
	if err != nil {
		t.Fatal(err)
	}

	index := request(t, handler, http.MethodGet, "/", nil, nil)
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "PVN") {
		t.Fatalf("index status=%d body=%s", index.Code, index.Body.String())
	}
	if !strings.Contains(index.Header().Get("Content-Security-Policy"), "frame-ancestors 'self' https://pve.example.test:8006") {
		t.Fatalf("CSP=%q", index.Header().Get("Content-Security-Policy"))
	}
	if index.Header().Get("X-Content-Type-Options") != "nosniff" || index.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("security headers=%v", index.Header())
	}
	if index.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index cache=%q", index.Header().Get("Cache-Control"))
	}

	asset := request(t, handler, http.MethodGet, "/assets/app.js", nil, nil)
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Content-Type"), "javascript") || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset status=%d headers=%v", asset.Code, asset.Header())
	}
	spa := request(t, handler, http.MethodGet, "/networks/private", nil, nil)
	if spa.Code != http.StatusOK || !strings.Contains(spa.Body.String(), "PVN") {
		t.Fatalf("SPA status=%d body=%s", spa.Code, spa.Body.String())
	}
	apiResponse := request(t, handler, http.MethodGet, "/api/v1/health", nil, nil)
	if apiResponse.Code != http.StatusOK || !strings.Contains(apiResponse.Body.String(), "status") {
		t.Fatalf("API status=%d body=%s", apiResponse.Code, apiResponse.Body.String())
	}
	head := request(t, handler, http.MethodHead, "/assets/app.js", nil, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD status=%d headers=%v body=%q", head.Code, head.Header(), head.Body.String())
	}
}

func TestApplicationHandlerRejectsTraversalAndMissingAssets(t *testing.T) {
	handler, err := NewApplicationHandler(http.NotFoundHandler(), WebOptions{Root: webFixture(t)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = "/../secret"
	req.URL.RawPath = "/%2e%2e/secret"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("traversal status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	missing := request(t, handler, http.MethodGet, "/assets/missing.js", nil, nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing asset status=%d", missing.Code)
	}
	post := request(t, handler, http.MethodPost, "/", nil, nil)
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST static status=%d", post.Code)
	}
}

func TestApplicationHandlerValidation(t *testing.T) {
	if _, err := NewApplicationHandler(nil, WebOptions{Root: webFixture(t)}); err == nil {
		t.Fatal("nil API accepted")
	}
	if _, err := NewApplicationHandler(http.NotFoundHandler(), WebOptions{Root: t.TempDir()}); err == nil {
		t.Fatal("missing index accepted")
	}
	if _, err := NewApplicationHandler(http.NotFoundHandler(), WebOptions{Root: webFixture(t), FrameAncestors: []string{"https://good.example; script-src *"}}); err == nil {
		t.Fatal("invalid frame ancestor accepted")
	}
}
