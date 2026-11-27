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
	req.OnlineSource = strings.ToLower(strings.TrimSpace(req.OnlineSource))
	req.OnlineImageURL = strings.TrimSpace(req.OnlineImageURL)
	req.OnlineImageSHA256 = strings.ToLower(strings.TrimSpace(req.OnlineImageSHA256))
	req.OnlineImageFilename = strings.TrimSpace(req.OnlineImageFilename)
	req.OnlineRelease = strings.TrimSpace(req.OnlineRelease)
	req.Distro = strings.ToLower(strings.TrimSpace(req.Distro))
	req.Suite = strings.TrimSpace(req.Suite)
	req.Arch = strings.TrimSpace(req.Arch)
	req.Mirror = strings.TrimSpace(req.Mirror)
	req.SecurityMirror = strings.TrimSpace(req.SecurityMirror)

	if req.SourceType == "" {
		req.SourceType = "debootstrap"
	}
	if req.SourceType != "debootstrap" && req.SourceType != "archive" && req.SourceType != "online_image" {
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
	if req.SourceType == "online_image" {
		if req.Password == "" {
			return nil, fmt.Errorf("使用在线镜像库创建容器时，容器用户密码不能为空")
		}
		if !IsAllowedOnlineImageURL(req.OnlineImageURL) {
			return nil, fmt.Errorf("请从内置镜像库选择有效的系统镜像（仅支持可信 HTTPS 镜像源）")
		}
		if req.OnlineImageFilename == "" {
			req.OnlineImageFilename = filepath.Base(req.OnlineImageURL)
		}
		if req.Suite == "" && req.OnlineRelease != "" {
			req.Suite = req.OnlineRelease
		}
		if req.Mirror == "" {
			req.Mirror = req.OnlineImageURL
		}
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

func isOpenWrtRootfs(rootfs, distro string) bool {
	if strings.EqualFold(strings.TrimSpace(distro), "openwrt") {
		return true
	}
	if strings.TrimSpace(rootfs) == "" {
		return false
	}
	for _, marker := range []string{"etc/openwrt_release", "etc/openwrt_version"} {
		if _, err := os.Stat(filepath.Join(rootfs, marker)); err == nil {
			return true
		}
	}
	return false
}

func renderOpenWrtHost0NetworkUCI() string {
	return `config interface 'loopback'
	option device 'lo'
	option proto 'static'
	option ipaddr '127.0.0.1'
	option netmask '255.0.0.0'

config interface 'lan'
	option device 'host0'
	option proto 'dhcp'
`
}

func ensureOpenWrtLanDHCPIgnored(raw string) string {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	var out []string
	inLan := false
	foundLan := false
	sawIgnore := false

	isLanHeader := func(trimmed string) bool {
		fields := strings.Fields(trimmed)
		if len(fields) < 3 || fields[0] != "config" || fields[1] != "dhcp" {
			return false
		}
		name := strings.Trim(fields[2], `"'`)
		return name == "lan"
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "config ") {
			if inLan && !sawIgnore {
				out = append(out, "\toption ignore '1'")
			}
			inLan = isLanHeader(trimmed)
			if inLan {
				foundLan = true
				sawIgnore = false
			}
			out = append(out, line)
			continue
		}
		if inLan && strings.HasPrefix(trimmed, "option ") {
			fields := strings.Fields(trimmed)
			if len(fields) >= 2 && strings.Trim(fields[1], `"'`) == "ignore" {
				if !sawIgnore {
					out = append(out, "\toption ignore '1'")
					sawIgnore = true
				}
				continue
			}
		}
		out = append(out, line)
	}
	if inLan && !sawIgnore {
		out = append(out, "\toption ignore '1'")
	}
	if !foundLan {
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
		out = append(out, "config dhcp 'lan'", "\toption interface 'lan'", "\toption ignore '1'")
	}
	res := strings.Join(out, "\n")
	if !strings.HasSuffix(res, "\n") {
		res += "\n"
	}
	return res
}

func configureOpenWrtRootfs(logFn func(string), rootfs string, req CreateContainerRequest) {
	logFn("   检测到 OpenWrt 系统，自动适配 UCI host0 DHCP 并默认禁用 LAN DHCP Server（防止与主路由冲突）")
	_ = os.WriteFile(filepath.Join(rootfs, "etc/hostname"), []byte(req.Machine+"\n"), 0644)

	// 1. Update UCI system hostname if present
	sysCfgPath := filepath.Join(rootfs, "etc/config/system")
	if b, err := os.ReadFile(sysCfgPath); err == nil {
		lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
		for i, l := range lines {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "option hostname") {
				lines[i] = fmt.Sprintf("\toption hostname '%s'", req.Machine)
			}
		}
		_ = os.WriteFile(sysCfgPath, []byte(strings.Join(lines, "\n")), 0644)
	}

	// 2. Configure host0 as DHCP client in /etc/config/network (preserving original as .orig)
	netCfgPath := filepath.Join(rootfs, "etc/config/network")
	_ = os.MkdirAll(filepath.Dir(netCfgPath), 0755)
	origBackup := netCfgPath + ".orig"
	if _, err := os.Stat(origBackup); os.IsNotExist(err) {
		if orig, rErr := os.ReadFile(netCfgPath); rErr == nil {
			_ = os.WriteFile(origBackup, orig, 0644)
		}
	}
	_ = os.WriteFile(netCfgPath, []byte(renderOpenWrtHost0NetworkUCI()), 0644)

	// 3. Disable DHCP Server on LAN by default
	dhcpCfgPath := filepath.Join(rootfs, "etc/config/dhcp")
	var dhcpRaw string
	if b, err := os.ReadFile(dhcpCfgPath); err == nil {
		dhcpRaw = string(b)
	}
	_ = os.WriteFile(dhcpCfgPath, []byte(ensureOpenWrtLanDHCPIgnored(dhcpRaw)), 0644)

	// 4. Set root password (chpasswd or BusyBox passwd)
	if req.Password != "" {
		logFn("   设置 OpenWrt root 登录密码")
		chpasswdCmd := exec.Command("chroot", rootfs, "chpasswd")
		chpasswdCmd.Stdin = strings.NewReader("root:" + req.Password + "\n")
		if chpasswdCmd.Run() != nil {
			passwdCmd := exec.Command("chroot", rootfs, "/bin/passwd", "root")
			passwdCmd.Stdin = strings.NewReader(req.Password + "\n" + req.Password + "\n")
			_ = passwdCmd.Run()
		}
	}
}

func extractRootfsArchive(logFn func(string), archivePath, rootfs string) error {
	lower := strings.ToLower(archivePath)
	var tarArgs []string
	switch {
	case strings.HasSuffix(lower, ".tar.zst") || strings.HasSuffix(lower, ".zst"):
		if _, err := exec.LookPath("zstd"); err != nil {
			return fmt.Errorf("解压 .tar.zst 需要宿主机安装 zstd")
		}
		tarArgs = []string{"-I", "zstd", "--numeric-owner", "-xpf", archivePath, "-C", rootfs}
	case strings.HasSuffix(lower, ".tar.xz") || strings.HasSuffix(lower, ".xz"):
		if _, err := exec.LookPath("xz"); err != nil {
			return fmt.Errorf("解压 .tar.xz 需要宿主机安装 xz-utils")
		}
		tarArgs = []string{"--numeric-owner", "-xJf", archivePath, "-C", rootfs}
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		tarArgs = []string{"--numeric-owner", "-xzf", archivePath, "-C", rootfs}
	default:
		tarArgs = []string{"--numeric-owner", "-xpaf", archivePath, "-C", rootfs}
	}
	if err := runStreamCmd(logFn, nil, "tar", tarArgs...); err != nil {
		return fmt.Errorf("解压镜像归档失败: %v", err)
	}
	return nil
}

func systemdUnitExistsInRootfs(rootfs, unit string) bool {
	for _, base := range []string{"usr/lib/systemd/system", "lib/systemd/system", "etc/systemd/system"} {
		if _, err := os.Stat(filepath.Join(rootfs, base, unit)); err == nil {
			return true
		}
	}
	return false
}

func configureExtractedRootfs(logFn func(string), rootfs string, req CreateContainerRequest) {
	_ = os.WriteFile(filepath.Join(rootfs, "etc/hostname"), []byte(req.Machine+"\n"), 0644)
	hostsContent := fmt.Sprintf("127.0.0.1 localhost\n127.0.1.1 %s\n::1 localhost ip6-localhost ip6-loopback\n", req.Machine)
	_ = os.WriteFile(filepath.Join(rootfs, "etc/hosts"), []byte(hostsContent), 0644)

	// 1. Cross-distro host0 DHCP configuration
	netDir := filepath.Join(rootfs, "etc/systemd/network")
	_ = os.MkdirAll(netDir, 0755)
	netCfgFile := filepath.Join(netDir, "20-host0.network")
	if _, err := os.Stat(netCfgFile); os.IsNotExist(err) {
		netCfg := "[Match]\nName=host0\n\n[Network]\nDHCP=yes\nIPv6AcceptRA=yes\n"
		_ = os.WriteFile(netCfgFile, []byte(netCfg), 0644)
	}

	hasNetworkd := systemdUnitExistsInRootfs(rootfs, "systemd-networkd.service")
	hasResolved := systemdUnitExistsInRootfs(rootfs, "systemd-resolved.service")
	hasNM := systemdUnitExistsInRootfs(rootfs, "NetworkManager.service")

	if hasNetworkd {
		_ = exec.Command("systemctl", "--root="+rootfs, "enable", "systemd-networkd.service").Run()
	} else if hasNM {
		_ = exec.Command("systemctl", "--root="+rootfs, "enable", "NetworkManager.service").Run()
		nmDir := filepath.Join(rootfs, "etc/NetworkManager/system-connections")
		_ = os.MkdirAll(nmDir, 0755)
		nmFile := filepath.Join(nmDir, "host0.nmconnection")
		if _, err := os.Stat(nmFile); os.IsNotExist(err) {
			nmCfg := "[connection]\nid=host0\nuuid=00000000-0000-4000-8000-000000000001\ntype=ethernet\ninterface-name=host0\nautoconnect=true\n\n[ipv4]\nmethod=auto\n\n[ipv6]\nmethod=auto\n"
			_ = os.WriteFile(nmFile, []byte(nmCfg), 0600)
			_ = os.Chmod(nmFile, 0600)
		}
	} else {
		ifacesPath := filepath.Join(rootfs, "etc/network/interfaces")
		if b, err := os.ReadFile(ifacesPath); err == nil && !strings.Contains(string(b), "host0") {
			updated := strings.TrimRight(string(b), "\n") + "\n\nauto host0\niface host0 inet dhcp\n"
			_ = os.WriteFile(ifacesPath, []byte(updated), 0644)
		}
	}

	if hasResolved {
		_ = exec.Command("systemctl", "--root="+rootfs, "enable", "systemd-resolved.service").Run()
		resolvPath := filepath.Join(rootfs, "etc/resolv.conf")
		_ = os.Remove(resolvPath)
		_ = os.Symlink("/run/systemd/resolve/stub-resolv.conf", resolvPath)
	} else {
		var resolvCands []string
		for _, p := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
			if b, err := os.ReadFile(p); err == nil {
				resolvCands = append(resolvCands, string(b))
			}
		}
		resolvContent, _ := buildChrootResolvConf(resolvCands...)
		resolvPath := filepath.Join(rootfs, "etc/resolv.conf")
		_ = os.Remove(resolvPath)
		_ = os.WriteFile(resolvPath, []byte(resolvContent), 0644)
	}

	// 2. Configure user, password, and sudo/wheel privileges
	if req.Password != "" && req.ContainerUser != "" {
		logFn("   配置容器用户与 sudo 权限: " + req.ContainerUser)
		if exec.Command("chroot", rootfs, "id", req.ContainerUser).Run() != nil {
			if exec.Command("chroot", rootfs, "useradd", "-m", "-s", "/bin/bash", req.ContainerUser).Run() != nil {
				_ = exec.Command("chroot", rootfs, "useradd", "-m", "-s", "/bin/sh", req.ContainerUser).Run()
			}
		}
		chpasswdCmd := exec.Command("chroot", rootfs, "chpasswd")
		chpasswdCmd.Stdin = strings.NewReader(fmt.Sprintf("%s:%s\n", req.ContainerUser, req.Password))
		_ = chpasswdCmd.Run()

		if grpBytes, err := os.ReadFile(filepath.Join(rootfs, "etc/group")); err == nil {
			grpText := string(grpBytes)
			for _, g := range []string{"sudo", "wheel"} {
				if strings.Contains(grpText, "\n"+g+":") || strings.HasPrefix(grpText, g+":") {
					_ = exec.Command("chroot", rootfs, "usermod", "-aG", g, req.ContainerUser).Run()
				}
			}
		}
		if st, err := os.Stat(filepath.Join(rootfs, "etc/sudoers.d")); err == nil && st.IsDir() {
			sudoRule := fmt.Sprintf("%s ALL=(ALL:ALL) ALL\n", req.ContainerUser)
			sudoFile := filepath.Join(rootfs, "etc/sudoers.d/90-fn-nspawn-user")
			if err := os.WriteFile(sudoFile, []byte(sudoRule), 0440); err == nil {
				_ = os.Chmod(sudoFile, 0440)
			}
		}
	}

	// 3. SSH host keys & enable ssh/sshd if installed
	if _, err := os.Stat(filepath.Join(rootfs, "usr/bin/ssh-keygen")); err == nil {
		_ = runStreamCmd(logFn, nil, "chroot", rootfs, "ssh-keygen", "-A")
		if keyFiles, err := filepath.Glob(filepath.Join(rootfs, "etc/ssh/ssh_host_*_key")); err == nil {
			for _, kf := range keyFiles {
				_ = os.Chown(kf, 0, 0)
				_ = os.Chmod(kf, 0600)
			}
		}
		if pubFiles, err := filepath.Glob(filepath.Join(rootfs, "etc/ssh/ssh_host_*_key.pub")); err == nil {
			for _, pf := range pubFiles {
				_ = os.Chown(pf, 0, 0)
				_ = os.Chmod(pf, 0644)
			}
		}
	}
	for _, sshUnit := range []string{"ssh.service", "sshd.service"} {
		if systemdUnitExistsInRootfs(rootfs, sshUnit) {
			_ = exec.Command("systemctl", "--root="+rootfs, "enable", sshUnit).Run()
		}
	}
}

func buildChrootResolvConf(rawCandidates ...string) (string, []string) {
	seen := make(map[string]bool)
	var servers []string
	for _, raw := range rawCandidates {
		for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
				continue
			}
			fields := strings.Fields(trimmed)
			if len(fields) < 2 || fields[0] != "nameserver" {
				continue
			}
			ip := strings.TrimSpace(fields[1])
			if ip == "" || strings.HasPrefix(ip, "127.") || ip == "::1" || seen[ip] {
				continue
			}
			seen[ip] = true
			servers = append(servers, ip)
		}
	}
	if len(servers) == 0 {
		servers = []string{"223.5.5.5", "119.29.29.29"}
	}
	var sb strings.Builder
	for _, s := range servers {
		sb.WriteString("nameserver ")
		sb.WriteString(s)
		sb.WriteByte('\n')
	}
	return sb.String(), servers
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
	archiveHint := strings.ToLower(req.SourceArchive)
	if req.SourceType == "online_image" {
		archiveHint = strings.ToLower(req.OnlineImageFilename + " " + req.OnlineImageURL)
	}
	needsXZ := (req.SourceType == "online_image" || req.SourceType == "archive") &&
		(strings.Contains(archiveHint, ".tar.xz") || strings.HasSuffix(strings.TrimSpace(archiveHint), ".xz"))
	needsZstd := (req.SourceType == "online_image" || req.SourceType == "archive") &&
		(strings.Contains(archiveHint, ".tar.zst") || strings.HasSuffix(strings.TrimSpace(archiveHint), ".zst"))

	depLabel := "systemd-container, btrfs-progs"
	if req.SourceType == "debootstrap" {
		depLabel += ", debootstrap"
	}
	if needsXZ {
		depLabel += ", xz-utils"
	}
	if needsZstd {
		depLabel += ", zstd"
	}
	logFn("1. 检查并准备宿主机依赖 (" + depLabel + ")")

	checkBins := [][2]string{
		{"systemd-nspawn", "systemd-container"},
		{"btrfs", "btrfs-progs"},
	}
	if req.SourceType == "debootstrap" {
		checkBins = append(checkBins, [2]string{"debootstrap", "debootstrap"})
	}
	if needsXZ {
		checkBins = append(checkBins, [2]string{"xz", "xz-utils"})
	}
	if needsZstd {
		checkBins = append(checkBins, [2]string{"zstd", "zstd"})
	}

	var installPkgs []string
	seenPkg := make(map[string]bool)
	for _, pair := range checkBins {
		if _, err := exec.LookPath(pair[0]); err != nil && !seenPkg[pair[1]] {
			seenPkg[pair[1]] = true
			installPkgs = append(installPkgs, pair[1])
		}
	}
	if len(installPkgs) > 0 {
		if err := runStreamCmd(logFn, nil, "apt-get", "update"); err != nil {
			taskErr = fmt.Errorf("apt-get update 失败: %v", err)
			return
		}
		installArgs := append([]string{"install", "-y", "ca-certificates"}, installPkgs...)
		if err := runStreamCmd(logFn, []string{"DEBIAN_FRONTEND=noninteractive"}, "apt-get", installArgs...); err != nil {
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

	initMode := ""
	enableTun := false

	if req.SourceType == "online_image" {
		logFn(fmt.Sprintf("3. 拉取在线系统镜像 (%s / %s %s)...", req.Distro, req.Suite, req.Arch))
		archivePath, err := m.DownloadOnlineImage(logFn, req.OnlineImageURL, req.OnlineSource, req.OnlineImageFilename, req.OnlineImageSHA256, req.BaseRoot)
		if err != nil {
			taskErr = err
			return
		}
		req.SourceArchive = archivePath

		logFn("4. 解压系统镜像至 rootfs...")
		if err := extractRootfsArchive(logFn, archivePath, paths.Rootfs); err != nil {
			taskErr = err
			return
		}

		if isOpenWrtRootfs(paths.Rootfs, req.Distro) {
			initMode = InitModeOpenWrt
			enableTun = true
			req.Distro = "openwrt"
			req.ContainerUser = "root"
			logFn("5. 适配 OpenWrt procd 启动与安全旁路由网络")
			configureOpenWrtRootfs(logFn, paths.Rootfs, req)
		} else {
			logFn("5. 适配容器主机名、网络 (host0 DHCP)、用户与 SSH 服务")
			configureExtractedRootfs(logFn, paths.Rootfs, req)
		}
	} else if req.SourceType == "archive" {
		// 3a. Extract rootfs archive
		logFn("3. 从本地镜像归档解压 rootfs: " + req.SourceArchive)
		if err := extractRootfsArchive(logFn, req.SourceArchive, paths.Rootfs); err != nil {
			taskErr = err
			return
		}

		if isOpenWrtRootfs(paths.Rootfs, req.Distro) {
			initMode = InitModeOpenWrt
			enableTun = true
			req.Distro = "openwrt"
			req.ContainerUser = "root"
			logFn("4. 适配 OpenWrt procd 启动与安全旁路由网络")
			configureOpenWrtRootfs(logFn, paths.Rootfs, req)
		} else {
			logFn("4. 更新容器主机名、基础网络配置与用户")
			configureExtractedRootfs(logFn, paths.Rootfs, req)
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

		var resolvCands []string
		for _, p := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
			if b, err := os.ReadFile(p); err == nil {
				resolvCands = append(resolvCands, string(b))
			}
		}
		resolvContent, dnsServers := buildChrootResolvConf(resolvCands...)
		chrootResolv := filepath.Join(paths.Rootfs, "etc/resolv.conf")
		_ = os.Remove(chrootResolv)
		if err := os.WriteFile(chrootResolv, []byte(resolvContent), 0644); err != nil {
			taskErr = fmt.Errorf("写入容器临时 DNS 配置失败: %v", err)
			return
		}
		logFn("[INFO] 构建期 chroot DNS: " + strings.Join(dnsServers, ", "))

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
			"chroot", paths.Rootfs, "apt-get", "-o", "APT::Sandbox::User=root", "-o", "Acquire::Retries=3", "update"); err != nil {
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

		chrootArgs := append([]string{paths.Rootfs, "apt-get", "-o", "APT::Sandbox::User=root", "-o", "Acquire::Retries=3", "install", "-y"}, pkgs...)
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
			InitMode:      initMode,
			EnableTun:     enableTun,
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
