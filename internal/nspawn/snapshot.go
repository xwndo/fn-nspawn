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
		showOut, err := exec.Command("btrfs", "subvolume", "show", sp).Output()
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
		if propOut, err := exec.Command("btrfs", "property", "get", sp, "ro").Output(); err == nil {
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
	if req.StopFirst && wasRunning {
		if err := m.ControlService(machine, "stop"); err != nil {
			return nil, fmt.Errorf("停止容器失败: %v", err)
		}
		defer func() {
			_ = m.ControlService(machine, "start")
		}()
	}

	args := []string{"subvolume", "snapshot"}
	if req.ReadOnly {
		args = append(args, "-r")
	}
	args = append(args, info.Config.Rootfs, snapPath)

	if out, err := exec.Command("btrfs", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("创建快照失败: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	return &SnapshotInfo{
		Name:      snapName,
		Path:      snapPath,
		ReadOnly:  req.ReadOnly,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}, nil
}

func (m *Manager) DeleteSnapshot(machine string, snapName string) error {
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
	if err := exec.Command("btrfs", "subvolume", "show", snapPath).Run(); err != nil {
		return nil, fmt.Errorf("快照不是有效的 Btrfs 子卷: %s", snapPath)
	}

	task := m.tasks.Create("rollback", machine)
	go func() {
		var taskErr error
		defer func() {
			m.tasks.Finish(task.ID, taskErr)
		}()

		logFn := func(msg string) {
			m.tasks.AppendLog(task.ID, msg)
		}

		cfg := info.Config
		logFn(fmt.Sprintf("开始回滚容器 %s 到快照 %s", cfg.Machine, req.SnapshotName))

		// 1. Stop container
		logFn("1. 停止容器服务 " + cfg.ServiceName)
		if err := runCmdLogged(logFn, "systemctl", "stop", cfg.ServiceName); err != nil {
			taskErr = fmt.Errorf("停止容器失败: %v", err)
			return
		}
		time.Sleep(2 * time.Second)

		// 2. Safety backup of current rootfs
		if req.SafetyBackup {
			if err := exec.Command("btrfs", "subvolume", "show", cfg.Rootfs).Run(); err == nil {
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

		// 3. Delete current rootfs
		if err := exec.Command("btrfs", "subvolume", "show", cfg.Rootfs).Run(); err == nil {
			logFn("3. 删除当前 rootfs: " + cfg.Rootfs)
			if err := runCmdLogged(logFn, "btrfs", "subvolume", "delete", cfg.Rootfs); err != nil {
				taskErr = fmt.Errorf("删除当前 rootfs 失败: %v", err)
				return
			}
		}

		// 4. Clone writable rootfs from snapshot
		logFn(fmt.Sprintf("4. 从快照恢复可写 rootfs: %s -> %s", snapPath, cfg.Rootfs))
		if err := runCmdLogged(logFn, "btrfs", "subvolume", "snapshot", snapPath, cfg.Rootfs); err != nil {
			taskErr = fmt.Errorf("恢复快照失败: %v", err)
			return
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

func (m *Manager) StartExportImageTask(machine string, req ExportImageRequest) (*TaskRecord, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	cfg := info.Config
	exportsDir := cfg.ExportsDir
	if exportsDir == "" && cfg.ContainerDir != "" {
		exportsDir = filepath.Join(cfg.ContainerDir, "images", "exports")
	}
	if exportsDir == "" {
		return nil, fmt.Errorf("当前容器未配置镜像导出目录")
	}

	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "tar.gz"
	}
	if format != "tar.gz" && format != "tar.zst" {
		return nil, fmt.Errorf("不支持的归档格式: %s（支持 tar.gz / tar.zst）", format)
	}
	if format == "tar.zst" {
		if _, err := exec.LookPath("zstd"); err != nil {
			return nil, fmt.Errorf("宿主机未安装 zstd，请改用 tar.gz 格式")
		}
	}

	srcDir := cfg.Rootfs
	if snap := strings.TrimSpace(req.SourceSnapshot); snap != "" {
		if strings.ContainsAny(snap, `/\`) || snap == "." || snap == ".." {
			return nil, fmt.Errorf("非法的快照名称: %s", snap)
		}
		srcDir = filepath.Join(cfg.SnapshotDir, snap)
	}
	if st, err := os.Stat(srcDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("导出源目录不存在: %s", srcDir)
	}

	tag := sanitizeTag(req.Tag, "image")
	outName := fmt.Sprintf("%s-%s-%s.%s", cfg.Machine, tag, time.Now().Format("20060102-150405"), format)
	outPath := filepath.Join(exportsDir, outName)

	task := m.tasks.Create("export-image", machine)
	go func() {
		var taskErr error
		var tempSnap string
		stoppedForExport := false
		defer func() {
			if tempSnap != "" {
				_ = exec.Command("btrfs", "subvolume", "delete", tempSnap).Run()
			}
			if stoppedForExport {
				_ = exec.Command("systemctl", "start", cfg.ServiceName).Run()
			}
			m.tasks.Finish(task.ID, taskErr)
		}()

		logFn := func(msg string) {
			m.tasks.AppendLog(task.ID, msg)
		}

		if err := os.MkdirAll(exportsDir, 0755); err != nil {
			taskErr = err
			return
		}

		packDir := srcDir
		if req.SourceSnapshot == "" {
			if req.StopFirst && info.Runtime.ActiveState == "active" {
				logFn("1. 停止容器以保证文件系统一致性...")
				if err := runCmdLogged(logFn, "systemctl", "stop", cfg.ServiceName); err != nil {
					taskErr = fmt.Errorf("停止容器失败: %v", err)
					return
				}
				stoppedForExport = true
				time.Sleep(2 * time.Second)
			} else if info.Runtime.RootfsIsBtrfs && cfg.SnapshotDir != "" {
				_ = os.MkdirAll(cfg.SnapshotDir, 0755)
				tempSnap = filepath.Join(cfg.SnapshotDir, fmt.Sprintf(".tmp-export-%d", time.Now().UnixNano()))
				logFn("1. 创建只读临时 Btrfs 快照用于无停机一致性打包: " + tempSnap)
				if err := runCmdLogged(logFn, "btrfs", "subvolume", "snapshot", "-r", cfg.Rootfs, tempSnap); err != nil {
					taskErr = fmt.Errorf("创建临时快照失败: %v", err)
					return
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
			taskErr = fmt.Errorf("打包镜像失败: %v", err)
			return
		}
		if err := os.Rename(tmpOut, outPath); err != nil {
			_ = os.Remove(tmpOut)
			taskErr = err
			return
		}
		if fi, err := os.Stat(outPath); err == nil {
			logFn(fmt.Sprintf("[OK] 镜像已导出: %s (%.2f MiB)", outPath, float64(fi.Size())/1024/1024))
		} else {
			logFn("[OK] 镜像已导出: " + outPath)
		}
	}()

	return task, nil
}

func (m *Manager) DeleteImage(machine string, filename string) error {
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
