package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"fn-nspawn/internal/nspawn"
	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

var (
	wsUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if origin == "" {
				return true
			}
			u, err := url.Parse(origin)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return false
			}
			return true
		},
	}
	reSafeUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]*[$]?$`)
)

type termControlMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

func (s *Server) handleWSTerminal(w http.ResponseWriter, r *http.Request) {
	if !s.verifyWSToken(r.URL.Query().Get("token")) {
		http.Error(w, "invalid or missing websocket token", http.StatusForbidden)
		return
	}
	machine := strings.TrimSpace(r.URL.Query().Get("machine"))
	user := strings.TrimSpace(r.URL.Query().Get("user"))
	if user == "" {
		user = "root"
	}
	if !reSafeUser.MatchString(user) {
		http.Error(w, "invalid user", http.StatusBadRequest)
		return
	}

	info, err := s.mgr.GetContainer(machine)
	if err != nil || info.Runtime.ActiveState != "active" {
		http.Error(w, "container not running", http.StatusBadRequest)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	var cmd *exec.Cmd
	if info.Config.InitMode == nspawn.InitModeOpenWrt {
		pid := info.Runtime.LeaderPID
		if pid <= 0 {
			pid = info.Runtime.MainPID
		}
		if pid <= 0 {
			_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n[ERROR] 无法获取 OpenWrt 容器进程 PID，请确认容器已运行\r\n"))
			return
		}
		nsenterBin, nErr := exec.LookPath("nsenter")
		if nErr != nil {
			nsenterBin = "/usr/bin/nsenter"
		}
		shellBin := "/bin/sh"
		if info.Config.Rootfs != "" {
			if _, sErr := os.Stat(filepath.Join(info.Config.Rootfs, "bin/ash")); sErr == nil {
				shellBin = "/bin/ash"
			}
		}
		cmd = exec.Command(nsenterBin, "-t", strconv.Itoa(pid), "-a", "--", shellBin, "-l")
		cmd.Env = []string{
			"TERM=xterm-256color",
			"HOME=/root",
			"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		}
	} else {
		machinectlBin, mErr := exec.LookPath("machinectl")
		if mErr != nil {
			machinectlBin = "/usr/bin/machinectl"
		}
		cmd = exec.Command(machinectlBin, "shell", user+"@"+info.Config.Machine)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 110})
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n[ERROR] 启动终端失败: "+err.Error()+"\r\n"))
		return
	}

	var closeOnce sync.Once
	cleanup := func() {
		closeOnce.Do(func() {
			_ = ptmx.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		})
	}
	defer cleanup()

	var wsWriteMu sync.Mutex
	writeWS := func(msgType int, data []byte) error {
		wsWriteMu.Lock()
		defer wsWriteMu.Unlock()
		return conn.WriteMessage(msgType, data)
	}

	// PTY -> WebSocket
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if wErr := writeWS(websocket.BinaryMessage, buf[:n]); wErr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		cleanup()
		_ = conn.Close()
	}()

	// WebSocket -> PTY
	for {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if msgType == websocket.TextMessage && len(payload) > 0 && payload[0] == '{' {
			var ctrl termControlMsg
			if json.Unmarshal(payload, &ctrl) == nil && ctrl.Type == "resize" && ctrl.Cols > 0 && ctrl.Rows > 0 {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: ctrl.Cols, Rows: ctrl.Rows})
				continue
			}
		}
		if _, err := ptmx.Write(payload); err != nil {
			break
		}
	}
}

func (s *Server) handleWSLogs(w http.ResponseWriter, r *http.Request) {
	if !s.verifyWSToken(r.URL.Query().Get("token")) {
		http.Error(w, "invalid or missing websocket token", http.StatusForbidden)
		return
	}
	machine := strings.TrimSpace(r.URL.Query().Get("machine"))
	info, err := s.mgr.GetContainer(machine)
	if err != nil {
		http.Error(w, "container not found", http.StatusNotFound)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	cmd := exec.Command("journalctl", "-u", info.Config.ServiceName, "-n", "150", "-f", "--no-pager", "-o", "short-iso")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	// Detect client disconnect
	done := make(chan struct{})
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				close(done)
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				return
			}
		}
	}()

	sc := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 512*1024)
	for sc.Scan() {
		select {
		case <-done:
			return
		default:
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(sc.Text())); err != nil {
			return
		}
	}
}
