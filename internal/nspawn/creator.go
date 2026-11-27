package nspawn

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

func (m *Manager) StartCreateTask(req CreateContainerRequest) (*TaskRecord, error) {
	req.Machine = strings.TrimSpace(req.Machine)
	req.Description = strings.TrimSpace(req.Description)
	req.ContainerUser = strings.TrimSpace(req.ContainerUser)
	req.BaseRoot = filepath.Clean(strings.TrimSpace(req.BaseRoot))
	req.PhysicalNIC = strings.TrimSpace(req.PhysicalNIC)
	req.TmpSize = strings.TrimSpace(req.TmpSize)
	req.MemoryMax = strings.TrimSpace(req.MemoryMax)
	req.CPUQuota = strings.TrimSpace(req.CPUQuota)
	req.SourceType = strings.ToLower(strings.TrimSpace(req.SourceType))
	req.SourceArchive = strings.TrimSpace(req.SourceArchive)
	req.Distro = strings.ToLower(strings.TrimSpace(req.Distro))
	req.Suite = strings.TrimSpace(req.Suite)
	req.Arch = strings.TrimSpace(req.Arch)
	req.Mirror = strings.TrimSpace(req.Mirror)
	req.SecurityMirror = strings.TrimSpace(req.SecurityMirror)

	if req.SourceType == "" {
		req.SourceType = "debootstrap"
	}
	if req.SourceType != "debootstrap" && req.SourceType != "archive" {
		return nil, fmt.Errorf("不支持的创建来源类型: %s", req.SourceType)
	}

	if !reMachineName.MatchString(req.Machine) {
		return nil, fmt.Errorf("容器名格式不正确: %s", req.Machine)
	}
	veth := "ve-" + req.Machine
	if len(veth) > 15 {
		return nil, fmt.Errorf("veth 名称过长 (%s 超过 15 个字符)，请将容器名控制在 12 个字符以内", veth)
	}
	if req.ContainerUser == "" {
		req.ContainerUser = "admin"
	}
	if !reUserName.MatchString(req.ContainerUser) {
		return nil, fmt.Errorf("容器用户名格式不正确: %s", req.ContainerUser)
	}
	if req.SourceType == "debootstrap" && req.Password == "" {
		return nil, fmt.Errorf("在线构建容器时，容器用户密码不能为空")
	}
	if req.SourceType == "archive" {
		if req.SourceArchive == "" || !filepath.IsAbs(req.SourceArchive) {
			return nil, fmt.Errorf("请指定本地 rootfs 镜像文件的绝对路径")
		}
		st, err := os.Stat(req.SourceArchive)
		if err != nil || st.IsDir() {
			return nil, fmt.Errorf("镜像文件不存在或不是文件: %s", req.SourceArchive)
		}
	}
	// Always store newly created containers in the configured StandardBaseRoot for layout consistency.
	stdBase := filepath.Clean(strings.TrimSpace(m.cfgStore.Get().StandardBaseRoot))
	if stdBase == "" || stdBase == "." {
		stdBase = "/vol1/nspawn"
	}
	req.BaseRoot = stdBase
	if !filepath.IsAbs(req.BaseRoot) {
		return nil, fmt.Errorf("存储根目录必须是绝对路径: %s", req.BaseRoot)
	}
	if req.PhysicalNIC == "" {
		req.PhysicalNIC = m.cfgStore.Get().DefaultPhysicalNIC
	}
	if req.PhysicalNIC == "" {
		req.PhysicalNIC = m.PreferredOVSNIC()
	}
	if req.PhysicalNIC == "" {
		return nil, fmt.Errorf("未检测到已开启 OVS 的物理网卡，请先在飞牛「系统设置 → 网络设置」中为物理网卡开启 OVS")
	}
	if req.TmpSize == "" {
		req.TmpSize = "4G"
	}
	if req.Distro == "" {
		req.Distro = "debian"
	}
	if req.Suite == "" {
		if req.Distro == "ubuntu" {
			req.Suite = "noble"
		} else {
			req.Suite = "trixie"
		}
	}
	if req.Arch == "" {
		if runtime.GOARCH == "arm64" {
			req.Arch = "arm64"
		} else {
			req.Arch = "amd64"
		}
	}
	if req.Mirror == "" {
		if req.Distro == "ubuntu" {
			req.Mirror = "https://mirrors.ustc.edu.cn/ubuntu"
		} else {
			req.Mirror = "https://mirrors.ustc.edu.cn/debian"
		}
	}
	if req.SecurityMirror == "" {
		if req.Distro == "ubuntu" {
			req.SecurityMirror = "https://mirrors.ustc.edu.cn/ubuntu"
		} else {
			req.SecurityMirror = "https://mirrors.ustc.edu.cn/debian-security"
		}
	}

	paths := BuildStandardPaths(req.BaseRoot, req.Machine)
	if _, err := os.Stat(paths.Rootfs); err == nil {
		return nil, fmt.Errorf("rootfs 已存在，为避免覆盖现有容器已中止: %s", paths.Rootfs)
	}
	if _, err := os.Stat(paths.ConfigFile); err == nil {
		return nil, fmt.Errorf("容器配置文件已存在，为避免覆盖已中止（请改用“从配置恢复”）: %s", paths.ConfigFile)
	}

	if err := exec.Command("ip", "link", "show", req.PhysicalNIC).Run(); err != nil {
		return nil, fmt.Errorf("找不到物理网卡: %s", req.PhysicalNIC)
	}
	brOut, err := exec.Command("ovs-vsctl", "port-to-br", req.PhysicalNIC).Output()
	if err != nil || strings.TrimSpace(string(brOut)) == "" {
		return nil, fmt.Errorf("物理网卡 %s 当前未加入任何 OVS Bridge，请先在 fnOS 网络设置启用 OVS", req.PhysicalNIC)
	}

	for _, b := range req.BindMounts {
		if !filepath.IsAbs(b.HostPath) || !filepath.IsAbs(b.ContainerPath) {
			return nil, fmt.Errorf("Bind Mount 必须使用绝对路径: %s -> %s", b.HostPath, b.ContainerPath)
		}
		st, err := os.Stat(b.HostPath)
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("宿主机源目录不存在: %s", b.HostPath)
		}
	}

	_ = m.cfgStore.AddBaseRoot(req.BaseRoot)

	task := m.tasks.Create("create", req.Machine)
	go m.runCreateTask(task.ID, req, paths, veth)
	return task, nil
}

func buildDebootstrapArgs(distro, suite, arch, rootfs, mirror string, suiteScriptExists, fallbackScriptExists, noCheckGPG bool) ([]string, string) {
	args := []string{
		"--arch=" + arch,
		"--include=ca-certificates",
	}
	if noCheckGPG {
		args = append(args, "--no-check-gpg")
	}
	args = append(args, suite, rootfs, mirror)

	fallbackName := "sid"
	if strings.EqualFold(distro, "ubuntu") {
		fallbackName = "gutsy"
	}
	fallbackScript := "/usr/share/debootstrap/scripts/" + fallbackName
	if !suiteScriptExists && fallbackScriptExists {
		args = append(args, fallbackScript)
		return args, fallbackScript
	}
	return args, ""
}

func (m *Manager) runCreateTask(taskID string, req CreateContainerRequest, paths StandardPaths, veth string) {
	var taskErr error
	rootfsCreated := false
	var mountedPaths []string

	cleanupMounts := func() {
		for i := len(mountedPaths) - 1; i >= 0; i-- {
			p := mountedPaths[i]
			if exec.Command("mountpoint", "-q", p).Run() == nil {
				if exec.Command("umount", p).Run() != nil {
					_ = exec.Command("umount", "-l", p).Run()
				}
			}
		}
		mountedPaths = nil
	}

	defer func() {
		cleanupMounts()
		if taskErr != nil && rootfsCreated {
			if _, statErr := os.Stat(filepath.Join(paths.Rootfs, "etc")); os.IsNotExist(statErr) {
				_ = exec.Command("btrfs", "subvolume", "delete", paths.Rootfs).Run()
				_ = os.RemoveAll(paths.ContainerDir)
				m.tasks.AppendLog(taskID, fmt.Sprintf("[INFO] 构建在写入系统前中止，已自动清理空的残留目录: %s（可直接同名重试）", paths.ContainerDir))
			} else {
				m.tasks.AppendLog(taskID, fmt.Sprintf("[INFO] 已保留未完成的 rootfs: %s（确认无用后可手动执行 btrfs subvolume delete）", paths.Rootfs))
			}
		}
		m.tasks.Finish(taskID, taskErr)
	}()

	logFn := func(msg string) {
		m.tasks.AppendLog(taskID, msg)
	}

	// 1. Ensure host dependencies
	logFn("1. 检查并准备宿主机依赖 (systemd-container, btrfs-progs" + func() string {
		if req.SourceType == "debootstrap" {
			return ", debootstrap"
		}
		return ""
	}() + ")")

	requiredBins := []string{"systemd-nspawn", "btrfs"}
	if req.SourceType == "debootstrap" {
		requiredBins = append(requiredBins, "debootstrap")
	}
	needInstall := false
	for _, bin := range requiredBins {
		if _, err := exec.LookPath(bin); err != nil {
			needInstall = true
			break
		}
	}
	if needInstall {
		if err := runStreamCmd(logFn, nil, "apt-get", "update"); err != nil {
			taskErr = fmt.Errorf("apt-get update 失败: %v", err)
			return
		}
		if err := runStreamCmd(logFn, []string{"DEBIAN_FRONTEND=noninteractive"},
			"apt-get", "install", "-y", "systemd-container", "debootstrap", "btrfs-progs", "ca-certificates"); err != nil {
			taskErr = fmt.Errorf("安装宿主机依赖失败: %v", err)
			return
		}
	} else {
		logFn("[OK] 宿主机依赖均已就绪")
	}

	// 2. Create standardized self-contained container directory tree
	logFn("2. 初始化标准化容器目录结构: " + paths.ContainerDir)
	logFn("   ├── rootfs/           (Btrfs 子卷)")
	logFn("   ├── config/           (container.json + systemd unit + 网络/恢复脚本)")
	logFn("   └── images/           (snapshots/ + exports/)")

	for _, dir := range []string{paths.ContainerDir, paths.ConfDir, paths.ImagesDir, paths.SnapshotDir, paths.ExportsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			taskErr = err
			return
		}
	}
	if !IsBtrfsPath(paths.ContainerDir) {
		taskErr = fmt.Errorf("目录 %s 不在 Btrfs 文件系统上，无法创建 Btrfs subvolume", paths.ContainerDir)
		return
	}
	if err := runStreamCmd(logFn, nil, "btrfs", "subvolume", "create", paths.Rootfs); err != nil {
		taskErr = fmt.Errorf("创建 Btrfs subvolume 失败: %v", err)
		return
	}
	rootfsCreated = true

	if req.SourceType == "archive" {
		// 3a. Extract rootfs archive
		logFn("3. 从本地镜像归档解压 rootfs: " + req.SourceArchive)
		lower := strings.ToLower(req.SourceArchive)
		var tarArgs []string
		if strings.HasSuffix(lower, ".tar.zst") || strings.HasSuffix(lower, ".zst") {
			if _, err := exec.LookPath("zstd"); err != nil {
				taskErr = fmt.Errorf("解压 .tar.zst 需要宿主机安装 zstd")
				return
			}
			tarArgs = []string{"-I", "zstd", "--numeric-owner", "-xpf", req.SourceArchive, "-C", paths.Rootfs}
		} else {
			tarArgs = []string{"--numeric-owner", "-xpaf", req.SourceArchive, "-C", paths.Rootfs}
		}
		if err := runStreamCmd(logFn, nil, "tar", tarArgs...); err != nil {
			taskErr = fmt.Errorf("解压镜像归档失败: %v", err)
			return
		}

		logFn("4. 更新容器主机名与基础网络配置")
		_ = os.WriteFile(filepath.Join(paths.Rootfs, "etc/hostname"), []byte(req.Machine+"\n"), 0644)
		hostsContent := fmt.Sprintf("127.0.0.1 localhost\n127.0.1.1 %s\n::1 localhost ip6-localhost ip6-loopback\n", req.Machine)
		_ = os.WriteFile(filepath.Join(paths.Rootfs, "etc/hosts"), []byte(hostsContent), 0644)

		netDir := filepath.Join(paths.Rootfs, "etc/systemd/network")
		_ = os.MkdirAll(netDir, 0755)
		netCfgFile := filepath.Join(netDir, "20-host0.network")
		if _, err := os.Stat(netCfgFile); os.IsNotExist(err) {
			netCfg := "[Match]\nName=host0\n\n[Network]\nDHCP=yes\nIPv6AcceptRA=yes\n"
			_ = os.WriteFile(netCfgFile, []byte(netCfg), 0644)
		}
		_ = exec.Command("systemctl", "--root="+paths.Rootfs, "enable", "systemd-networkd.service").Run()

		if req.Password != "" {
			logFn("5. 配置/更新容器用户: " + req.ContainerUser)
			if exec.Command("chroot", paths.Rootfs, "id", req.ContainerUser).Run() != nil {
				_ = runStreamCmd(logFn, nil, "chroot", paths.Rootfs, "useradd", "-m", "-s", "/bin/bash", req.ContainerUser)
			}
			chpasswdCmd := exec.Command("chroot", paths.Rootfs, "chpasswd")
			chpasswdCmd.Stdin = strings.NewReader(fmt.Sprintf("%s:%s\n", req.ContainerUser, req.Password))
			_ = chpasswdCmd.Run()
			_ = exec.Command("chroot", paths.Rootfs, "usermod", "-aG", "sudo", req.ContainerUser).Run()
		}
	} else {
		// 3b. Run debootstrap (compatible with newer Ubuntu/Debian suites on older Debian hosts)
		noCheckGPG := false
		if req.Distro == "ubuntu" {
			const ubuntuKeyring = "/usr/share/keyrings/ubuntu-archive-keyring.gpg"
			if _, err := os.Stat(ubuntuKeyring); err != nil {
				logFn("[INFO] 宿主机缺少 ubuntu-archive-keyring，尝试自动安装...")
				_ = runStreamCmd(logFn, nil, "apt-get", "update")
				if err := runStreamCmd(logFn, []string{"DEBIAN_FRONTEND=noninteractive"},
					"apt-get", "install", "-y", "ubuntu-archive-keyring"); err != nil {
					logFn("[WARN] 安装 ubuntu-archive-keyring 未成功: " + err.Error())
				}
				if _, err := os.Stat(ubuntuKeyring); err != nil {
					noCheckGPG = true
					logFn("[WARN] 未找到 Ubuntu GPG 密钥环，将使用 --no-check-gpg 继续引导 Ubuntu 根文件系统")
				}
			}
		}

		suiteScriptPath := filepath.Join("/usr/share/debootstrap/scripts", req.Suite)
		_, suiteErr := os.Stat(suiteScriptPath)
		fallbackName := "sid"
		if req.Distro == "ubuntu" {
			fallbackName = "gutsy"
		}
		fallbackPath := filepath.Join("/usr/share/debootstrap/scripts", fallbackName)
		_, fallbackErr := os.Stat(fallbackPath)

		debootstrapArgs, usedFallback := buildDebootstrapArgs(
			req.Distro, req.Suite, req.Arch, paths.Rootfs, req.Mirror,
			suiteErr == nil, fallbackErr == nil, noCheckGPG,
		)
		if usedFallback != "" {
			logFn(fmt.Sprintf("[INFO] 宿主机 debootstrap 未收录 %s 脚本，自动使用兼容脚本: %s", req.Suite, usedFallback))
		}

		logFn(fmt.Sprintf("3. 执行 debootstrap (%s/%s %s)...", req.Distro, req.Suite, req.Arch))
		if err := runStreamCmd(logFn, nil, "debootstrap", debootstrapArgs...); err != nil {
			taskErr = fmt.Errorf("debootstrap 失败: %v", err)
			return
		}

		// 4. Write APT sources
		logFn("4. 配置 APT 软件源与 chroot 挂载环境")
		var sources string
		if req.Distro == "ubuntu" {
			sources = fmt.Sprintf(
				"deb %s %s main restricted universe multiverse\n"+
					"deb %s %s-updates main restricted universe multiverse\n"+
					"deb %s %s-security main restricted universe multiverse\n",
				req.Mirror, req.Suite, req.Mirror, req.Suite, req.SecurityMirror, req.Suite,
			)
		} else {
			sources = fmt.Sprintf(
				"deb %s %s main contrib non-free non-free-firmware\n"+
					"deb %s %s-updates main contrib non-free non-free-firmware\n"+
					"deb %s %s-security main contrib non-free non-free-firmware\n",
				req.Mirror, req.Suite, req.Mirror, req.Suite, req.SecurityMirror, req.Suite,
			)
		}
		if err := os.WriteFile(filepath.Join(paths.Rootfs, "etc/apt/sources.list"), []byte(sources), 0644); err != nil {
			taskErr = err
			return
		}

		if resolvBytes, err := os.ReadFile("/etc/resolv.conf"); err == nil {
			_ = os.WriteFile(filepath.Join(paths.Rootfs, "etc/resolv.conf"), resolvBytes, 0644)
		}

		for _, sub := range []string{"proc", "sys", "dev", "dev/pts", "run"} {
			_ = os.MkdirAll(filepath.Join(paths.Rootfs, sub), 0755)
		}
		mountStep := func(target string, args ...string) error {
			if exec.Command("mountpoint", "-q", target).Run() == nil {
				return nil
			}
			if err := exec.Command("mount", args...).Run(); err != nil {
				return err
			}
			mountedPaths = append(mountedPaths, target)
			return nil
		}
		if err := mountStep(filepath.Join(paths.Rootfs, "proc"), "-t", "proc", "proc", filepath.Join(paths.Rootfs, "proc")); err != nil {
			taskErr = err
			return
		}
		if err := mountStep(filepath.Join(paths.Rootfs, "sys"), "--rbind", "/sys", filepath.Join(paths.Rootfs, "sys")); err != nil {
			taskErr = err
			return
		}
		_ = exec.Command("mount", "--make-rslave", filepath.Join(paths.Rootfs, "sys")).Run()
		if err := mountStep(filepath.Join(paths.Rootfs, "dev"), "--rbind", "/dev", filepath.Join(paths.Rootfs, "dev")); err != nil {
			taskErr = err
			return
		}
		_ = exec.Command("mount", "--make-rslave", filepath.Join(paths.Rootfs, "dev")).Run()
		_ = mountStep(filepath.Join(paths.Rootfs, "dev/pts"), "--bind", "/dev/pts", filepath.Join(paths.Rootfs, "dev/pts"))
		if err := mountStep(filepath.Join(paths.Rootfs, "run"), "--rbind", "/run", filepath.Join(paths.Rootfs, "run")); err != nil {
			taskErr = err
			return
		}
		_ = exec.Command("mount", "--make-rslave", filepath.Join(paths.Rootfs, "run")).Run()

		policyRc := filepath.Join(paths.Rootfs, "usr/sbin/policy-rc.d")
		_ = os.WriteFile(policyRc, []byte("#!/bin/sh\nexit 101\n"), 0755)

		// 5. Install packages inside container
		logFn("5. 安装容器内基础包 (systemd, networkd, resolved, openssh-server...)")
		if err := runStreamCmd(logFn, []string{"DEBIAN_FRONTEND=noninteractive"},
			"chroot", paths.Rootfs, "apt-get", "update"); err != nil {
			taskErr = fmt.Errorf("容器内 apt-get update 失败: %v", err)
			return
		}

		pkgs := []string{
			"systemd", "systemd-sysv", "systemd-resolved", "dbus", "dbus-user-session",
			"sudo", "curl", "wget", "ca-certificates", "gnupg", "nano", "vim-tiny", "less",
			"openssh-client", "openssh-server", "iproute2", "iputils-ping", "dnsutils",
			"procps", "locales", "tzdata", "bash-completion",
		}
		if req.InstallExtras {
			pkgs = append(pkgs, "git", "jq", "unzip", "zip", "rsync", "net-tools", "lsof", "htop", "tree", "tmux", "build-essential", "pkg-config", "python3", "python3-pip", "python3-venv")
		}

		chrootArgs := append([]string{paths.Rootfs, "apt-get", "install", "-y"}, pkgs...)
		if err := runStreamCmd(logFn, []string{"DEBIAN_FRONTEND=noninteractive"}, "chroot", chrootArgs...); err != nil {
			taskErr = fmt.Errorf("安装容器软件失败: %v", err)
			return
		}
		_ = os.Remove(policyRc)

		// 6. Configure hostname, locale, user
		logFn("6. 配置主机名、locale 与 sudo 用户: " + req.ContainerUser)
		_ = os.WriteFile(filepath.Join(paths.Rootfs, "etc/hostname"), []byte(req.Machine+"\n"), 0644)
		hostsContent := fmt.Sprintf("127.0.0.1 localhost\n127.0.1.1 %s\n::1 localhost ip6-localhost ip6-loopback\n", req.Machine)
		_ = os.WriteFile(filepath.Join(paths.Rootfs, "etc/hosts"), []byte(hostsContent), 0644)

		localeGenPath := filepath.Join(paths.Rootfs, "etc/locale.gen")
		if lg, err := os.ReadFile(localeGenPath); err == nil {
			updated := strings.ReplaceAll(string(lg), "# en_US.UTF-8 UTF-8", "en_US.UTF-8 UTF-8")
			_ = os.WriteFile(localeGenPath, []byte(updated), 0644)
		}
		_ = runStreamCmd(logFn, nil, "chroot", paths.Rootfs, "locale-gen")
		_ = os.WriteFile(filepath.Join(paths.Rootfs, "etc/default/locale"), []byte("LANG=en_US.UTF-8\n"), 0644)

		if exec.Command("chroot", paths.Rootfs, "id", req.ContainerUser).Run() != nil {
			if err := runStreamCmd(logFn, nil, "chroot", paths.Rootfs, "useradd", "-m", "-s", "/bin/bash", req.ContainerUser); err != nil {
				taskErr = fmt.Errorf("创建用户失败: %v", err)
				return
			}
		}
		chpasswdCmd := exec.Command("chroot", paths.Rootfs, "chpasswd")
		chpasswdCmd.Stdin = strings.NewReader(fmt.Sprintf("%s:%s\n", req.ContainerUser, req.Password))
		if out, err := chpasswdCmd.CombinedOutput(); err != nil {
			taskErr = fmt.Errorf("设置密码失败: %v (%s)", err, string(out))
			return
		}
		_ = exec.Command("chroot", paths.Rootfs, "usermod", "-aG", "sudo", req.ContainerUser).Run()

		// 7. systemd-networkd / resolved / sshd
		logFn("7. 配置 systemd-networkd (host0 DHCP) 与 SSH 服务")
		_ = os.MkdirAll(filepath.Join(paths.Rootfs, "etc/systemd/network"), 0755)
		netCfg := "[Match]\nName=host0\n\n[Network]\nDHCP=yes\nIPv6AcceptRA=yes\n"
		_ = os.WriteFile(filepath.Join(paths.Rootfs, "etc/systemd/network/20-host0.network"), []byte(netCfg), 0644)
		_ = exec.Command("systemctl", "--root="+paths.Rootfs, "enable", "systemd-networkd.service").Run()
		_ = exec.Command("systemctl", "--root="+paths.Rootfs, "enable", "systemd-resolved.service").Run()

		resolvPath := filepath.Join(paths.Rootfs, "etc/resolv.conf")
		_ = os.Remove(resolvPath)
		_ = os.Symlink("/run/systemd/resolve/stub-resolv.conf", resolvPath)

		_ = runStreamCmd(logFn, nil, "chroot", paths.Rootfs, "ssh-keygen", "-A")
		if keyFiles, err := filepath.Glob(filepath.Join(paths.Rootfs, "etc/ssh/ssh_host_*_key")); err == nil {
			for _, kf := range keyFiles {
				_ = os.Chown(kf, 0, 0)
				_ = os.Chmod(kf, 0600)
			}
		}
		if pubFiles, err := filepath.Glob(filepath.Join(paths.Rootfs, "etc/ssh/ssh_host_*_key.pub")); err == nil {
			for _, pf := range pubFiles {
				_ = os.Chown(pf, 0, 0)
				_ = os.Chmod(pf, 0644)
			}
		}
		_ = exec.Command("systemctl", "--root="+paths.Rootfs, "enable", "ssh.service").Run()

		cleanupMounts()
	}

	autostart := true
	if req.Autostart != nil {
		autostart = *req.Autostart
	}

	// 8. Write declarative container.json + OVS scripts + restore.sh + systemd unit
	logFn("8. 持久化格式化配置 container.json 并生成 OVS/systemd 运行文件")
	spec := ContainerSpec{
		SchemaVersion: ContainerSpecSchemaVersion,
		Machine:       req.Machine,
		Description:   req.Description,
		Layout:        DefaultSpecLayout(),
		System: SpecSystem{
			ContainerUser: req.ContainerUser,
			Hostname:      req.Machine,
			Autostart:     autostart,
			ServiceName:   paths.ServiceName,
		},
		Image: SpecImage{
			SourceType:     req.SourceType,
			Distro:         req.Distro,
			Suite:          req.Suite,
			Arch:           req.Arch,
			Mirror:         req.Mirror,
			SecurityMirror: req.SecurityMirror,
			SourceArchive:  req.SourceArchive,
		},
		Network: SpecNetwork{
			Mode:        "ovs-veth",
			PhysicalNIC: req.PhysicalNIC,
			Veth:        veth,
			DHCP:        true,
		},
		Resources: SpecResources{
			TmpSize:   req.TmpSize,
			MemoryMax: req.MemoryMax,
			CPUQuota:  req.CPUQuota,
			TasksMax:  "infinity",
		},
		BindMounts: req.BindMounts,
	}
	if err := WriteContainerSpec(paths.ConfigFile, spec); err != nil {
		taskErr = fmt.Errorf("写入 container.json 失败: %v", err)
		return
	}

	if err := os.WriteFile(filepath.Join(paths.ConfDir, "network-up.sh"), []byte(RenderNetworkUpScript(req.PhysicalNIC, veth)), 0700); err != nil {
		taskErr = err
		return
	}
	if err := os.WriteFile(filepath.Join(paths.ConfDir, "network-down.sh"), []byte(RenderNetworkDownScript(veth)), 0700); err != nil {
		taskErr = err
		return
	}
	if err := os.WriteFile(filepath.Join(paths.ConfDir, "restore.sh"), []byte(RenderRestoreScript(req.Machine, req.PhysicalNIC, paths.Rootfs, paths.ConfDir, paths.ServiceName, autostart)), 0700); err != nil {
		taskErr = err
		return
	}

	unitCfg := *SpecToConfig(spec, paths.ContainerDir, filepath.Join("/etc/systemd/system", paths.ServiceName))
	if err := os.WriteFile(paths.ServicePath, []byte(RenderServiceUnit(unitCfg)), 0644); err != nil {
		taskErr = err
		return
	}
	if out, err := exec.Command("systemd-analyze", "verify", paths.ServicePath).CombinedOutput(); err != nil {
		taskErr = fmt.Errorf("systemd-analyze verify 失败: %s", strings.TrimSpace(string(out)))
		return
	}

	if err := applySystemdAutostart(paths.ServicePath, paths.ServiceName, autostart); err != nil {
		taskErr = err
		return
	}

	if req.AutoStart {
		logFn("9. 启动容器并等待 OVS 与 DHCP IPv4 就绪...")
		if err := runStreamCmd(logFn, nil, "systemctl", "restart", paths.ServiceName); err != nil {
			taskErr = fmt.Errorf("启动容器失败: %v", err)
			return
		}

		var gotIP string
		var gotMAC string
		for i := 0; i < 35; i++ {
			info, err := m.GetContainer(req.Machine)
			if err == nil && len(info.Runtime.IPv4Addrs) > 0 {
				gotIP = strings.Join(info.Runtime.IPv4Addrs, ", ")
				gotMAC = info.Runtime.Host0MAC
				break
			}
			time.Sleep(1 * time.Second)
		}
		if gotIP != "" {
			logFn(fmt.Sprintf("[OK] 容器已就绪! host0 MAC: %s, IPv4: %s", gotMAC, gotIP))
		} else {
			logFn("[WARN] 容器已启动，但 35 秒内尚未检测到 DHCP IPv4，可在面板中继续查看网络状态")
		}
	} else {
		logFn("[OK] 容器创建完成（配置文件已保存至 " + paths.ConfigFile + "）")
	}
}

func runStreamCmd(logFn func(string), extraEnv []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	pipeReader := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		buf := make([]byte, 0, 64*1024)
		sc.Buffer(buf, 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				logFn(line)
			}
		}
	}

	wg.Add(2)
	go pipeReader(stdout)
	go pipeReader(stderr)
	wg.Wait()
	return cmd.Wait()
}
