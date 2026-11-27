package nspawn

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (m *Manager) StartMigrateTask(machine string, req MigrateContainerRequest) (*TaskRecord, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	srcCfg := info.Config
	if srcCfg.Rootfs == "" || srcCfg.Rootfs == "/" {
		return nil, fmt.Errorf("未识别到容器有效的 rootfs 路径，无法迁移")
	}
	if st, err := os.Stat(srcCfg.Rootfs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("源容器 rootfs 目录不存在或不可访问: %s", srcCfg.Rootfs)
	}
	if srcCfg.InStandardRoot {
		return nil, fmt.Errorf("容器 %s 已经位于标准容器目录中，无需迁移", srcCfg.Machine)
	}

	targetMachine := strings.TrimSpace(req.TargetMachine)
	if targetMachine == "" {
		targetMachine = srcCfg.Machine
	}
	if !reMachineName.MatchString(targetMachine) {
		return nil, fmt.Errorf("目标容器名格式不正确: %s", targetMachine)
	}
	veth := "ve-" + targetMachine
	if len(veth) > 15 {
		return nil, fmt.Errorf("veth 名称过长 (%s 超过 15 个字符)，请将目标容器名控制在 12 个字符以内", veth)
	}

	appCfg := m.cfgStore.Get()
	targetBaseRoot := strings.TrimSpace(req.TargetBaseRoot)
	if targetBaseRoot == "" {
		targetBaseRoot = appCfg.StandardBaseRoot
	}
	targetBaseRoot = filepath.Clean(targetBaseRoot)
	if targetBaseRoot == "" || targetBaseRoot == "." || !filepath.IsAbs(targetBaseRoot) {
		return nil, fmt.Errorf("目标标准容器根目录必须是绝对路径")
	}

	paths := BuildStandardPaths(targetBaseRoot, targetMachine)
	if filepath.Clean(srcCfg.Rootfs) == filepath.Clean(paths.Rootfs) {
		return nil, fmt.Errorf("目标 rootfs 与源 rootfs 路径相同，请更换目标目录或目标容器名称")
	}
	if _, err := os.Stat(paths.Rootfs); err == nil {
		return nil, fmt.Errorf("目标 rootfs 已存在，为避免覆盖已中止: %s", paths.Rootfs)
	}
	if _, err := os.Stat(paths.ConfigFile); err == nil {
		return nil, fmt.Errorf("目标容器配置已存在，为避免覆盖已中止: %s", paths.ConfigFile)
	}

	nic := strings.TrimSpace(req.PhysicalNIC)
	if nic == "" {
		nic = srcCfg.PhysicalNIC
	}
	if nic == "" {
		nic = appCfg.DefaultPhysicalNIC
	}
	if err := exec.Command("ip", "link", "show", nic).Run(); err != nil {
		return nil, fmt.Errorf("找不到物理网卡 %s，请在迁移窗口中重新选择", nic)
	}
	brOut, err := exec.Command("ovs-vsctl", "port-to-br", nic).Output()
	if err != nil || strings.TrimSpace(string(brOut)) == "" {
		return nil, fmt.Errorf("物理网卡 %s 当前未开启 OVS（未加入任何 OVS Bridge），请先在飞牛网络设置中开启该网卡的 OVS", nic)
	}

	autostart := srcCfg.Autostart || info.Runtime.Enabled
	if req.Autostart != nil {
		autostart = *req.Autostart
	}

	task := m.tasks.Create("migrate-standard", targetMachine)
	go m.runMigrateTask(task.ID, info, paths, targetMachine, veth, nic, autostart, req.StartNow)
	return task, nil
}

func (m *Manager) runMigrateTask(
	taskID string,
	srcInfo *ContainerInfo,
	paths StandardPaths,
	targetMachine string,
	veth string,
	physicalNIC string,
	autostart bool,
	startNow bool,
) {
	var taskErr error
	cleanupTarget := true
	defer func() {
		if taskErr != nil && cleanupTarget {
			_ = exec.Command("btrfs", "subvolume", "delete", paths.Rootfs).Run()
			_ = os.RemoveAll(paths.ContainerDir)
		}
		m.tasks.Finish(taskID, taskErr)
	}()

	logFn := func(msg string) {
		m.tasks.AppendLog(taskID, msg)
	}

	srcCfg := srcInfo.Config
	srcRt := srcInfo.Runtime

	logFn("0. 安全迁移规划（全程保留原容器全部数据，仅停用并清理原开机启动项）")
	logFn("   源容器:   " + srcCfg.Machine + " (layout=" + srcCfg.Layout + ")")
	logFn("   源 Rootfs: " + srcCfg.Rootfs)
	logFn("   新目录:   " + paths.ContainerDir)

	// 1. Stop source container safely before cloning rootfs
	if srcRt.ActiveState == "active" || srcRt.MachinectlUp {
		logFn("1. 安全停止原容器，确保文件系统一致性...")
		if strings.HasSuffix(srcCfg.ServiceName, ".service") {
			_ = exec.Command("systemctl", "stop", srcCfg.ServiceName).Run()
		}
		machinectlBin := lookBinary("machinectl", "/usr/bin/machinectl", "/bin/machinectl")
		_ = exec.Command(machinectlBin, "poweroff", srcCfg.Machine).Run()

		stopped := false
		for i := 0; i < 20; i++ {
			out, _ := exec.Command(machinectlBin, "show", srcCfg.Machine, "--property=State", "--no-pager").Output()
			activeOut, _ := exec.Command("systemctl", "is-active", srcCfg.ServiceName).Output()
			if len(strings.TrimSpace(string(out))) == 0 && strings.TrimSpace(string(activeOut)) != "active" {
				stopped = true
				break
			}
			time.Sleep(1 * time.Second)
		}
		if !stopped {
			logFn("   等待优雅关机超时，尝试 machinectl terminate " + srcCfg.Machine)
			_ = exec.Command(machinectlBin, "terminate", srcCfg.Machine).Run()
			if strings.HasSuffix(srcCfg.ServiceName, ".service") {
				_ = exec.Command("systemctl", "stop", srcCfg.ServiceName).Run()
			}
			time.Sleep(2 * time.Second)
		}
		logFn("[OK] 原容器已停止")
	} else {
		logFn("1. 原容器处于停止状态，直接开始安全克隆")
	}

	// 2. Prepare standard directory tree & clone rootfs non-destructively
	for _, dir := range []string{paths.ContainerDir, paths.ConfDir, paths.ImagesDir, paths.SnapshotDir, paths.ExportsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			taskErr = err
			return
		}
	}

	logFn("2. 无损复刻 rootfs 到标准目录（不修改原 rootfs）...")
	isSrcBtrfs := exec.Command("btrfs", "subvolume", "show", srcCfg.Rootfs).Run() == nil
	isDstBtrfs := IsBtrfsPath(paths.ContainerDir)
	cloned := false

	if isSrcBtrfs && isDstBtrfs {
		if err := runCmdLogged(logFn, "btrfs", "subvolume", "snapshot", srcCfg.Rootfs, paths.Rootfs); err == nil {
			cloned = true
			logFn("[OK] 同存储池 Btrfs 瞬时快照克隆完成")
		} else {
			logFn("[INFO] 跨文件系统或非同池快照不可用，转为完整目录复制模式...")
			_ = os.RemoveAll(paths.Rootfs)
		}
	}

	if !cloned {
		if isDstBtrfs {
			if err := runCmdLogged(logFn, "btrfs", "subvolume", "create", paths.Rootfs); err != nil {
				taskErr = fmt.Errorf("创建目标 Btrfs 子卷失败: %v", err)
				return
			}
		} else if err := os.MkdirAll(paths.Rootfs, 0755); err != nil {
			taskErr = err
			return
		}

		if rsyncBin, err := exec.LookPath("rsync"); err == nil {
			logFn("   使用 rsync -aHAX --numeric-ids 完整复制文件属性与权限...")
			if err := runStreamCmd(logFn, nil, rsyncBin, "-aHAX", "--numeric-ids", srcCfg.Rootfs+"/", paths.Rootfs+"/"); err != nil {
				taskErr = fmt.Errorf("rsync 复制 rootfs 失败: %v", err)
				return
			}
		} else {
			logFn("   使用 cp -a --reflink=auto 复制 rootfs...")
			if err := runStreamCmd(logFn, nil, "cp", "-a", "--reflink=auto", srcCfg.Rootfs+"/.", paths.Rootfs+"/"); err != nil {
				taskErr = fmt.Errorf("cp 复制 rootfs 失败: %v", err)
				return
			}
		}
		logFn("[OK] rootfs 完整复制完成")
	}

	// 3. Ensure cloned container network & hostname (without touching original rootfs)
	netDir := filepath.Join(paths.Rootfs, "etc/systemd/network")
	_ = os.MkdirAll(netDir, 0755)
	netCfgFile := filepath.Join(netDir, "20-host0.network")
	if !fileExists(netCfgFile) {
		netCfg := "[Match]\nName=host0\n\n[Network]\nDHCP=yes\n\n[DHCPv4]\nUseDNS=yes\nUseRoutes=yes\n"
		_ = os.WriteFile(netCfgFile, []byte(netCfg), 0644)
	}
	_ = exec.Command("systemctl", "--root="+paths.Rootfs, "enable", "systemd-networkd.service").Run()
	if targetMachine != srcCfg.Machine {
		_ = os.WriteFile(filepath.Join(paths.Rootfs, "etc/hostname"), []byte(targetMachine+"\n"), 0644)
	}

	// 4. Generate standardized config/ (5 files) and verify
	logFn("3. 按原容器配置生成标准化 config/（container.json + .service + OVS 脚本 + restore.sh）...")
	tmpSize := srcCfg.TmpSize
	if tmpSize == "" {
		tmpSize = "4G"
	}
	tasksMax := srcCfg.TasksMax
	if tasksMax == "" {
		tasksMax = "infinity"
	}
	sourceType := srcCfg.SourceType
	if sourceType == "" {
		sourceType = "migrated"
	}
	desc := srcCfg.Description
	if desc == "" {
		desc = fmt.Sprintf("由 %s (%s) 安全迁移至标准目录", srcCfg.Machine, srcCfg.Layout)
	}

	spec := ContainerSpec{
		SchemaVersion: ContainerSpecSchemaVersion,
		Machine:       targetMachine,
		Description:   desc,
		Layout:        DefaultSpecLayout(),
		System: SpecSystem{
			ContainerUser: srcCfg.ContainerUser,
			Hostname:      targetMachine,
			Autostart:     autostart,
			ServiceName:   paths.ServiceName,
		},
		Image: SpecImage{
			SourceType:     sourceType,
			Distro:         srcCfg.Distro,
			Suite:          srcCfg.Suite,
			Arch:           srcCfg.Arch,
			Mirror:         srcCfg.Mirror,
			SecurityMirror: srcCfg.SecurityMirror,
			SourceArchive:  srcCfg.SourceArchive,
		},
		Network: SpecNetwork{
			Mode:        "ovs-veth",
			PhysicalNIC: physicalNIC,
			Veth:        veth,
			DHCP:        true,
		},
		Resources: SpecResources{
			TmpSize:   tmpSize,
			MemoryMax: srcCfg.MemoryMax,
			CPUQuota:  srcCfg.CPUQuota,
			TasksMax:  tasksMax,
		},
		BindMounts: append([]BindMount(nil), srcCfg.BindMounts...),
	}

	if err := WriteContainerSpec(paths.ConfigFile, spec); err != nil {
		taskErr = fmt.Errorf("写入 container.json 失败: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(paths.ConfDir, "network-up.sh"), []byte(RenderNetworkUpScript(physicalNIC, veth)), 0700); err != nil {
		taskErr = err
		return
	}
	if err := os.WriteFile(filepath.Join(paths.ConfDir, "network-down.sh"), []byte(RenderNetworkDownScript(veth)), 0700); err != nil {
		taskErr = err
		return
	}
	if err := os.WriteFile(filepath.Join(paths.ConfDir, "restore.sh"), []byte(RenderRestoreScript(targetMachine, physicalNIC, paths.Rootfs, paths.ConfDir, paths.ServiceName, autostart)), 0700); err != nil {
		taskErr = err
		return
	}

	unitCfg := *SpecToConfig(spec, paths.ContainerDir, filepath.Join("/etc/systemd/system", paths.ServiceName))
	if err := os.WriteFile(paths.ServicePath, []byte(RenderServiceUnit(unitCfg)), 0644); err != nil {
		taskErr = err
		return
	}
	if vOut, err := exec.Command("systemd-analyze", "verify", paths.ServicePath).CombinedOutput(); err != nil {
		taskErr = fmt.Errorf("systemd-analyze verify 失败: %s", strings.TrimSpace(string(vOut)))
		return
	}

	// New standard container files are valid on disk; keep target dir even if later start fails.
	cleanupTarget = false

	// 5. Cutover systemd registration: disable & remove ONLY old systemd registration, keeping all source data intact
	logFn("4. 移除旧容器 systemd 启动项与注册单元（原数据目录全部完整保留）...")
	oldUnit := strings.TrimSpace(srcCfg.ServiceName)
	if oldUnit != "" && strings.HasSuffix(oldUnit, ".service") {
		_ = exec.Command("systemctl", "disable", oldUnit).Run()
		_ = os.Remove(filepath.Join("/etc/systemd/system/multi-user.target.wants", oldUnit))
		if oldUnit != paths.ServiceName {
			_ = os.Remove(filepath.Join("/etc/systemd/system", oldUnit))
		} else {
			// Same service name: remove old link/unit before installing the new regular copy
			_ = os.Remove(filepath.Join("/etc/systemd/system", oldUnit))
		}
	}
	if srcCfg.Machine != "" {
		legacyNetSvc := fmt.Sprintf("nspawn-%s-network.service", srcCfg.Machine)
		_ = exec.Command("systemctl", "disable", "--now", legacyNetSvc).Run()
		_ = os.Remove(filepath.Join("/etc/systemd/system/multi-user.target.wants", legacyNetSvc))
		_ = os.Remove(filepath.Join("/etc/systemd/system", legacyNetSvc))
	}

	if err := applySystemdAutostart(paths.ServicePath, paths.ServiceName, autostart); err != nil {
		taskErr = fmt.Errorf("注册新标准 systemd 单元失败: %v", err)
		return
	}
	_ = m.cfgStore.AddBaseRoot(paths.BaseRoot)

	if startNow {
		logFn("5. 启动新标准容器并等待 OVS 与 DHCP IPv4 就绪...")
		if err := runCmdLogged(logFn, "systemctl", "restart", paths.ServiceName); err != nil {
			taskErr = fmt.Errorf("新容器启动失败: %v", err)
			return
		}
		var gotIP string
		var gotMAC string
		for i := 0; i < 35; i++ {
			updated, err := m.GetContainer(targetMachine)
			if err == nil && len(updated.Runtime.IPv4Addrs) > 0 {
				gotIP = strings.Join(updated.Runtime.IPv4Addrs, ", ")
				gotMAC = updated.Runtime.Host0MAC
				break
			}
			time.Sleep(1 * time.Second)
		}
		logFn(fmt.Sprintf("[OK] 迁移完成并已运行！MAC=%s, IPv4=%s", gotMAC, gotIP))
	} else {
		logFn("[OK] 已安全迁移至标准容器目录并完成 systemd 注册！")
	}

	logFn("---- 原始数据保留清单（确认新容器稳定运行后，您可自行决定是否清理）----")
	logFn("   原 Rootfs 目录（未改动）: " + srcCfg.Rootfs)
	if srcCfg.ConfDir != "" && srcCfg.ConfDir != paths.ConfDir {
		logFn("   原配置目录（未改动）:   " + srcCfg.ConfDir)
	}
}
