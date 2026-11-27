package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"fn-nspawn/internal/config"
	"fn-nspawn/internal/nspawn"
	"fn-nspawn/internal/server"
	"fn-nspawn/web"
)

func main() {
	var (
		socketPath = flag.String("socket", envOr("FN_NSPAWN_SOCKET", defaultSocketPath()), "Unix domain socket path for fnOS unified gateway")
		listenAddr = flag.String("listen", os.Getenv("FN_NSPAWN_LISTEN"), "Optional TCP listen address (e.g. 127.0.0.1:8099)")
		etcDir     = flag.String("etc", envOr("TRIM_PKGETC", "./data"), "Persistent config directory")
		devMode    = flag.Bool("dev", os.Getenv("FN_NSPAWN_DEV") == "1", "Disable gateway header check for local development")
	)
	flag.Parse()

	cfgStore, err := config.NewStore(*etcDir)
	if err != nil {
		log.Fatalf("初始化配置失败: %v", err)
	}

	tasks := nspawn.NewTaskManager()
	mgr := nspawn.NewManager(cfgStore, tasks)
	if !*devMode {
		go func() {
			mgr.EnsureDefaultOVSNIC()
			mgr.ReconcileOnStartup()
		}()
	}
	srv := server.New(cfgStore, mgr, web.StaticFS(), *devMode)

	var listeners []net.Listener

	if *socketPath != "" && *listenAddr == "" || (*socketPath != "" && os.Getenv("TRIM_APPDEST") != "") {
		if err := os.MkdirAll(filepath.Dir(*socketPath), 0755); err != nil {
			log.Fatalf("创建 socket 目录失败: %v", err)
		}
		_ = os.Remove(*socketPath)
		unixLn, err := net.Listen("unix", *socketPath)
		if err != nil {
			log.Fatalf("监听 Unix Socket 失败 (%s): %v", *socketPath, err)
		}
		_ = os.Chmod(*socketPath, 0666)
		listeners = append(listeners, unixLn)
		log.Printf("fn-nspawn 已监听 Unix Socket: %s", *socketPath)
	}

	if *listenAddr != "" {
		tcpLn, err := net.Listen("tcp", *listenAddr)
		if err != nil {
			log.Fatalf("监听 TCP 失败 (%s): %v", *listenAddr, err)
		}
		listeners = append(listeners, tcpLn)
		log.Printf("fn-nspawn 已监听 TCP: http://%s", *listenAddr)
	}

	if len(listeners) == 0 {
		log.Fatalf("未指定任何监听地址 (--socket 或 --listen)")
	}

	httpServer := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
	}

	errCh := make(chan error, len(listeners))
	for _, ln := range listeners {
		l := ln
		go func() {
			if err := httpServer.Serve(l); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("收到退出信号 %v，正在关闭服务...", sig)
	case err := <-errCh:
		log.Printf("服务异常退出: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	if *socketPath != "" {
		_ = os.Remove(*socketPath)
	}
}

func defaultSocketPath() string {
	if dest := os.Getenv("TRIM_APPDEST"); dest != "" {
		return filepath.Join(dest, "app.sock")
	}
	return "/var/apps/fn-nspawn/target/app.sock"
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
