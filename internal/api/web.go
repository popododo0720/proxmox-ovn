package api

import (
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type WebOptions struct {
	Root           string
	FrameAncestors []string
}

type applicationHandler struct {
	api http.Handler
	web fs.FS
	csp string
}

// NewApplicationHandler mounts the JSON API and the compiled single-page UI
// on one HTTPS origin.
func NewApplicationHandler(apiHandler http.Handler, options WebOptions) (http.Handler, error) {
	if apiHandler == nil {
		return nil, errors.New("API handler is required")
	}
	if options.Root == "" {
		return nil, errors.New("web root is required")
	}
	web := os.DirFS(options.Root)
	info, err := fs.Stat(web, "index.html")
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("web root %q has no index.html", options.Root)
	}
	ancestors := options.FrameAncestors
	if len(ancestors) == 0 {
		ancestors = []string{"https://*:8006"}
	}
	for _, origin := range ancestors {
		parsed, parseErr := url.Parse(origin)
		if parseErr != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || strings.ContainsAny(origin, " ;\t\r\n") {
			return nil, fmt.Errorf("invalid frame ancestor %q", origin)
		}
	}
	csp := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'self' " + strings.Join(ancestors, " ")
	return &applicationHandler{api: apiHandler, web: web, csp: csp}, nil
}

func (h *applicationHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	setWebSecurityHeaders(writer, h.csp)
	if request.URL.Path == "/api/v1" || strings.HasPrefix(request.URL.Path, "/api/v1/") {
		h.api.ServeHTTP(writer, request)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		methodNotAllowed(writer, http.MethodGet, http.MethodHead)
		return
	}
	decoded, err := url.PathUnescape(request.URL.EscapedPath())
	if err != nil || strings.Contains(decoded, "\\") || strings.IndexByte(decoded, 0) >= 0 {
		writeError(writer, http.StatusBadRequest, "invalid_path", "request path is invalid", nil)
		return
	}
	name := strings.TrimPrefix(decoded, "/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		writeError(writer, http.StatusNotFound, "not_found", "file was not found", nil)
		return
	}
	info, statErr := fs.Stat(h.web, name)
	if statErr != nil || info.IsDir() {
		if strings.HasPrefix(name, "assets/") || filepath.Ext(name) != "" {
			writeError(writer, http.StatusNotFound, "not_found", "file was not found", nil)
			return
		}
		name = "index.html"
	}
	content, err := fs.ReadFile(h.web, name)
	if err != nil {
		writeError(writer, http.StatusNotFound, "not_found", "file was not found", nil)
		return
	}
	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType == "" {
		contentType = http.DetectContentType(content)
	}
	writer.Header().Set("Content-Type", contentType)
	if strings.HasPrefix(name, "assets/") {
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		writer.Header().Set("Cache-Control", "no-cache")
	}
	writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodGet {
		_, _ = writer.Write(content)
	}
}

func setWebSecurityHeaders(writer http.ResponseWriter, csp string) {
	writer.Header().Set("Content-Security-Policy", csp)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	writer.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}
