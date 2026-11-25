// Package webui embeds the compiled Flowsk Angular SPA (`dist/`) into the
// single static `flowsk` Go binary via `embed.FS` and serves it with SPA
// client-side routing fallback, hardened security headers, and zero host path
// exposure.
package webui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// embeddedDist bundles the production Angular build output compiled into `dist/`
// during `task build` and `task install`.
//
//go:embed all:dist
var embeddedDist embed.FS

// RuntimeBootstrap carries strictly logical startup hints returned by
// `GET /api/ui-config` so the Angular SPA automatically focuses the active
// workspace, optional bound project, and operator actor without ever receiving
// host filesystem paths.
type RuntimeBootstrap struct {
	// WorkspaceID is the optional resolved workspace UUID when running locally.
	WorkspaceID string `json:"workspaceId,omitempty"`

	// WorkspaceSlug is the initial workspace slug or UUID (e.g., "default").
	WorkspaceSlug string `json:"workspaceSlug"`

	// ProjectSlug is the optional initial project slug or key resolved from CWD or `--project`.
	ProjectSlug string `json:"projectSlug,omitempty"`

	// ActorID is the optional resolved operator actor UUID when running locally.
	ActorID string `json:"actorId,omitempty"`

	// ActorHandle is the default operator handle for interactive UI actions.
	ActorHandle string `json:"actorHandle,omitempty"`
}

// DistFS returns the sub-filesystem rooted at `dist/` inside the embedded bundle.
func DistFS() (fs.FS, error) {
	return fs.Sub(embeddedDist, "dist")
}

// NewBootstrapHandler serves `GET /api/ui-config` with strictly logical startup metadata.
func NewBootstrapHandler(cfg RuntimeBootstrap) http.Handler {
	ws := strings.TrimSpace(cfg.WorkspaceSlug)
	if ws == "" {
		ws = "default"
	}
	payload := RuntimeBootstrap{
		WorkspaceID:   strings.TrimSpace(cfg.WorkspaceID),
		WorkspaceSlug: ws,
		ProjectSlug:   strings.TrimSpace(cfg.ProjectSlug),
		ActorID:       strings.TrimSpace(cfg.ActorID),
		ActorHandle:   strings.TrimSpace(cfg.ActorHandle),
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		applySecurityHeaders(w)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(payload)
		}
	})
}

// NewSPAHandler constructs an `http.Handler` serving the embedded Angular SPA
// assets from `dist/`, falling back to `index.html` for extensionless client-side
// routes while returning 404 for missing static asset files.
func NewSPAHandler() (http.Handler, error) {
	subFS, err := DistFS()
	if err != nil {
		return nil, err
	}
	return NewSPAHandlerFromFS(subFS), nil
}

// NewSPAHandlerFromFS serves an Angular SPA from any `fs.FS` (used by tests and `NewSPAHandler`).
func NewSPAHandlerFromFS(staticFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(staticFS))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}

		applySecurityHeaders(w)

		cleanPath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if cleanPath == "" || cleanPath == "index.html" {
			serveIndexHTML(w, r, staticFS)
			return
		}

		if info, statErr := fs.Stat(staticFS, cleanPath); statErr == nil && !info.IsDir() {
			if isFingerprintedAsset(cleanPath) {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		// Missing explicit asset files (e.g. `.js`, `.css`, `.png`) return 404 instead of HTML.
		if path.Ext(cleanPath) != "" {
			http.NotFound(w, r)
			return
		}

		// Extensionless client-side Angular routes fall back to index.html.
		serveIndexHTML(w, r, staticFS)
	})
}

func serveIndexHTML(w http.ResponseWriter, r *http.Request, staticFS fs.FS) {
	raw, err := fs.ReadFile(staticFS, "index.html")
	if err != nil {
		http.Error(w, "embedded web UI index.html not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(raw)
	}
}

func applySecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
}

func isFingerprintedAsset(cleanPath string) bool {
	ext := strings.ToLower(path.Ext(cleanPath))
	switch ext {
	case ".js", ".css", ".woff2", ".woff":
		return strings.Count(path.Base(cleanPath), ".") >= 2 || strings.Contains(path.Base(cleanPath), "-")
	default:
		return false
	}
}
