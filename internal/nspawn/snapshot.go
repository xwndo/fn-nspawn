package nspawn

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (m *Manager) ListSnapshots(machine string) ([]SnapshotInfo, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	snapDir := info.Config.SnapshotDir
	if snapDir == "" {
		return []SnapshotInfo{}, nil
	}
	if _, err := os.Stat(snapDir); os.IsNotExist(err) {
		return []SnapshotInfo{}, nil
	}

	entries, err := os.ReadDir(snapDir)
	if err != nil {
		return nil, err
	}

	prefix := info.Config.Machine + "-"
	var snaps []SnapshotInfo
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		sp := filepath.Join(snapDir, e.Name())
		showOut, err := execOutputTimeout(defaultProbeTimeout, "btrfs", "subvolume", "show", sp)
		if err != nil {
			continue
		}
		sn := SnapshotInfo{
			Name: e.Name(),
			Path: sp,
		}
		for _, line := range strings.Split(string(showOut), "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "UUID:"):
				sn.UUID = strings.TrimSpace(strings.TrimPrefix(trimmed, "UUID:"))
			case strings.HasPrefix(trimmed, "Parent UUID:"):
				sn.ParentUUID = strings.TrimSpace(strings.TrimPrefix(trimmed, "Parent UUID:"))
			case strings.HasPrefix(trimmed, "Creation time:"):
				sn.CreatedAt = strings.TrimSpace(strings.TrimPrefix(trimmed, "Creation time:"))
			case strings.HasPrefix(trimmed, "Flags:"):
				flags := strings.ToLower(strings.TrimPrefix(trimmed, "Flags:"))
				if strings.Contains(flags, "readonly") {
					sn.ReadOnly = true
				}
			}
		}
		if propOut, err := execOutputTimeout(defaultProbeTimeout, "btrfs", "property", "get", sp, "ro"); err == nil {
			if strings.Contains(string(propOut), "ro=true") {
				sn.ReadOnly = true
			}
		}
		snaps = append(snaps, sn)
	}

	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].Name > snaps[j].Name
	})
	return snaps, nil
}

func sanitizeTag(tag string, fallback string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = fallback
	}
	var cleanTag strings.Builder
	for _, r := range tag {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			cleanTag.WriteRune(r)
		}
	}
	if cleanTag.Len() == 0 {
		return fallback
	}
	return cleanTag.String()
}

func (m *Manager) CreateSnapshot(machine string, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	releaseOp, err := m.acquireContainerOp("create-snapshot", machine)
	if err != nil {
		return nil, err
	}
	defer releaseOp()
	return m.createSnapshotUnlocked(machine, req)
}

func (m *Manager) createSnapshotUnlocked(machine string, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	if !info.Runtime.RootfsExists || !info.Runtime.RootfsIsBtrfs {
		return nil, fmt.Errorf("容器 rootfs 不是有效的 Btrfs 子卷: %s", info.Config.Rootfs)
	}

	tag := sanitizeTag(req.Tag, "manual")
	if err := os.MkdirAll(info.Config.SnapshotDir, 0755); err != nil {
		return nil, err
	}

	snapName := fmt.Sprintf("%s-%s-%s", info.Config.Machine, tag, time.Now().Format("20060102-150405"))
	snapPath := filepath.Join(info.Config.SnapshotDir, snapName)

	wasRunning := info.Runtime.ActiveState == "active"
	stopped := false
	if req.StopFirst && wasRunning {
		if err := m.controlServiceUnlocked(machine, "stop"); err != nil {
			return nil, fmt.Errorf("停止容器失败: %v", err)
		}
		stopped = true
	}

	args := []string{"subvolume", "snapshot"}
	if req.ReadOnly {
		args = append(args, "-r")
	}
	args = append(args, info.Config.Rootfs, snapPath)

	out, snapErr := exec.Command("btrfs", args...).CombinedOutput()
	if stopped {
		if startErr := m.controlServiceUnlocked(machine, "start"); startErr != nil {
			if snapErr != nil {
				return nil, fmt.Errorf("创建快照失败: %v (%s)；且重新启动容器失败: %v", snapErr, strings.TrimSpace(string(out)), startErr)
			}
			return nil, fmt.Errorf("快照 %s 已创建，但重新启动容器失败: %v", snapName, startErr)
		}
	}
	if snapErr != nil {
		return nil, fmt.Errorf("创建快照失败: %v (%s)", snapErr, strings.TrimSpace(string(out)))
	}

	return &SnapshotInfo{
		Name:      snapName,
		Path:      snapPath,
		ReadOnly:  req.ReadOnly,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}, nil
}

func (m *Manager) DeleteSnapshot(machine string, snapName string) error {
	releaseOp, err := m.acquireContainerOp("delete-snapshot", machine)
	if err != nil {
		return err
	}
	defer releaseOp()
	return m.deleteSnapshotUnlocked(machine, snapName)
}

func (m *Manager) deleteSnapshotUnlocked(machine string, snapName string) error {
	info, err := m.GetContainer(machine)
	if err != nil {
		return err
	}
	if snapName == "" || strings.ContainsAny(snapName, `/\`) || snapName == "." || snapName == ".." {
		return fmt.Errorf("非法的快照名称: %s", snapName)
	}
	if !strings.HasPrefix(snapName, info.Config.Machine+"-") {
		return fmt.Errorf("快照名称与容器不匹配: %s", snapName)
	}
	snapPath := filepath.Join(info.Config.SnapshotDir, snapName)
	if out, err := exec.Command("btrfs", "subvolume", "delete", snapPath).CombinedOutput(); err != nil {
		return fmt.Errorf("删除快照失败: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Manager) StartRollbackTask(machine string, req RollbackRequest) (*TaskRecord, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	if req.SnapshotName == "" || strings.ContainsAny(req.SnapshotName, `/\`) {
		return nil, fmt.Errorf("非法的快照名称: %s", req.SnapshotName)
	}
	snapPath := filepath.Join(info.Config.SnapshotDir, req.SnapshotName)
	if err := execRunTimeout(defaultProbeTimeout, "btrfs", "subvolume", "show", snapPath); err != nil {
		return nil, fmt.Errorf("快照不是有效的 Btrfs 子卷: %s", snapPath)
	}

	releaseOp, err := m.acquireContainerOp("rollback", machine)
	if err != nil {
		return nil, err
	}

	task := m.tasks.Create("rollback", machine)
	go func() {
		defer releaseOp()
		var taskErr error
		defer func() {
			m.tasks.Finish(task.ID, taskErr)
		}()

		logFn := func(msg string) {
			m.tasks.AppendLog(task.ID, msg)
		}

		cfg := info.Config
		logFn(fmt.Sprintf("开始回滚容器 %s 到快照 %s", cfg.Machine, req.SnapshotName))

		// 1. Stop container and wait until inactive so rootfs is cleanly released
		logFn("1. 停止容器服务 " + cfg.ServiceName)
		if err := runCmdLogged(logFn, "systemctl", "stop", cfg.ServiceName); err != nil {
			taskErr = fmt.Errorf("停止容器失败: %v", err)
			return
		}
		machinectlBin := lookBinary("machinectl", "/usr/bin/machinectl", "/bin/machinectl")
		for i := 0; i < 15; i++ {
			actOut, _ := execOutputTimeout(2*time.Second, "systemctl", "is-active", cfg.ServiceName)
			mOut, _ := execOutputTimeout(2*time.Second, machinectlBin, "show", cfg.Machine, "--property=State", "--no-pager")
			if strings.TrimSpace(string(actOut)) != "active" && len(strings.TrimSpace(string(mOut))) == 0 {
				break
			}
			time.Sleep(1 * time.Second)
		}

		// 2. Safety backup of current rootfs
		if req.SafetyBackup {
			if execRunTimeout(defaultProbeTimeout, "btrfs", "subvolume", "show", cfg.Rootfs) == nil {
				_ = os.MkdirAll(cfg.SnapshotDir, 0755)
				safetyName := fmt.Sprintf("%s-before-rollback-%s", cfg.Machine, time.Now().Format("20060102-150405"))
				safetyPath := filepath.Join(cfg.SnapshotDir, safetyName)
				logFn("2. 创建回滚前只读保险快照: " + safetyPath)
				if err := runCmdLogged(logFn, "btrfs", "subvolume", "snapshot", "-r", cfg.Rootfs, safetyPath); err != nil {
					taskErr = fmt.Errorf("创建保险快照失败: %v", err)
					return
				}
			} else {
				logFn("2. 当前 rootfs 不存在或非 Btrfs 子卷，跳过保险快照")
			}
		}

		// 3. Stage current rootfs aside before creating replacement so failure can roll back cleanly
		var stagedOldRootfs string
		if _, stErr := os.Stat(cfg.Rootfs); stErr == nil {
			stagedOldRootfs = fmt.Sprintf("%s.rollback-old-%d", cfg.Rootfs, time.Now().UnixNano())
			logFn("3. 暂存旧 rootfs 并从快照克隆新可写子卷...")
			if err := os.Rename(cfg.Rootfs, stagedOldRootfs); err != nil {
				taskErr = fmt.Errorf("暂存当前 rootfs 失败: %v", err)
				return
			}
		}

		// 4. Clone writable rootfs from snapshot
		logFn(fmt.Sprintf("4. 从快照恢复可写 rootfs: %s -> %s", snapPath, cfg.Rootfs))
		if err := runCmdLogged(logFn, "btrfs", "subvolume", "snapshot", snapPath, cfg.Rootfs); err != nil {
			if stagedOldRootfs != "" {
				_ = os.Rename(stagedOldRootfs, cfg.Rootfs)
				logFn("[WARN] 恢复快照失败，已自动还原原 rootfs 目录")
			}
			taskErr = fmt.Errorf("恢复快照失败: %v", err)
			return
		}

		if stagedOldRootfs != "" {
			if err := runCmdLogged(logFn, "btrfs", "subvolume", "delete", stagedOldRootfs); err != nil {
				_ = os.RemoveAll(stagedOldRootfs)
			}
		}

		// 5. Start container
		logFn("5. 启动容器服务 " + cfg.ServiceName)
		if err := runCmdLogged(logFn, "systemctl", "start", cfg.ServiceName); err != nil {
			taskErr = fmt.Errorf("启动容器失败: %v", err)
			return
		}

		// 6. Verify
		logFn("6. 等待容器与网络就绪...")
		time.Sleep(4 * time.Second)
		updated, err := m.GetContainer(cfg.Machine)
		if err != nil {
			taskErr = err
			return
		}
		if updated.Runtime.ActiveState != "active" {
			taskErr = fmt.Errorf("容器未处于 active 状态 (当前: %s/%s)", updated.Runtime.ActiveState, updated.Runtime.SubState)
			return
		}
		logFn(fmt.Sprintf("[OK] 容器服务已运行，OVS 桥接: %v (%s)", updated.Runtime.VethAttached, updated.Runtime.OVSBridge))
		if len(updated.Runtime.IPv4Addrs) > 0 {
			logFn("[OK] 容器 IPv4: " + strings.Join(updated.Runtime.IPv4Addrs, ", "))
		}
	}()

	return task, nil
}

func (m *Manager) ListImages(machine string) ([]ImageArchiveInfo, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	exportsDir := info.Config.ExportsDir
	if exportsDir == "" && info.Config.ContainerDir != "" {
		exportsDir = filepath.Join(info.Config.ContainerDir, "images", "exports")
	}
	if exportsDir == "" {
		return []ImageArchiveInfo{}, nil
	}
	if _, err := os.Stat(exportsDir); os.IsNotExist(err) {
		return []ImageArchiveInfo{}, nil
	}
	entries, err := os.ReadDir(exportsDir)
	if err != nil {
		return nil, err
	}

	var out []ImageArchiveInfo
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		lower := strings.ToLower(e.Name())
		var format string
		switch {
		case strings.HasSuffix(lower, ".tar.zst"):
			format = "tar.zst"
		case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
			format = "tar.gz"
		case strings.HasSuffix(lower, ".tar.xz"):
			format = "tar.xz"
		case strings.HasSuffix(lower, ".tar"):
			format = "tar"
		default:
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, ImageArchiveInfo{
			Name:      e.Name(),
			Path:      filepath.Join(exportsDir, e.Name()),
			SizeBytes: fi.Size(),
			Format:    format,
			CreatedAt: fi.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name > out[j].Name
	})
	return out, nil
}

func validateExportImageRequest(cfg ContainerConfig, req ExportImageRequest) (string, string, string, string, error) {
	exportsDir := cfg.ExportsDir
	if exportsDir == "" && cfg.ContainerDir != "" {
		exportsDir = filepath.Join(cfg.ContainerDir, "images", "exports")
	}
	if exportsDir == "" {
		return "", "", "", "", fmt.Errorf("当前容器未配置镜像导出目录")
	}

	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "tar.gz"
	}
	if format != "tar.gz" && format != "tar.zst" {
		return "", "", "", "", fmt.Errorf("不支持的归档格式: %s（支持 tar.gz / tar.zst）", format)
	}
	if format == "tar.zst" {
		if _, err := exec.LookPath("zstd"); err != nil {
			return "", "", "", "", fmt.Errorf("宿主机未安装 zstd，请改用 tar.gz 格式")
		}
	}

	srcDir := cfg.Rootfs
	if snap := strings.TrimSpace(req.SourceSnapshot); snap != "" {
		if strings.ContainsAny(snap, `/\`) || snap == "." || snap == ".." {
			return "", "", "", "", fmt.Errorf("非法的快照名称: %s", snap)
		}
		srcDir = filepath.Join(cfg.SnapshotDir, snap)
	}
	if st, err := os.Stat(srcDir); err != nil || !st.IsDir() {
		return "", "", "", "", fmt.Errorf("导出源目录不存在: %s", srcDir)
	}

	tag := sanitizeTag(req.Tag, "image")
	outName := fmt.Sprintf("%s-%s-%s.%s", cfg.Machine, tag, time.Now().Format("20060102-150405"), format)
	return exportsDir, format, srcDir, filepath.Join(exportsDir, outName), nil
}

func (m *Manager) exportImageSync(logFn func(string), info *ContainerInfo, req ExportImageRequest) (string, error) {
	cfg := info.Config
	exportsDir, format, srcDir, outPath, err := validateExportImageRequest(cfg, req)
	if err != nil {
		return "", err
	}

	var tempSnap string
	stoppedForExport := false
	defer func() {
		if tempSnap != "" {
			_ = exec.Command("btrfs", "subvolume", "delete", tempSnap).Run()
		}
		if stoppedForExport {
			_ = exec.Command("systemctl", "start", cfg.ServiceName).Run()
		}
	}()

	if err := os.MkdirAll(exportsDir, 0755); err != nil {
		return "", err
	}

	packDir := srcDir
	if req.SourceSnapshot == "" {
		if req.StopFirst && info.Runtime.ActiveState == "active" {
			logFn("1. 停止容器以保证文件系统一致性...")
			if err := runCmdLogged(logFn, "systemctl", "stop", cfg.ServiceName); err != nil {
				return "", fmt.Errorf("停止容器失败: %v", err)
			}
			stoppedForExport = true
			time.Sleep(2 * time.Second)
		} else if info.Runtime.RootfsIsBtrfs && cfg.SnapshotDir != "" {
			_ = os.MkdirAll(cfg.SnapshotDir, 0755)
			tempSnap = filepath.Join(cfg.SnapshotDir, fmt.Sprintf(".tmp-export-%d", time.Now().UnixNano()))
			logFn("1. 创建只读临时 Btrfs 快照用于无停机一致性打包: " + tempSnap)
			if err := runCmdLogged(logFn, "btrfs", "subvolume", "snapshot", "-r", cfg.Rootfs, tempSnap); err != nil {
				return "", fmt.Errorf("创建临时快照失败: %v", err)
			}
			packDir = tempSnap
		}
	} else {
		logFn("1. 从指定快照打包镜像: " + srcDir)
	}

	tmpOut := outPath + ".tmp"
	logFn(fmt.Sprintf("2. 开始打包压缩 rootfs -> %s（耗时视容器体积而定，请耐心等待）...", outPath))
	var tarArgs []string
	if format == "tar.zst" {
		tarArgs = []string{"-I", "zstd -T0 -3", "--numeric-owner", "--one-file-system", "-cpf", tmpOut, "-C", packDir, "."}
	} else {
		tarArgs = []string{"--numeric-owner", "--one-file-system", "-czpf", tmpOut, "-C", packDir, "."}
	}
	if err := runCmdLogged(logFn, "tar", tarArgs...); err != nil {
		_ = os.Remove(tmpOut)
		return "", fmt.Errorf("打包镜像失败: %v", err)
	}
	if err := os.Rename(tmpOut, outPath); err != nil {
		_ = os.Remove(tmpOut)
		return "", err
	}
	if fi, err := os.Stat(outPath); err == nil {
		logFn(fmt.Sprintf("[OK] 镜像已导出: %s (%.2f MiB)", outPath, float64(fi.Size())/1024/1024))
	} else {
		logFn("[OK] 镜像已导出: " + outPath)
	}
	if stoppedForExport {
		stoppedForExport = false
		logFn("3. 重新启动容器服务 " + cfg.ServiceName)
		if err := runCmdLogged(logFn, "systemctl", "start", cfg.ServiceName); err != nil {
			return outPath, fmt.Errorf("镜像已导出 (%s)，但重新启动容器失败: %v", filepath.Base(outPath), err)
		}
	}
	return outPath, nil
}

func (m *Manager) StartExportImageTask(machine string, req ExportImageRequest) (*TaskRecord, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	if _, _, _, _, err := validateExportImageRequest(info.Config, req); err != nil {
		return nil, err
	}

	releaseOp, err := m.acquireContainerOp("export-image", machine)
	if err != nil {
		return nil, err
	}

	task := m.tasks.Create("export-image", machine)
	go func() {
		defer releaseOp()
		logFn := func(msg string) {
			m.tasks.AppendLog(task.ID, msg)
		}
		_, taskErr := m.exportImageSync(logFn, info, req)
		m.tasks.Finish(task.ID, taskErr)
	}()

	return task, nil
}

func (m *Manager) DeleteImage(machine string, filename string) error {
	releaseOp, err := m.acquireContainerOp("delete-image", machine)
	if err != nil {
		return err
	}
	defer releaseOp()
	return m.deleteImageUnlocked(machine, filename)
}

func (m *Manager) deleteImageUnlocked(machine string, filename string) error {
	info, err := m.GetContainer(machine)
	if err != nil {
		return err
	}
	if filename == "" || strings.ContainsAny(filename, `/\`) || filename == "." || filename == ".." || strings.HasPrefix(filename, ".") {
		return fmt.Errorf("非法的镜像文件名: %s", filename)
	}
	exportsDir := info.Config.ExportsDir
	if exportsDir == "" && info.Config.ContainerDir != "" {
		exportsDir = filepath.Join(info.Config.ContainerDir, "images", "exports")
	}
	target := filepath.Join(exportsDir, filename)
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("镜像文件不存在: %s", target)
	}
	return os.Remove(target)
}
