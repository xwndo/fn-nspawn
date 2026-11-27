package nspawn

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (m *Manager) StartRestoreTask(machine string) (*TaskRecord, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	if info.Config.Layout == LayoutExternal {
		return nil, fmt.Errorf("外部容器不支持直接修复，请使用“迁移为标准容器”生成标准配置目录")
	}

	task := m.tasks.Create("restore", machine)
	go func() {
		var taskErr error
		defer func() {
			m.tasks.Finish(task.ID, taskErr)
		}()

		logFn := func(msg string) {
			m.tasks.AppendLog(task.ID, msg)
		}
		cfg := info.Config

		// Reconcile from container.json if standard layout
		if cfg.Layout == LayoutStandard && cfg.ConfigFile != "" {
			if spec, sErr := ReadContainerSpec(cfg.ConfigFile); sErr == nil {
				logFn("0. 读取声明式配置文件: " + cfg.ConfigFile)
				reconciled := SpecToConfig(*spec, cfg.ContainerDir, filepath.Join("/etc/systemd/system", cfg.ServiceName))
				cfg = *reconciled
			}
		}

		logFn("1. 检查 rootfs 是否存在: " + cfg.Rootfs)
		if st, err := os.Stat(cfg.Rootfs); err != nil || !st.IsDir() {
			taskErr = fmt.Errorf("rootfs 不存在或不是目录: %s", cfg.Rootfs)
			return
		}
		if err := exec.Command("btrfs", "subvolume", "show", cfg.Rootfs).Run(); err != nil {
			logFn("[WARN] rootfs 不是 Btrfs 子卷（仍可正常启动，但无法使用 Btrfs 快照功能）")
		} else {
			logFn("[OK] rootfs 是有效的 Btrfs 子卷")
		}

		if cfg.Layout == LayoutStandard {
			_ = os.MkdirAll(cfg.ConfDir, 0755)
			_ = os.MkdirAll(cfg.ImagesDir, 0755)
			_ = os.MkdirAll(cfg.SnapshotDir, 0755)
			_ = os.MkdirAll(cfg.ExportsDir, 0755)
		}

		logFn("2. 检查 OVS 与物理网卡: " + cfg.PhysicalNIC)
		if _, err := exec.LookPath("ovs-vsctl"); err != nil {
			taskErr = fmt.Errorf("找不到 ovs-vsctl，请先在 fnOS 启用 OVS")
			return
		}
		brOut, err := exec.Command("ovs-vsctl", "port-to-br", cfg.PhysicalNIC).Output()
		ovsBridge := strings.TrimSpace(string(brOut))
		if err != nil || ovsBridge == "" {
			taskErr = fmt.Errorf("网卡 %s 当前未加入任何 OVS Bridge", cfg.PhysicalNIC)
			return
		}
		logFn("[OK] 当前 OVS Bridge: " + ovsBridge)

		// Clean legacy independent network service if present
		oldNetSvc := fmt.Sprintf("nspawn-%s-network.service", cfg.Machine)
		_ = exec.Command("systemctl", "disable", "--now", oldNetSvc).Run()
		_ = os.Remove(filepath.Join("/etc/systemd/system", oldNetSvc))

		// Reconcile scripts & systemd unit
		netUp := filepath.Join(cfg.ConfDir, "network-up.sh")
		netDown := filepath.Join(cfg.ConfDir, "network-down.sh")
		restoreSh := filepath.Join(cfg.ConfDir, "restore.sh")

		if cfg.HasConfigFile || !fileExists(netUp) {
			_ = os.WriteFile(netUp, []byte(RenderNetworkUpScript(cfg.PhysicalNIC, cfg.Veth)), 0700)
		} else {
			_ = os.Chmod(netUp, 0700)
		}
		if cfg.HasConfigFile || !fileExists(netDown) {
			_ = os.WriteFile(netDown, []byte(RenderNetworkDownScript(cfg.Veth)), 0700)
		} else {
			_ = os.Chmod(netDown, 0700)
		}
		if cfg.HasConfigFile || !fileExists(restoreSh) {
			_ = os.WriteFile(restoreSh, []byte(RenderRestoreScript(cfg.Machine, cfg.PhysicalNIC, cfg.Rootfs, cfg.ConfDir, cfg.ServiceName, cfg.Autostart)), 0700)
		} else {
			_ = os.Chmod(restoreSh, 0700)
		}
		if cfg.HasConfigFile || !fileExists(cfg.ServicePath) {
			logFn("根据 container.json 生成/同步 systemd unit: " + cfg.ServicePath)
			if err := os.WriteFile(cfg.ServicePath, []byte(RenderServiceUnit(cfg)), 0644); err != nil {
				taskErr = fmt.Errorf("写入 service 文件失败: %v", err)
				return
			}
			if vOut, err := exec.Command("systemd-analyze", "verify", cfg.ServicePath).CombinedOutput(); err != nil {
				taskErr = fmt.Errorf("systemd-analyze verify 失败: %s", strings.TrimSpace(string(vOut)))
				return
			}
		}

		// Re-link systemd unit & sync autostart without losing symlink
		sysLink := filepath.Join("/etc/systemd/system", cfg.ServiceName)
		autoCmd := "disable"
		if cfg.Autostart {
			autoCmd = "enable"
		}
		logFn(fmt.Sprintf("3. 同步 systemd 实体单元 (%s <- %s) 并应用自启 (%s)", sysLink, cfg.ServicePath, autoCmd))
		if err := applySystemdAutostart(cfg.ServicePath, cfg.ServiceName, cfg.Autostart); err != nil {
			taskErr = err
			return
		}

		logFn("4. 启动/重启容器服务: systemctl restart " + cfg.ServiceName)
		if err := runCmdLogged(logFn, "systemctl", "restart", cfg.ServiceName); err != nil {
			taskErr = fmt.Errorf("restart 失败: %v", err)
			return
		}

		time.Sleep(3 * time.Second)
		updated, err := m.GetContainer(cfg.Machine)
		if err == nil {
			logFn(fmt.Sprintf("[OK] 状态=%s/%s, 自启=%v, OVS接入=%v (%s)",
				updated.Runtime.ActiveState, updated.Runtime.SubState,
				updated.Runtime.Enabled, updated.Runtime.VethAttached, updated.Runtime.OVSBridge))
			if len(updated.Runtime.IPv4Addrs) > 0 {
				logFn("[OK] IPv4=" + strings.Join(updated.Runtime.IPv4Addrs, ", "))
			}
		}
	}()

	return task, nil
}

func resolveImportTarget(inputPath string) (resolvedPath string, configType string, containerDir string, err error) {
	p := filepath.Clean(strings.TrimSpace(inputPath))
	if p == "" || p == "." || !filepath.IsAbs(p) {
		return "", "", "", fmt.Errorf("请提供配置文件或容器目录的绝对路径")
	}
	st, statErr := os.Stat(p)
	if statErr != nil {
		return "", "", "", fmt.Errorf("路径不存在: %s", p)
	}

	if st.IsDir() {
		// 1. <dir>/config/container.json
		cand1 := filepath.Join(p, "config", ContainerSpecFileName)
		if fileExists(cand1) {
			return cand1, "container_json", p, nil
		}
		// 2. <dir>/container.json (user selected config/ dir directly)
		cand2 := filepath.Join(p, ContainerSpecFileName)
		if fileExists(cand2) {
			cDir := p
			if filepath.Base(p) == "config" {
				cDir = filepath.Dir(p)
			}
			return cand2, "container_json", cDir, nil
		}
		// 3. <dir>/config/nspawn-*.service
		if svcs, _ := filepath.Glob(filepath.Join(p, "config", "nspawn-*.service")); len(svcs) > 0 {
			return svcs[0], "service_unit", p, nil
		}
		// 4. <dir>/nspawn-*.service
		if svcs, _ := filepath.Glob(filepath.Join(p, "nspawn-*.service")); len(svcs) > 0 {
			cDir := p
			if filepath.Base(p) == "config" {
				cDir = filepath.Dir(p)
			}
			return svcs[0], "service_unit", cDir, nil
		}
		return "", "", "", fmt.Errorf("目录 %s 下未找到 config/container.json 或 nspawn-*.service", p)
	}

	base := filepath.Base(p)
	parent := filepath.Dir(p)
	if base == ContainerSpecFileName {
		cDir := parent
		if filepath.Base(parent) == "config" {
			cDir = filepath.Dir(parent)
		}
		return p, "container_json", cDir, nil
	}
	if strings.HasPrefix(base, "nspawn-") && strings.HasSuffix(base, ".service") {
		cDir := parent
		if filepath.Base(parent) == "config" {
			cDir = filepath.Dir(parent)
		}
		return p, "service_unit", cDir, nil
	}
	return "", "", "", fmt.Errorf("不支持的配置文件类型: %s（请选择 container.json、容器目录或 nspawn-*.service）", p)
}

func (m *Manager) PreviewImportFromConfig(configPath string) (*ImportPreviewResponse, error) {
	resolved, cfgType, containerDir, err := resolveImportTarget(configPath)
	if err != nil {
		return nil, err
	}

	var spec ContainerSpec
	var rootfsPath string
	if cfgType == "container_json" {
		sp, err := ReadContainerSpec(resolved)
		if err != nil {
			return nil, err
		}
		spec = *sp
		cfg := SpecToConfig(spec, containerDir, "")
		rootfsPath = cfg.Rootfs
	} else {
		cfg, err := ParseServiceFile(resolved, "")
		if err != nil {
			return nil, err
		}
		if cfg.Layout == LayoutStandard {
			containerDir = cfg.ContainerDir
			rootfsPath = filepath.Join(containerDir, "rootfs")
		} else {
			rootfsPath = cfg.Rootfs
		}
		spec = ConfigToSpec(*cfg, nil)
	}

	baseRoot := filepath.Dir(containerDir)
	rootfsExists := false
	rootfsIsBtrfs := false
	if st, err := os.Stat(rootfsPath); err == nil && st.IsDir() {
		rootfsExists = true
		if exec.Command("btrfs", "subvolume", "show", rootfsPath).Run() == nil {
			rootfsIsBtrfs = true
		}
	}

	_, findErr := m.GetContainer(spec.Machine)
	return &ImportPreviewResponse{
		ResolvedConfigPath: resolved,
		ConfigType:         cfgType,
		ContainerDir:       containerDir,
		BaseRoot:           baseRoot,
		RootfsPath:         rootfsPath,
		RootfsExists:       rootfsExists,
		RootfsIsBtrfs:      rootfsIsBtrfs,
		AlreadyRegistered:  findErr == nil,
		Spec:               spec,
	}, nil
}

func (m *Manager) StartImportTask(req ImportContainerRequest) (*TaskRecord, error) {
	preview, err := m.PreviewImportFromConfig(req.ConfigPath)
	if err != nil {
		return nil, err
	}
	if !preview.RootfsExists {
		return nil, fmt.Errorf("未找到容器 rootfs 目录: %s（请确认磁盘已挂载且目录结构完整）", preview.RootfsPath)
	}

	spec := preview.Spec
	if nic := strings.TrimSpace(req.PhysicalNIC); nic != "" {
		spec.Network.PhysicalNIC = nic
	}
	if strings.TrimSpace(spec.Network.PhysicalNIC) == "" {
		spec.Network.PhysicalNIC = m.PreferredOVSNIC()
	}
	if strings.TrimSpace(spec.Network.PhysicalNIC) == "" {
		return nil, fmt.Errorf("未检测到已开启 OVS 的物理网卡，请先在飞牛「系统设置 → 网络设置」中为物理网卡开启 OVS")
	}
	if err := exec.Command("ip", "link", "show", spec.Network.PhysicalNIC).Run(); err != nil {
		return nil, fmt.Errorf("找不到物理网卡 %s（重装系统后网卡名如有变化，请在导入弹窗中重新选择）", spec.Network.PhysicalNIC)
	}
	brOut, err := exec.Command("ovs-vsctl", "port-to-br", spec.Network.PhysicalNIC).Output()
	if err != nil || strings.TrimSpace(string(brOut)) == "" {
		return nil, fmt.Errorf("物理网卡 %s 当前未加入任何 OVS Bridge，请先在 fnOS 网络设置启用 OVS", spec.Network.PhysicalNIC)
	}

	applyAutostart := true
	if req.ApplyAutostart != nil {
		applyAutostart = *req.ApplyAutostart
	}

	task := m.tasks.Create("import-restore", spec.Machine)
	go func() {
		var taskErr error
		defer func() {
			m.tasks.Finish(task.ID, taskErr)
		}()

		logFn := func(msg string) {
			m.tasks.AppendLog(task.ID, msg)
		}

		logFn("1. 解析恢复配置文件: " + preview.ResolvedConfigPath)
		logFn("   容器目录: " + preview.ContainerDir)
		logFn("   Rootfs:   " + preview.RootfsPath)

		cfg := SpecToConfig(spec, preview.ContainerDir, filepath.Join("/etc/systemd/system", spec.System.ServiceName))
		// Keep legacy rootfs if importing a legacy *-conf unit whose rootfs is outside containerDir
		if preview.ConfigType == "service_unit" && preview.RootfsPath != "" {
			cfg.Rootfs = preview.RootfsPath
		}

		for _, dir := range []string{cfg.ConfDir, cfg.ImagesDir, cfg.SnapshotDir, cfg.ExportsDir} {
			if dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					taskErr = err
					return
				}
			}
		}

		logFn("2. 重建/更新 OVS 网络脚本、restore.sh 与格式化配置 container.json")
		if err := WriteContainerSpec(cfg.ConfigFile, spec); err != nil {
			taskErr = fmt.Errorf("写入 container.json 失败: %v", err)
			return
		}
		if err := os.WriteFile(filepath.Join(cfg.ConfDir, "network-up.sh"), []byte(RenderNetworkUpScript(cfg.PhysicalNIC, cfg.Veth)), 0700); err != nil {
			taskErr = err
			return
		}
		if err := os.WriteFile(filepath.Join(cfg.ConfDir, "network-down.sh"), []byte(RenderNetworkDownScript(cfg.Veth)), 0700); err != nil {
			taskErr = err
			return
		}
		if err := os.WriteFile(filepath.Join(cfg.ConfDir, "restore.sh"), []byte(RenderRestoreScript(cfg.Machine, cfg.PhysicalNIC, cfg.Rootfs, cfg.ConfDir, cfg.ServiceName, cfg.Autostart)), 0700); err != nil {
			taskErr = err
			return
		}

		logFn("3. 生成并校验 systemd service unit: " + cfg.ServicePath)
		if err := os.WriteFile(cfg.ServicePath, []byte(RenderServiceUnit(*cfg)), 0644); err != nil {
			taskErr = err
			return
		}
		if vOut, err := exec.Command("systemd-analyze", "verify", cfg.ServicePath).CombinedOutput(); err != nil {
			taskErr = fmt.Errorf("systemd-analyze verify 失败: %s", strings.TrimSpace(string(vOut)))
			return
		}

		_ = m.cfgStore.AddBaseRoot(preview.BaseRoot)

		if applyAutostart {
			autoCmd := "disable"
			if cfg.Autostart {
				autoCmd = "enable"
			}
			logFn(fmt.Sprintf("4. 按配置文件同步 systemd 实体单元与开机自启 (%s)", autoCmd))
			if err := applySystemdAutostart(cfg.ServicePath, cfg.ServiceName, cfg.Autostart); err != nil {
				taskErr = fmt.Errorf("设置开机自启失败: %v", err)
				return
			}
		} else {
			if err := ensureSystemdLink(cfg.ServicePath, cfg.ServiceName); err != nil {
				taskErr = err
				return
			}
			if err := runCmdLogged(logFn, "systemctl", "daemon-reload"); err != nil {
				taskErr = fmt.Errorf("systemctl daemon-reload 失败: %v", err)
				return
			}
		}

		if req.StartNow {
			logFn("5. 启动容器并检查 OVS 与 IPv4 网络...")
			if err := runCmdLogged(logFn, "systemctl", "restart", cfg.ServiceName); err != nil {
				taskErr = fmt.Errorf("启动容器失败: %v", err)
				return
			}
			time.Sleep(4 * time.Second)
			if updated, err := m.GetContainer(cfg.Machine); err == nil {
				logFn(fmt.Sprintf("[OK] 容器 %s 已恢复运行！OVS=%v (%s), IPv4=%s",
					cfg.Machine, updated.Runtime.VethAttached, updated.Runtime.OVSBridge,
					strings.Join(updated.Runtime.IPv4Addrs, ", ")))
			}
		} else {
			logFn("[OK] 容器 " + cfg.Machine + " 已从配置文件成功恢复注册！")
		}
	}()

	return task, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
