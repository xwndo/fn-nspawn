package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"fn-nspawn/internal/config"
	"fn-nspawn/internal/nspawn"
)

func newTestServer(t *testing.T, devMode bool) (*Server, string) {
	t.Helper()
	tmp := t.TempDir()
	store, err := config.NewStore(filepath.Join(tmp, "etc"))
	if err != nil {
		t.Fatal(err)
	}
	stdRoot := filepath.Join(tmp, "nspawn")
	if err := store.Save(config.AppConfig{
		StandardBaseRoot: stdRoot,
		BaseRoots:        []string{stdRoot},
	}); err != nil {
		t.Fatal(err)
	}
	mgr := nspawn.NewManager(store, nspawn.NewTaskManager())
	staticFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")},
	}
	return New(store, mgr, staticFS, devMode), stdRoot
}

func TestServerAuthTransportAndPeerCredBoundaries(t *testing.T) {
	srv, _ := newTestServer(t, false)

	// 1. Health endpoint is open even over TCP
	reqHealth := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	reqHealth = reqHealth.WithContext(WithTransportForTest(reqHealth.Context(), "tcp", false))
	recHealth := httptest.NewRecorder()
	srv.ServeHTTP(recHealth, reqHealth)
	if recHealth.Code != http.StatusOK {
		t.Fatalf("expected /api/health 200 over TCP, got %d", recHealth.Code)
	}

	// 2. TCP connection spoofing X-Trim-Isadmin in production mode MUST be rejected
	reqTCP := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	reqTCP.Header.Set("X-Trim-Userid", "1000")
	reqTCP.Header.Set("X-Trim-Isadmin", "true")
	reqTCP = reqTCP.WithContext(WithTransportForTest(reqTCP.Context(), "tcp", true))
	recTCP := httptest.NewRecorder()
	srv.ServeHTTP(recTCP, reqTCP)
	if recTCP.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for TCP header spoofing in prod mode, got %d", recTCP.Code)
	}

	// 3. Unix socket connection from untrusted local user (peerOK=false) MUST be rejected
	reqBadPeer := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	reqBadPeer.Header.Set("X-Trim-Userid", "1000")
	reqBadPeer.Header.Set("X-Trim-Isadmin", "true")
	reqBadPeer = reqBadPeer.WithContext(WithTransportForTest(reqBadPeer.Context(), "unix", false))
	recBadPeer := httptest.NewRecorder()
	srv.ServeHTTP(recBadPeer, reqBadPeer)
	if recBadPeer.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for untrusted Unix socket peer, got %d", recBadPeer.Code)
	}

	// 4. Trusted Unix socket peer with non-admin header -> 403
	reqNonAdmin := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	reqNonAdmin.Header.Set("X-Trim-Userid", "1001")
	reqNonAdmin.Header.Set("X-Trim-Isadmin", "false")
	reqNonAdmin = reqNonAdmin.WithContext(WithTransportForTest(reqNonAdmin.Context(), "unix", true))
	recNonAdmin := httptest.NewRecorder()
	srv.ServeHTTP(recNonAdmin, reqNonAdmin)
	if recNonAdmin.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin user, got %d", recNonAdmin.Code)
	}

	// 5. Trusted Unix socket peer + admin header -> 200 with ws_token
	reqOK := httptest.NewRequest(http.MethodGet, "/app/fn-nspawn/api/me", nil)
	reqOK.Header.Set("X-Trim-Userid", "1000")
	reqOK.Header.Set("X-Trim-Username", "superwr")
	reqOK.Header.Set("X-Trim-Isadmin", "true")
	reqOK = reqOK.WithContext(WithTransportForTest(reqOK.Context(), "unix", true))
	recOK := httptest.NewRecorder()
	srv.ServeHTTP(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid gateway admin request, got %d: %s", recOK.Code, recOK.Body.String())
	}
	var me map[string]interface{}
	if err := json.Unmarshal(recOK.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me["ws_token"] != srv.WSToken() || srv.WSToken() == "" {
		t.Fatalf("expected non-empty matching ws_token, got %v", me["ws_token"])
	}
}

func TestWebSocketTokenAndImageCacheEndpoints(t *testing.T) {
	srv, stdRoot := newTestServer(t, true)

	// Missing or invalid token on WS endpoints must return 403 before container lookup
	for _, path := range []string{
		"/api/ws/terminal?machine=demo&user=root",
		"/api/ws/terminal?machine=demo&user=root&token=wrong",
		"/api/ws/logs?machine=demo",
		"/api/ws/logs?machine=demo&token=wrong",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for %s, got %d", path, rec.Code)
		}
	}

	// Online image cache stats & clear API
	cacheFile := filepath.Join(stdRoot, ".image-cache", "tuna", "debian-bookworm.tar.xz")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("cached-image-payload"), 0644); err != nil {
		t.Fatal(err)
	}

	reqStats := httptest.NewRequest(http.MethodGet, "/api/images/cache", nil)
	recStats := httptest.NewRecorder()
	srv.ServeHTTP(recStats, reqStats)
	if recStats.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET /api/images/cache, got %d", recStats.Code)
	}
	var stats nspawn.OnlineImageCacheStats
	if err := json.Unmarshal(recStats.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.FileCount != 1 || stats.TotalBytes != int64(len("cached-image-payload")) {
		t.Fatalf("unexpected cache stats: %+v", stats)
	}

	reqDel := httptest.NewRequest(http.MethodDelete, "/api/images/cache", nil)
	recDel := httptest.NewRecorder()
	srv.ServeHTTP(recDel, reqDel)
	if recDel.Code != http.StatusOK {
		t.Fatalf("expected 200 on DELETE /api/images/cache, got %d", recDel.Code)
	}
	var clearRes nspawn.OnlineImageCacheClearResult
	if err := json.Unmarshal(recDel.Body.Bytes(), &clearRes); err != nil {
		t.Fatal(err)
	}
	if clearRes.RemovedFiles != 1 {
		t.Fatalf("expected 1 removed file, got %+v", clearRes)
	}
}
