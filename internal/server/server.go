package server

import (
	"io/fs"
	"net/http"
	"strings"

	"fn-nspawn/internal/config"
	"fn-nspawn/internal/nspawn"
)

const GatewayPrefix = "/app/fn-nspawn"

type Server struct {
	cfgStore *config.Store
	mgr      *nspawn.Manager
	staticFS fs.FS
	devMode  bool
	mux      *http.ServeMux
}

func New(cfgStore *config.Store, mgr *nspawn.Manager, staticFS fs.FS, devMode bool) *Server {
	s := &Server{
		cfgStore: cfgStore,
		mgr:      mgr,
		staticFS: staticFS,
		devMode:  devMode,
		mux:      http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/me", s.handleMe)
	s.mux.HandleFunc("/api/host", s.handleHost)
	s.mux.HandleFunc("/api/settings", s.handleSettings)
	s.mux.HandleFunc("/api/images/online", s.handleOnlineImages)
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
	s.mux.Handle("/", fileServer)
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
		uid := r.Header.Get("X-Trim-Userid")
		isAdmin := r.Header.Get("X-Trim-Isadmin")
		if uid == "" || isAdmin != "true" {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "需要飞牛 NAS 管理员权限才能管理系统容器",
				})
				return
			}
			http.Error(w, "403 Forbidden: 需要飞牛 NAS 管理员权限", http.StatusForbidden)
			return
		}
	}

	s.mux.ServeHTTP(w, r)
}
