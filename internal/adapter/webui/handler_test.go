// File handler_test.go verifies embedded SPA asset serving, client-side route
// fallback, security headers, and logical bootstrap JSON responses.
package webui_test

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mindmain/flowsk/internal/adapter/webui"
)

func TestSPAHandler_RoutesAndSecurityHeaders(t *testing.T) {
	t.Parallel()

	memFS := fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte("<html><app-root></app-root></html>")},
		"main-ABCDEF12.js":  &fstest.MapFile{Data: []byte("console.log('flowsk');")},
		"styles-123456.css": &fstest.MapFile{Data: []byte("body{margin:0}")},
	}
	h := webui.NewSPAHandlerFromFS(memFS)

	// 1. Root serves index.html with security headers and no-cache.
	reqRoot := httptest.NewRequest(http.MethodGet, "/", nil)
	recRoot := httptest.NewRecorder()
	h.ServeHTTP(recRoot, reqRoot)
	if recRoot.Code != http.StatusOK || !strings.Contains(recRoot.Body.String(), "<app-root>") {
		t.Fatalf("unexpected root response: code=%d body=%q", recRoot.Code, recRoot.Body.String())
	}
	if got := recRoot.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("expected X-Content-Type-Options=nosniff, got %q", got)
	}
	const wantCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'"
	if got := recRoot.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("unexpected Content-Security-Policy:\ngot:  %q\nwant: %q", got, wantCSP)
	}

	// 2. Fingerprinted JS bundle gets immutable Cache-Control.
	reqJS := httptest.NewRequest(http.MethodGet, "/main-ABCDEF12.js", nil)
	recJS := httptest.NewRecorder()
	h.ServeHTTP(recJS, reqJS)
	if recJS.Code != http.StatusOK {
		t.Fatalf("expected 200 for JS asset, got %d", recJS.Code)
	}
	if cc := recJS.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("expected immutable cache control on fingerprinted JS, got %q", cc)
	}

	// 3. Extensionless deep route falls back to index.html for SPA router.
	reqDeep := httptest.NewRequest(http.MethodGet, "/projects/flowsk/kanban", nil)
	recDeep := httptest.NewRecorder()
	h.ServeHTTP(recDeep, reqDeep)
	if recDeep.Code != http.StatusOK || !strings.Contains(recDeep.Body.String(), "<app-root>") {
		t.Fatalf("expected SPA fallback to index.html, got code=%d", recDeep.Code)
	}

	// 4. Missing static file with extension returns 404.
	reqMissing := httptest.NewRequest(http.MethodGet, "/missing-chunk.js", nil)
	recMissing := httptest.NewRecorder()
	h.ServeHTTP(recMissing, reqMissing)
	if recMissing.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing .js file, got %d", recMissing.Code)
	}
}

func TestBootstrapHandler(t *testing.T) {
	t.Parallel()
	h := webui.NewBootstrapHandler(webui.RuntimeBootstrap{
		WorkspaceID:   "11111111-1111-4111-8111-111111111111",
		WorkspaceSlug: "default",
		ProjectSlug:   "flowsk",
		ActorID:       "22222222-2222-4222-8222-222222222222",
		ActorHandle:   "mind",
	})
	req := httptest.NewRequest(http.MethodGet, "/api/ui-config", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	var got webui.RuntimeBootstrap
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal bootstrap JSON: %v", err)
	}
	if got.WorkspaceID != "11111111-1111-4111-8111-111111111111" ||
		got.WorkspaceSlug != "default" ||
		got.ProjectSlug != "flowsk" ||
		got.ActorID != "22222222-2222-4222-8222-222222222222" ||
		got.ActorHandle != "mind" {
		t.Fatalf("unexpected bootstrap payload: %+v", got)
	}
}

func TestDynamicBootstrapHandler_AuthStateAndZeroLeakage(t *testing.T) {
	t.Parallel()

	h := webui.NewDynamicBootstrapHandler(func(r *http.Request) webui.RuntimeBootstrap {
		authed := r.Header.Get("Authorization") == "Bearer secret"
		return webui.RuntimeBootstrap{
			WorkspaceID:   "11111111-1111-4111-8111-111111111111",
			WorkspaceSlug: "prod",
			ActorID:       "22222222-2222-4222-8222-222222222222",
			ActorHandle:   "operator",
			AuthRequired:  true,
			Authenticated: authed,
		}
	})

	// 1. Unauthenticated caller sees authRequired=true, authenticated=false, and NEVER receives WorkspaceID or ActorID.
	reqUnauth := httptest.NewRequest(http.MethodGet, "/api/ui-config", nil)
	recUnauth := httptest.NewRecorder()
	h.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusOK {
		t.Fatalf("unauth status = %d, want 200", recUnauth.Code)
	}
	var unauthPayload webui.RuntimeBootstrap
	if err := json.Unmarshal(recUnauth.Body.Bytes(), &unauthPayload); err != nil {
		t.Fatalf("unmarshal unauth payload: %v", err)
	}
	if !unauthPayload.AuthRequired || unauthPayload.Authenticated {
		t.Fatalf("unexpected auth flags on unauth request: %+v", unauthPayload)
	}
	if unauthPayload.WorkspaceID != "" || unauthPayload.ActorID != "" {
		t.Fatalf("leaked WorkspaceID/ActorID to unauthenticated caller: %+v", unauthPayload)
	}

	// 2. Authenticated caller receives full logical bootstrap metadata.
	reqAuth := httptest.NewRequest(http.MethodGet, "/api/ui-config", nil)
	reqAuth.Header.Set("Authorization", "Bearer secret")
	recAuth := httptest.NewRecorder()
	h.ServeHTTP(recAuth, reqAuth)
	var authPayload webui.RuntimeBootstrap
	if err := json.Unmarshal(recAuth.Body.Bytes(), &authPayload); err != nil {
		t.Fatalf("unmarshal auth payload: %v", err)
	}
	if !authPayload.AuthRequired || !authPayload.Authenticated ||
		authPayload.WorkspaceID != "11111111-1111-4111-8111-111111111111" ||
		authPayload.ActorID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("unexpected auth bootstrap payload: %+v", authPayload)
	}
}

func TestEmbeddedDistIndexHTML_StrictCSPCompatible(t *testing.T) {
	t.Parallel()

	distFS, err := webui.DistFS()
	if err != nil {
		t.Fatalf("DistFS: %v", err)
	}
	rawIndex, err := fs.ReadFile(distFS, "index.html")
	if err != nil {
		t.Fatalf("read embedded dist/index.html: %v", err)
	}
	html := string(rawIndex)
	if !strings.Contains(html, "<app-root></app-root>") {
		t.Fatalf("embedded dist/index.html missing <app-root>: %s", html)
	}
	// Strict `script-src 'self'` CSP blocks inline event handlers such as Beasties'
	// `media="print" onload="this.media='all'"`, which would prevent Tailwind CSS from loading.
	for _, forbidden := range []string{`onload=`, `onerror=`, `media="print"`} {
		if strings.Contains(strings.ToLower(html), forbidden) {
			t.Fatalf("embedded dist/index.html contains CSP-incompatible token %q:\n%s", forbidden, html)
		}
	}
}
