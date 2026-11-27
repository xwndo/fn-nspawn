package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"fn-nspawn/internal/config"
	"fn-nspawn/internal/nspawn"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	username := r.Header.Get("X-Trim-Username")
	if username == "" && s.devMode {
		username = "dev-admin"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"username": username,
		"userid":   r.Header.Get("X-Trim-Userid"),
		"is_admin": s.devMode || r.Header.Get("X-Trim-Isadmin") == "true",
		"dev_mode": s.devMode,
	})
}

func (s *Server) handleHost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.InspectHostEnvironment())
}

func (s *Server) handleFSBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	res, err := s.mgr.BrowseFS(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleOnlineImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	refresh := q.Get("refresh") == "1" || strings.EqualFold(q.Get("refresh"), "true")
	res, err := s.mgr.ListOnlineImageCatalog(q.Get("source"), q.Get("arch"), refresh)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCronPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	expr := r.URL.Query().Get("expr")
	runs, err := nspawn.PreviewCronRuns(expr, 3)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"valid": false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"valid":     true,
		"next_runs": runs,
	})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.cfgStore.Get())
	case http.MethodPut:
		var next config.AppConfig
		if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.cfgStore.Save(next); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, s.cfgStore.Get())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.Tasks().List())
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	rec, ok := s.mgr.Tasks().Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "任务不存在"})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleImportPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req nspawn.ImportPreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.mgr.PreviewImportFromConfig(req.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleImportContainer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req nspawn.ImportContainerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	task, err := s.mgr.StartImportTask(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.mgr.DiscoverContainers()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		var req nspawn.CreateContainerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		task, err := s.mgr.StartCreateTask(req)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusAccepted, task)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleContainerRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/containers/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	machine := parts[0]

	// /api/containers/{machine}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			info, err := s.mgr.GetContainer(machine)
			if err != nil {
				writeErr(w, http.StatusNotFound, err)
				return
			}
			writeJSON(w, http.StatusOK, info)
		case http.MethodDelete:
			delRootfs := r.URL.Query().Get("delete_rootfs") == "true"
			delSnaps := r.URL.Query().Get("delete_snapshots") == "true"
			if err := s.mgr.DeleteContainer(machine, delRootfs, delSnaps); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	sub := parts[1]
	switch sub {
	case "action":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.mgr.ControlService(machine, body.Action); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		info, _ := s.mgr.GetContainer(machine)
		writeJSON(w, http.StatusOK, info)

	case "config":
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req nspawn.UpdateConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		updated, err := s.mgr.UpdateConfig(machine, req)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)

	case "spec":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		res, err := s.mgr.GetContainerSpecRaw(machine)
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, res)

	case "restore":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		task, err := s.mgr.StartRestoreTask(machine)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusAccepted, task)

	case "migrate":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req nspawn.MigrateContainerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		task, err := s.mgr.StartMigrateTask(machine, req)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusAccepted, task)

	case "logs":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
		logs, err := s.mgr.GetJournalLogs(machine, lines)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"logs": logs})

	case "snapshots":
		if len(parts) == 2 {
			switch r.Method {
			case http.MethodGet:
				snaps, err := s.mgr.ListSnapshots(machine)
				if err != nil {
					writeErr(w, http.StatusInternalServerError, err)
					return
				}
				writeJSON(w, http.StatusOK, snaps)
			case http.MethodPost:
				var req nspawn.CreateSnapshotRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					writeErr(w, http.StatusBadRequest, err)
					return
				}
				sn, err := s.mgr.CreateSnapshot(machine, req)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err)
					return
				}
				writeJSON(w, http.StatusCreated, sn)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		if len(parts) == 3 && r.Method == http.MethodDelete {
			if err := s.mgr.DeleteSnapshot(machine, parts[2]); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
			return
		}
		http.NotFound(w, r)

	case "images":
		if len(parts) == 2 && r.Method == http.MethodGet {
			imgs, err := s.mgr.ListImages(machine)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, http.StatusOK, imgs)
			return
		}
		if len(parts) == 3 && parts[2] == "export" && r.Method == http.MethodPost {
			var req nspawn.ExportImageRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			task, err := s.mgr.StartExportImageTask(machine, req)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusAccepted, task)
			return
		}
		if len(parts) == 3 && r.Method == http.MethodDelete {
			if err := s.mgr.DeleteImage(machine, parts[2]); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
			return
		}
		http.NotFound(w, r)

	case "rollback":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req nspawn.RollbackRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		task, err := s.mgr.StartRollbackTask(machine, req)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusAccepted, task)

	case "backup":
		if len(parts) == 2 {
			switch r.Method {
			case http.MethodGet:
				res, err := s.mgr.GetContainerBackup(machine)
				if err != nil {
					writeErr(w, http.StatusNotFound, err)
					return
				}
				writeJSON(w, http.StatusOK, res)
			case http.MethodPut:
				var req nspawn.SpecBackup
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					writeErr(w, http.StatusBadRequest, err)
					return
				}
				res, err := s.mgr.SaveContainerBackup(machine, req)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err)
					return
				}
				writeJSON(w, http.StatusOK, res)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		if len(parts) == 3 && parts[2] == "run" && r.Method == http.MethodPost {
			var body struct {
				Kind string `json:"kind"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			task, err := s.mgr.StartScheduledBackupTask(machine, body.Kind, "manual")
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusAccepted, task)
			return
		}
		http.NotFound(w, r)

	default:
		http.NotFound(w, r)
	}
}
