package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"io/fs"
	"net"
	"net/http"
	"strings"

	"fn-nspawn/internal/config"
	"fn-nspawn/internal/nspawn"
)

const GatewayPrefix = "/app/fn-nspawn"

type ctxKey int

const (
	ctxKeyConnTransport ctxKey = iota
	ctxKeyUnixPeerOK
)

// ConnContext tags incoming HTTP connections with their transport ("unix" or "tcp")
// and kernel-verified SO_PEERCRED authorization state on Linux Unix sockets.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	switch conn := c.(type) {
	case *net.UnixConn:
		ctx = context.WithValue(ctx, ctxKeyConnTransport, "unix")
		ctx = context.WithValue(ctx, ctxKeyUnixPeerOK, inspectUnixPeerAllowed(conn))
	case *net.TCPConn:
		ctx = context.WithValue(ctx, ctxKeyConnTransport, "tcp")
	}
	return ctx
}

// WithTransportForTest allows unit tests to simulate transport context metadata.
func WithTransportForTest(ctx context.Context, transport string, unixPeerOK bool) context.Context {
	ctx = context.WithValue(ctx, ctxKeyConnTransport, transport)
	ctx = context.WithValue(ctx, ctxKeyUnixPeerOK, unixPeerOK)
	return ctx
}

func generateWSToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "fn-nspawn-fallback-ws-token"
	}
	return hex.EncodeToString(b)
}

type Server struct {
	cfgStore *config.Store
	mgr      *nspawn.Manager
	staticFS fs.FS
	devMode  bool
	wsToken  string
	mux      *http.ServeMux
}

func New(cfgStore *config.Store, mgr *nspawn.Manager, staticFS fs.FS, devMode bool) *Server {
	s := &Server{
		cfgStore: cfgStore,
		mgr:      mgr,
		staticFS: staticFS,
		devMode:  devMode,
		wsToken:  generateWSToken(),
		mux:      http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) WSToken() string {
	return s.wsToken
}

func (s *Server) verifyWSToken(token string) bool {
	t := strings.TrimSpace(token)
	if t == "" || len(t) != len(s.wsToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.wsToken)) == 1
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/me", s.handleMe)
	s.mux.HandleFunc("/api/host", s.handleHost)
	s.mux.HandleFunc("/api/settings", s.handleSettings)
	s.mux.HandleFunc("/api/images/online", s.handleOnlineImages)
	s.mux.HandleFunc("/api/images/cache", s.handleOnlineImageCache)
	s.mux.HandleFunc("/api/cron/preview", s.handleCronPreview)
	s.mux.HandleFunc("/api/fs/browse", s.handleFSBrowse)
	s.mux.HandleFunc("/api/tasks", s.handleTasks)
	s.mux.HandleFunc("/api/tasks/", s.handleTaskDetail)
	s.mux.HandleFunc("/api/containers/import/preview", s.handleImportPreview)
	s.mux.HandleFunc("/api/containers/import", s.handleImportContainer)
	s.mux.HandleFunc("/api/containers", s.handleContainers)
	s.mux.HandleFunc("/api/containers/", s.handleContainerRoute)
	s.mux.HandleFunc("/api/ws/terminal", s.handleWSTerminal)
	s.mux.HandleFunc("/api/ws/logs", s.handleWSLogs)

	fileServer := http.FileServer(http.FS(s.staticFS))
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/vendor/") {
			w.Header().Set("Cache-Control", "public, max-age=604800")
		} else {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func rejectForbidden(w http.ResponseWriter, r *http.Request, msg string) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": msg})
		return
	}
	http.Error(w, "403 Forbidden: "+msg, http.StatusForbidden)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Ensure trailing slash for root gateway path so relative ./ assets resolve cleanly
	if r.URL.Path == GatewayPrefix {
		http.Redirect(w, r, GatewayPrefix+"/", http.StatusFound)
		return
	}

	// Strip gateway prefix if present
	if strings.HasPrefix(r.URL.Path, GatewayPrefix+"/") {
		r2 := r.Clone(r.Context())
		u := *r.URL
		u.Path = strings.TrimPrefix(r.URL.Path, GatewayPrefix)
		if u.Path == "" {
			u.Path = "/"
		}
		r2.URL = &u
		r = r2
	}

	// Health check is open
	if r.URL.Path == "/api/health" {
		s.mux.ServeHTTP(w, r)
		return
	}

	// Gateway admin authentication in production mode
	if !s.devMode {
		transport, _ := r.Context().Value(ctxKeyConnTransport).(string)
		if transport == "tcp" {
			rejectForbidden(w, r, "生产模式下禁止通过 TCP 直接访问，请通过飞牛网关访问或使用 --dev 本地调试")
			return
		}
		if transport == "unix" {
			if peerOK, ok := r.Context().Value(ctxKeyUnixPeerOK).(bool); ok && !peerOK {
				rejectForbidden(w, r, "拒绝非系统网关进程访问 Unix Socket")
				return
			}
		}

		uid := r.Header.Get("X-Trim-Userid")
		isAdmin := r.Header.Get("X-Trim-Isadmin")
		if uid == "" || isAdmin != "true" {
			rejectForbidden(w, r, "需要飞牛 NAS 管理员权限才能管理系统容器")
			return
		}
	}

	s.mux.ServeHTTP(w, r)
}
