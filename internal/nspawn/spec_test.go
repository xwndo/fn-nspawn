package nspawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fn-nspawn/internal/config"
)

func TestBuildStandardPathsAndSpecRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	baseRoot := filepath.Join(tmp, "nspawn")
	paths := BuildStandardPaths(baseRoot, "devbox")

	if paths.ContainerDir != filepath.Join(baseRoot, "devbox") {
		t.Fatalf("unexpected container dir: %s", paths.ContainerDir)
	}
	if paths.Rootfs != filepath.Join(paths.ContainerDir, "rootfs") {
		t.Fatalf("unexpected rootfs: %s", paths.Rootfs)
	}
	if paths.ConfigFile != filepath.Join(paths.ContainerDir, "config", "container.json") {
		t.Fatalf("unexpected config file: %s", paths.ConfigFile)
	}
	if paths.SnapshotDir != filepath.Join(paths.ContainerDir, "images", "snapshots") {
		t.Fatalf("unexpected snapshot dir: %s", paths.SnapshotDir)
	}
	if paths.ExportsDir != filepath.Join(paths.ContainerDir, "images", "exports") {
		t.Fatalf("unexpected exports dir: %s", paths.ExportsDir)
	}

	spec := ContainerSpec{
		Machine: "devbox",
		System: SpecSystem{
			ContainerUser: "demo",
			Autostart:     false,
		},
		Image: SpecImage{
			SourceType: "debootstrap",
			Distro:     "debian",
			Suite:      "trixie",
			Arch:       "amd64",
		},
		Network: SpecNetwork{
			PhysicalNIC: "eth0",
		},
		Resources: SpecResources{
			TmpSize:   "2G",
			MemoryMax: "4G",
			CPUQuota:  "200%",
		},
		BindMounts: []BindMount{
			{HostPath: "/vol1/data", ContainerPath: "/mnt/data", ReadOnly: true},
		},
	}

	if err := WriteContainerSpec(paths.ConfigFile, spec); err != nil {
		t.Fatalf("WriteContainerSpec failed: %v", err)
	}

	loaded, err := ReadContainerSpec(paths.ConfigFile)
	if err != nil {
		t.Fatalf("ReadContainerSpec failed: %v", err)
	}
	if loaded.Machine != "devbox" || loaded.System.Autostart != false || loaded.Network.PhysicalNIC != "eth0" {
		t.Fatalf("loaded spec mismatch: %+v", loaded)
	}
	if loaded.Layout.SnapshotsDir != "images/snapshots" || loaded.Layout.ExportsDir != "images/exports" {
		t.Fatalf("layout defaults mismatch: %+v", loaded.Layout)
	}

	// Simulate moving/restoring the container folder to a completely different path after OS reinstall
	relocatedDir := filepath.Join(tmp, "vol2", "migrated", "devbox")
	cfg := SpecToConfig(*loaded, relocatedDir, "")
	if cfg.Layout != LayoutStandard {
		t.Fatalf("expected standard layout, got %s", cfg.Layout)
	}
	if cfg.Rootfs != filepath.Join(relocatedDir, "rootfs") {
		t.Fatalf("relocated rootfs mismatch: %s", cfg.Rootfs)
	}
	if cfg.ConfDir != filepath.Join(relocatedDir, "config") {
		t.Fatalf("relocated confDir mismatch: %s", cfg.ConfDir)
	}
	if cfg.SnapshotDir != filepath.Join(relocatedDir, "images", "snapshots") {
		t.Fatalf("relocated snapshotDir mismatch: %s", cfg.SnapshotDir)
	}
	if cfg.ExportsDir != filepath.Join(relocatedDir, "images", "exports") {
		t.Fatalf("relocated exportsDir mismatch: %s", cfg.ExportsDir)
	}
}

func TestParseServiceAndImportPreview(t *testing.T) {
	tmp := t.TempDir()
	etcDir := filepath.Join(tmp, "etc")
	store, err := config.NewStore(etcDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	mgr := NewManager(store, NewTaskManager())

	baseRoot := filepath.Join(tmp, "vol1", "nspawn")
	paths := BuildStandardPaths(baseRoot, "appbox")
	for _, d := range []string{paths.Rootfs, paths.ConfDir, paths.SnapshotDir, paths.ExportsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	spec := ContainerSpec{
		Machine: "appbox",
		System: SpecSystem{
			ContainerUser: "admin",
			Autostart:     true,
			ServiceName:   paths.ServiceName,
		},
		Network: SpecNetwork{
			PhysicalNIC: "enp3s0",
			Veth:        "ve-appbox",
		},
		Resources: SpecResources{
			TmpSize: "4G",
		},
	}
	if err := WriteContainerSpec(paths.ConfigFile, spec); err != nil {
		t.Fatal(err)
	}

	cfg := SpecToConfig(spec, paths.ContainerDir, "")
	unit := RenderServiceUnit(*cfg)
	if err := os.WriteFile(paths.ServicePath, []byte(unit), 0644); err != nil {
		t.Fatal(err)
	}
	restoreSh := RenderRestoreScript(cfg.Machine, cfg.PhysicalNIC, cfg.Rootfs, cfg.ConfDir, cfg.ServiceName, false)
	if !strings.Contains(restoreSh, "AUTOSTART=false") {
		t.Fatalf("expected AUTOSTART=false in restore.sh")
	}
	if !strings.Contains(unit, "# fn-nspawn-source=") || !strings.Contains(unit, "StartLimitIntervalSec=0") || !strings.Contains(unit, "TimeoutStartSec=240") || !strings.Contains(unit, "ExecStartPre=") {
		t.Fatalf("expected fn-nspawn-source header, StartLimitIntervalSec=0, TimeoutStartSec=240 and ExecStartPre in unit:\n%s", unit)
	}
	netUpSh := RenderNetworkUpScript(cfg.PhysicalNIC, cfg.Veth)
	if !strings.Contains(netUpSh, "for _ in {1..60}; do") {
		t.Fatalf("expected OVS retry loop in network-up.sh:\n%s", netUpSh)
	}

	parsed, err := ParseServiceFile(paths.ServicePath, "")
	if err != nil {
		t.Fatalf("ParseServiceFile failed: %v", err)
	}
	if parsed.Layout != LayoutStandard || !parsed.HasConfigFile || !parsed.Autostart {
		t.Fatalf("unexpected parsed config: %+v", parsed)
	}
	if parsed.ExportsDir != paths.ExportsDir {
		t.Fatalf("expected exports dir %s, got %s", paths.ExportsDir, parsed.ExportsDir)
	}

	// Preview import from container.json
	prev1, err := mgr.PreviewImportFromConfig(paths.ConfigFile)
	if err != nil {
		t.Fatalf("PreviewImportFromConfig(file) failed: %v", err)
	}
	if prev1.ContainerDir != paths.ContainerDir || !prev1.RootfsExists || prev1.ConfigType != "container_json" {
		t.Fatalf("unexpected preview1: %+v", prev1)
	}

	// Preview import from container root dir
	prev2, err := mgr.PreviewImportFromConfig(paths.ContainerDir)
	if err != nil {
		t.Fatalf("PreviewImportFromConfig(dir) failed: %v", err)
	}
	if prev2.ResolvedConfigPath != paths.ConfigFile {
		t.Fatalf("expected %s, got %s", paths.ConfigFile, prev2.ResolvedConfigPath)
	}
}

func TestParseIPAddrJSONAndTextFallback(t *testing.T) {
	jsonSample := `[
		{
			"ifname": "lo",
			"address": "00:00:00:00:00:00",
			"addr_info": [{"family": "inet", "local": "127.0.0.1", "scope": "host"}]
		},
		{
			"ifname": "eth0",
			"address": "11:22:33:44:55:66",
			"addr_info": [
				{"family": "inet", "local": "10.0.0.5", "scope": "global"},
				{"family": "inet", "local": "10.0.0.5", "scope": "global"}
			]
		},
		{
			"ifname": "host0",
			"address": "aa:bb:cc:dd:ee:ff",
			"addr_info": [
				{"family": "inet", "local": "192.0.2.50", "scope": "global"},
				{"family": "inet6", "local": "fe80::1", "scope": "link"},
				{"family": "inet6", "local": "2001:db8::10", "scope": "global"}
			]
		}
	]`
	mac, v4s, v6s := parseIPAddrJSON([]byte(jsonSample))
	if mac != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("expected host0 mac aa:bb:cc:dd:ee:ff, got %s", mac)
	}
	if len(v4s) != 2 || v4s[0] != "10.0.0.5" || v4s[1] != "192.0.2.50" {
		t.Fatalf("unexpected ipv4s: %v", v4s)
	}
	if len(v6s) != 1 || v6s[0] != "2001:db8::10" {
		t.Fatalf("unexpected ipv6s: %v", v6s)
	}

	textSample := `1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
2: host0@if12    link/ether aa:bb:cc:dd:ee:01 brd ff:ff:ff:ff:ff:ff
2: host0@if12    inet 192.0.2.88/24 brd 192.0.2.255 scope global dynamic host0\       valid_lft 86000sec preferred_lft 86000sec
2: host0@if12    inet6 fe80::a8bb:ccff:fedd:ee01/64 scope link \       valid_lft forever preferred_lft forever
2: host0@if12    inet6 fd00:10::88/64 scope global \       valid_lft forever preferred_lft forever`
	tMac, tV4, tV6 := parseIPAddrText(textSample)
	if tMac != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("expected text mac aa:bb:cc:dd:ee:01, got %s", tMac)
	}
	if len(tV4) != 1 || tV4[0] != "192.0.2.88" {
		t.Fatalf("unexpected text ipv4: %v", tV4)
	}
	if len(tV6) != 1 || tV6[0] != "fd00:10::88" {
		t.Fatalf("unexpected text ipv6: %v", tV6)
	}
}

func TestParseMachinectlStatusAddresses(t *testing.T) {
	sample := `demo-debian(1234567890abcdef)
           Since: Thu 2026-11-26 11:00:00 CST; 5min ago
          Leader: 12345 (systemd)
         Service: systemd-nspawn; class container
            Root: /vol1/nspawn/demo-debian/rootfs
           Iface: ve-demo-debian
         Address: 192.0.2.100
                  2001:db8::100
                  fe80::1234%host0
              OS: Debian GNU/Linux 13 (trixie)
            Unit: nspawn-demo-debian.service`

	v4s, v6s := parseMachinectlStatusAddresses(sample)
	if len(v4s) != 1 || v4s[0] != "192.0.2.100" {
		t.Fatalf("unexpected machinectl ipv4s: %v", v4s)
	}
	if len(v6s) != 1 || v6s[0] != "2001:db8::100" {
		t.Fatalf("unexpected machinectl ipv6s: %v", v6s)
	}
}

func TestRenderRestoreScriptInstallsRegularUnitOnDisable(t *testing.T) {
	script := RenderRestoreScript("demo-debian", "enp2s0", "/vol1/nspawn/demo-debian/rootfs", "/vol1/nspawn/demo-debian/config", "nspawn-demo-debian.service", false)
	disableIdx := strings.Index(script, `systemctl disable "$NSPAWN_SERVICE_NAME"`)
	wantsIdx := strings.LastIndex(script, `rm -f "/etc/systemd/system/multi-user.target.wants/$NSPAWN_SERVICE_NAME"`)
	installIdx := strings.Index(script, `install -m 644 "$CONFIG/$NSPAWN_SERVICE_NAME" "/etc/systemd/system/$NSPAWN_SERVICE_NAME"`)
	recheckIdx := strings.LastIndex(script, `install -m 644 "$CONFIG/$NSPAWN_SERVICE_NAME" "/etc/systemd/system/$NSPAWN_SERVICE_NAME"`)
	if disableIdx < 0 || wantsIdx < 0 || installIdx < 0 || recheckIdx <= installIdx {
		t.Fatalf("expected regular file install before and guard after disable in restore script:\n%s", script)
	}
	if !(installIdx < disableIdx && disableIdx < wantsIdx && wantsIdx < recheckIdx) {
		t.Fatalf("expected disable/wants cleanup before regular file reinstall guard in restore script")
	}
}

func TestParseCopiedSystemdUnitAndLinkHealthy(t *testing.T) {
	tmp := t.TempDir()
	baseRoot := filepath.Join(tmp, "vol1", "nspawn-std")
	paths := BuildStandardPaths(baseRoot, "demo-debian")
	for _, d := range []string{paths.Rootfs, paths.ConfDir, paths.SnapshotDir, paths.ExportsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	spec := ContainerSpec{
		Machine: "demo-debian",
		System: SpecSystem{
			Autostart:   true,
			ServiceName: paths.ServiceName,
		},
		Network: SpecNetwork{
			PhysicalNIC: "enp2s0",
			Veth:        "ve-demo-debian",
		},
	}
	if err := WriteContainerSpec(paths.ConfigFile, spec); err != nil {
		t.Fatal(err)
	}
	cfg := SpecToConfig(spec, paths.ContainerDir, "")
	cfg.ServicePath = paths.ServicePath
	unitBytes := []byte(RenderServiceUnit(*cfg))
	if err := os.WriteFile(paths.ServicePath, unitBytes, 0644); err != nil {
		t.Fatal(err)
	}

	sysUnit := filepath.Join(tmp, "etc", "systemd", "system", paths.ServiceName)
	if err := os.MkdirAll(filepath.Dir(sysUnit), 0755); err != nil {
		t.Fatal(err)
	}

	// Missing unit -> not synced
	if isSystemdUnitSynced(sysUnit, paths.ServicePath) {
		t.Fatalf("expected LinkHealthy=false when system unit is missing")
	}

	// Regular copied unit with matching bytes -> synced, recovers ConfDir/ServicePath via header
	if err := os.WriteFile(sysUnit, unitBytes, 0644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseServiceFile(sysUnit, sysUnit)
	if err != nil {
		t.Fatalf("ParseServiceFile(sysUnit) failed: %v", err)
	}
	if !parsed.LinkHealthy {
		t.Fatalf("expected LinkHealthy=true for matching regular copy")
	}
	if filepath.Clean(parsed.ServicePath) != filepath.Clean(paths.ServicePath) {
		t.Fatalf("expected ServicePath %s, got %s", paths.ServicePath, parsed.ServicePath)
	}
	if filepath.Clean(parsed.ConfDir) != filepath.Clean(paths.ConfDir) {
		t.Fatalf("expected ConfDir %s, got %s", paths.ConfDir, parsed.ConfDir)
	}

	// Modified copy -> not synced
	if err := os.WriteFile(sysUnit, append(unitBytes, []byte("# drift\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	if isSystemdUnitSynced(sysUnit, paths.ServicePath) {
		t.Fatalf("expected LinkHealthy=false when regular copy differs from source")
	}

	// Symlink (legacy layout) -> never considered synced (requires upgrade to regular file)
	_ = os.Remove(sysUnit)
	if err := os.Symlink(paths.ServicePath, sysUnit); err == nil {
		if isSystemdUnitSynced(sysUnit, paths.ServicePath) {
			t.Fatalf("expected LinkHealthy=false for symlink to storage pool")
		}
	}
}

func TestDiscoverContainersStandardOverridesLegacy(t *testing.T) {
	tmp := t.TempDir()
	etcDir := filepath.Join(tmp, "etc")
	store, err := config.NewStore(etcDir)
	if err != nil {
		t.Fatal(err)
	}

	legacyBase := filepath.Join(tmp, "nspawn")
	stdBase := filepath.Join(tmp, "nspawn-std")
	if err := store.Save(config.AppConfig{
		BaseRoots: []string{legacyBase, stdBase},
	}); err != nil {
		t.Fatal(err)
	}

	// Legacy layout in BaseRoots[0]
	legacyConf := filepath.Join(legacyBase, "demo-debian-conf")
	legacyRootfs := filepath.Join(legacyBase, "demo-debian", "rootfs")
	if err := os.MkdirAll(legacyConf, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacyRootfs, 0755); err != nil {
		t.Fatal(err)
	}
	legacyUnit := RenderServiceUnit(ContainerConfig{
		Machine: "demo-debian",
		Rootfs:  legacyRootfs,
		ConfDir: legacyConf,
	})
	if err := os.WriteFile(filepath.Join(legacyConf, "nspawn-demo-debian.service"), []byte(legacyUnit), 0644); err != nil {
		t.Fatal(err)
	}

	// Standard layout in BaseRoots[1]
	stdPaths := BuildStandardPaths(stdBase, "demo-debian")
	for _, d := range []string{stdPaths.Rootfs, stdPaths.ConfDir, stdPaths.SnapshotDir, stdPaths.ExportsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteContainerSpec(stdPaths.ConfigFile, ContainerSpec{
		Machine: "demo-debian",
		System:  SpecSystem{ServiceName: stdPaths.ServiceName},
	}); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(store, NewTaskManager())
	list, err := mgr.DiscoverContainers()
	if err != nil {
		t.Fatal(err)
	}
	var matched []ContainerInfo
	for _, item := range list {
		if item.Config.Machine == "demo-debian" {
			matched = append(matched, item)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("expected 1 demo-debian container, got %d", len(matched))
	}
	if matched[0].Config.Layout != LayoutStandard {
		t.Fatalf("expected standard layout to override legacy, got %s", matched[0].Config.Layout)
	}
	if matched[0].Config.ContainerDir != stdPaths.ContainerDir {
		t.Fatalf("expected %s, got %s", stdPaths.ContainerDir, matched[0].Config.ContainerDir)
	}
}

func TestParseMemTotalKBAndByteSize(t *testing.T) {
	meminfo := "MemTotal:       16384000 kB\nMemFree:         2000000 kB\n"
	if got := parseMemTotalKB(meminfo); got != 16384000*1024 {
		t.Fatalf("expected %d, got %d", uint64(16384000*1024), got)
	}
	if got := parseMemTotalKB("MemFree: 100 kB\n"); got != 0 {
		t.Fatalf("expected 0 when MemTotal missing, got %d", got)
	}

	cases := []struct {
		in   string
		want uint64
	}{
		{"4G", 4 * 1024 * 1024 * 1024},
		{"512M", 512 * 1024 * 1024},
		{"1.5G", uint64(1.5 * 1024 * 1024 * 1024)},
		{"2048K", 2048 * 1024},
		{"1GiB", 1024 * 1024 * 1024},
		{"infinity", 0},
		{"", 0},
		{"50%", 0},
	}
	for _, tc := range cases {
		if got := parseByteSize(tc.in); got != tc.want {
			t.Fatalf("parseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseMachinectlListAndEnrichUnit(t *testing.T) {
	sample := `demo-debian container systemd-nspawn debian 13
ext-ubuntu  container systemd-nspawn ubuntu 24.04
docker-vm   vm        qemu           linux  -
helper      container lxc            alpine 3.19
ext-ubuntu  container systemd-nspawn ubuntu 24.04
`
	got := parseMachinectlListOutput(sample)
	if len(got) != 2 || got[0] != "demo-debian" || got[1] != "ext-ubuntu" {
		t.Fatalf("unexpected parsed machines: %v", got)
	}

	unit := `[Unit]
Description=External Container %i

[Service]
ExecStart=systemd-nspawn --quiet --keep-unit --boot -D /var/lib/machines/%i --bind=/vol1/share:/mnt/share --bind-ro=/vol1/iso:/mnt/iso --tmpfs=/tmp:mode=1777,size=2G
MemoryMax=6G
CPUQuota=300%
TasksMax=4096
`
	cfg := ContainerConfig{
		Machine: "ext-ubuntu",
		Rootfs:  "/var/lib/machines/ext-ubuntu",
		Layout:  LayoutExternal,
	}
	enrichConfigFromUnitText(unit, &cfg)
	if cfg.Machine != "ext-ubuntu" {
		t.Fatalf("Machine should not be overwritten by %%i template, got %s", cfg.Machine)
	}
	if cfg.Rootfs != "/var/lib/machines/ext-ubuntu" {
		t.Fatalf("Rootfs should stay /var/lib/machines/ext-ubuntu, got %s", cfg.Rootfs)
	}
	if cfg.TmpSize != "2G" || cfg.MemoryMax != "6G" || cfg.CPUQuota != "300%" || cfg.TasksMax != "4096" {
		t.Fatalf("unexpected enriched resources: %+v", cfg)
	}
	if len(cfg.BindMounts) != 2 || cfg.BindMounts[0].HostPath != "/vol1/share" || !cfg.BindMounts[1].ReadOnly {
		t.Fatalf("unexpected enriched bind mounts: %+v", cfg.BindMounts)
	}
}

func TestStandardBaseRootScanAndMigrateFlags(t *testing.T) {
	tmp := t.TempDir()
	etcDir := filepath.Join(tmp, "etc")
	store, err := config.NewStore(etcDir)
	if err != nil {
		t.Fatal(err)
	}

	stdBase := filepath.Join(tmp, "nspawn-std")
	extraBase := filepath.Join(tmp, "nspawn-extra")
	if err := store.Save(config.AppConfig{
		StandardBaseRoot: stdBase,
		BaseRoots:        []string{extraBase, stdBase},
	}); err != nil {
		t.Fatal(err)
	}

	roots := effectiveScanRoots(store.Get())
	if len(roots) != 2 || filepath.Clean(roots[0]) != filepath.Clean(stdBase) || filepath.Clean(roots[1]) != filepath.Clean(extraBase) {
		t.Fatalf("unexpected effectiveScanRoots: %v", roots)
	}

	// Container inside StandardBaseRoot
	inStd := BuildStandardPaths(stdBase, "stdbox")
	for _, d := range []string{inStd.Rootfs, inStd.ConfDir, inStd.SnapshotDir, inStd.ExportsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteContainerSpec(inStd.ConfigFile, ContainerSpec{
		Machine: "stdbox",
		System:  SpecSystem{ServiceName: inStd.ServiceName},
	}); err != nil {
		t.Fatal(err)
	}

	// Container inside supplementary extraBase (standardized dir but outside StandardBaseRoot)
	inExtra := BuildStandardPaths(extraBase, "extrabox")
	for _, d := range []string{inExtra.Rootfs, inExtra.ConfDir, inExtra.SnapshotDir, inExtra.ExportsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteContainerSpec(inExtra.ConfigFile, ContainerSpec{
		Machine: "extrabox",
		System:  SpecSystem{ServiceName: inExtra.ServiceName},
	}); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(store, NewTaskManager())
	list, err := mgr.DiscoverContainers()
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]ContainerInfo)
	for _, item := range list {
		byName[item.Config.Machine] = item
	}
	if c, ok := byName["stdbox"]; !ok || !c.Config.InStandardRoot || c.Config.CanMigrateToStandard {
		t.Fatalf("expected stdbox InStandardRoot=true & CanMigrateToStandard=false, got %+v", c.Config)
	}
	if c, ok := byName["extrabox"]; !ok || c.Config.InStandardRoot || !c.Config.CanMigrateToStandard {
		t.Fatalf("expected extrabox InStandardRoot=false & CanMigrateToStandard=true, got %+v", c.Config)
	}
}

func TestDefaultAppConfigHasNoHardcodedNIC(t *testing.T) {
	def := config.DefaultAppConfig()
	if def.DefaultPhysicalNIC != "" {
		t.Fatalf("expected empty DefaultPhysicalNIC by default, got %q", def.DefaultPhysicalNIC)
	}
	tmp := t.TempDir()
	store, err := config.NewStore(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config.AppConfig{StandardBaseRoot: "/vol1/nspawn"}); err != nil {
		t.Fatal(err)
	}
	if got := store.Get().DefaultPhysicalNIC; got != "" {
		t.Fatalf("expected empty DefaultPhysicalNIC after Save when unset, got %q", got)
	}
}

func TestBuildDebootstrapArgs(t *testing.T) {
	args, fb := buildDebootstrapArgs("debian", "bookworm", "amd64", "/vol1/nspawn/box/rootfs", "https://mirrors.ustc.edu.cn/debian", true, true, false)
	if fb != "" {
		t.Fatalf("expected no fallback script when suite script exists, got %q", fb)
	}
	want := []string{"--arch=amd64", "--include=ca-certificates", "bookworm", "/vol1/nspawn/box/rootfs", "https://mirrors.ustc.edu.cn/debian"}
	if len(args) != len(want) {
		t.Fatalf("unexpected args length: got %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q (full: %v)", i, args[i], want[i], args)
		}
	}

	argsUbuntu, fbUbuntu := buildDebootstrapArgs("ubuntu", "noble", "amd64", "/vol1/nspawn/ubox/rootfs", "https://mirrors.ustc.edu.cn/ubuntu", false, true, true)
	if fbUbuntu != "/usr/share/debootstrap/scripts/gutsy" {
		t.Fatalf("expected gutsy fallback script, got %q", fbUbuntu)
	}
	wantUbuntu := []string{"--arch=amd64", "--include=ca-certificates", "--no-check-gpg", "noble", "/vol1/nspawn/ubox/rootfs", "https://mirrors.ustc.edu.cn/ubuntu", "/usr/share/debootstrap/scripts/gutsy"}
	if len(argsUbuntu) != len(wantUbuntu) {
		t.Fatalf("unexpected ubuntu args length: got %v, want %v", argsUbuntu, wantUbuntu)
	}
	for i := range wantUbuntu {
		if argsUbuntu[i] != wantUbuntu[i] {
			t.Fatalf("ubuntu args[%d] = %q, want %q (full: %v)", i, argsUbuntu[i], wantUbuntu[i], argsUbuntu)
		}
	}

	argsDeb, fbDeb := buildDebootstrapArgs("debian", "trixie", "arm64", "/vol1/nspawn/tbox/rootfs", "https://mirrors.ustc.edu.cn/debian", false, true, false)
	if fbDeb != "/usr/share/debootstrap/scripts/sid" {
		t.Fatalf("expected sid fallback script, got %q", fbDeb)
	}
	if argsDeb[len(argsDeb)-1] != "/usr/share/debootstrap/scripts/sid" {
		t.Fatalf("expected trailing sid script, got %v", argsDeb)
	}
}

func TestBuildChrootResolvConf(t *testing.T) {
	stubOnly := "# systemd-resolved stub\nnameserver 127.0.0.53\nnameserver ::1\n"
	content, servers := buildChrootResolvConf(stubOnly, "")
	if len(servers) != 2 || servers[0] != "223.5.5.5" || servers[1] != "119.29.29.29" {
		t.Fatalf("expected fallback DNS [223.5.5.5 119.29.29.29], got %v", servers)
	}
	if !strings.Contains(content, "nameserver 223.5.5.5\n") {
		t.Fatalf("unexpected fallback resolv.conf content: %q", content)
	}

	upstream := "nameserver 192.168.1.1\nnameserver 223.5.5.5\n"
	content2, servers2 := buildChrootResolvConf(stubOnly, upstream, "nameserver 192.168.1.1\n")
	if len(servers2) != 2 || servers2[0] != "192.168.1.1" || servers2[1] != "223.5.5.5" {
		t.Fatalf("expected deduplicated upstream DNS [192.168.1.1 223.5.5.5], got %v", servers2)
	}
	if strings.Contains(content2, "127.0.0.53") || strings.Contains(content2, "::1") {
		t.Fatalf("loopback nameserver leaked into chroot resolv.conf: %q", content2)
	}
}

func TestParseSimpleStreamsCatalog(t *testing.T) {
	raw := []byte(`{
		"products": {
			"debian:bookworm:amd64:default": {
				"os": "Debian",
				"release": "bookworm",
				"release_title": "bookworm",
				"arch": "amd64",
				"variant": "default",
				"versions": {
					"20261117_05:24": { "items": { "root.tar.xz": { "path": "images/debian/bookworm/amd64/default/20261117_05:24/rootfs.tar.xz", "size": 90000000, "sha256": "aaa111" } } },
					"20261118_05:24": { "items": { "root.tar.xz": { "path": "images/debian/bookworm/amd64/default/20261118_05:24/rootfs.tar.xz", "size": 98728488, "sha256": "BBB222" } } }
				}
			},
			"archlinux:current:amd64:cloud": {
				"os": "Archlinux",
				"release": "current",
				"arch": "amd64",
				"variant": "cloud",
				"versions": {
					"20261118_04:18": { "items": { "root.tar.xz": { "path": "images/archlinux/current/amd64/cloud/20261118_04:18/rootfs.tar.xz", "size": 123, "sha256": "ccc" } } }
				}
			},
			"alpine:edge:amd64:default": {
				"os": "Alpine",
				"release": "edge",
				"arch": "amd64",
				"variant": "default",
				"versions": {
					"20261118_01:00": { "items": { "root.tar.xz": { "path": "images/alpine/edge/amd64/default/20261118_01:00/rootfs.tar.xz", "size": 123, "sha256": "ddd" } } }
				}
			},
			"fedora:43:arm64:default": {
				"os": "Fedora",
				"release": "43",
				"arch": "arm64",
				"variant": "default",
				"versions": {
					"20261117_20:33": { "items": { "root.tar.xz": { "path": "images/fedora/43/arm64/default/20261117_20:33/rootfs.tar.xz", "size": 150000000, "sha256": "eee" } } }
				}
			}
		}
	}`)
	items, err := parseSimpleStreamsCatalog(raw, "tuna", "https://mirrors.tuna.tsinghua.edu.cn/lxc-images/", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 amd64 default systemd item, got %d: %+v", len(items), items)
	}
	it := items[0]
	if it.Distro != "debian" || it.Release != "bookworm" || it.BuildVersion != "20261118_05:24" || it.SHA256 != "bbb222" {
		t.Fatalf("unexpected parsed item: %+v", it)
	}
	wantURL := "https://mirrors.tuna.tsinghua.edu.cn/lxc-images/images/debian/bookworm/amd64/default/20261118_05:24/rootfs.tar.xz"
	if it.URL != wantURL {
		t.Fatalf("unexpected URL: got %s, want %s", it.URL, wantURL)
	}
}

func TestParseProxmoxIndexCatalog(t *testing.T) {
	html := `
<a href="/proxmox/images/system/alpine-3.24-default_20261105_amd64.tar.xz">alpine</a>
<a href="/proxmox/images/system/gentoo-current-openrc_20250508_amd64.tar.xz">gentoo</a>
<a href="/proxmox/images/system/debian-13-standard_13.1-2_amd64.tar.zst">deb13-old</a>
<a href="/proxmox/images/system/debian-13-standard_13.6-1_amd64.tar.zst">deb13-new</a>
<a href="/proxmox/images/system/debian-13-standard_13.6-1_arm64.tar.zst">deb13-arm</a>
<a href="/proxmox/images/system/rockylinux-9-default_20240912_amd64.tar.xz">rocky9</a>
<a href="/proxmox/images/system/archlinux-base_20260420-1_amd64.tar.zst">arch</a>
`
	items := parseProxmoxIndexCatalog(html, "ustc", "https://mirrors.ustc.edu.cn/proxmox/images/system", "amd64")
	if len(items) != 3 {
		t.Fatalf("expected 3 amd64 systemd templates, got %d: %+v", len(items), items)
	}
	if items[0].Distro != "debian" || items[0].Filename != "debian-13-standard_13.6-1_amd64.tar.zst" {
		t.Fatalf("expected latest debian 13 first, got %+v", items[0])
	}
	if items[1].Distro != "archlinux" || items[1].Release != "current" {
		t.Fatalf("expected archlinux second, got %+v", items[1])
	}
	if items[2].Distro != "rockylinux" || items[2].Release != "9" {
		t.Fatalf("expected rockylinux third, got %+v", items[2])
	}
}

func TestIsAllowedOnlineImageURL(t *testing.T) {
	allowed := []string{
		"https://mirrors.tuna.tsinghua.edu.cn/lxc-images/images/debian/bookworm/amd64/default/20261118_05:24/rootfs.tar.xz",
		"https://mirrors.ustc.edu.cn/proxmox/images/system/rockylinux-9-default_20240912_amd64.tar.xz",
		"https://mirrors.bfsu.edu.cn/lxc-images/images/archlinux/current/amd64/default/20261118_04:18/rootfs.tar.xz",
		"https://images.linuxcontainers.org/images/fedora/43/amd64/default/20261117_20:33/rootfs.tar.xz",
	}
	for _, u := range allowed {
		if !IsAllowedOnlineImageURL(u) {
			t.Fatalf("expected allowed URL: %s", u)
		}
	}
	disallowed := []string{
		"http://mirrors.tuna.tsinghua.edu.cn/lxc-images/a.tar.xz",
		"https://evil.example.com/rootfs.tar.xz",
		"https://mirrors.ustc.edu.cn/other/file.tar.xz",
		"file:///etc/passwd",
	}
	for _, u := range disallowed {
		if IsAllowedOnlineImageURL(u) {
			t.Fatalf("expected rejected URL: %s", u)
		}
	}
}

func TestRenderServiceUnitOpenWrtVsSystemd(t *testing.T) {
	owCfg := ContainerConfig{
		Machine:     "ow-gw",
		Rootfs:      "/vol1/nspawn/ow-gw/rootfs",
		ConfDir:     "/vol1/nspawn/ow-gw/config",
		ServiceName: "nspawn-ow-gw.service",
		PhysicalNIC: "enp2s0",
		InitMode:    InitModeOpenWrt,
		EnableTun:   true,
	}
	owUnit := RenderServiceUnit(owCfg)
	if !strings.Contains(owUnit, "Type=simple\n") || strings.Contains(owUnit, "Type=notify") {
		t.Fatalf("expected OpenWrt unit to use Type=simple without Type=notify:\n%s", owUnit)
	}
	if !strings.Contains(owUnit, "--bind=/dev/net/tun") || !strings.Contains(owUnit, "DeviceAllow=/dev/net/tun rwm") {
		t.Fatalf("expected OpenWrt unit to enable /dev/net/tun:\n%s", owUnit)
	}
	if strings.Contains(owUnit, "--boot") || !strings.Contains(owUnit, "/sbin/init\n") {
		t.Fatalf("expected OpenWrt unit to run /sbin/init without --boot:\n%s", owUnit)
	}

	sysCfg := ContainerConfig{
		Machine:     "debbox",
		Rootfs:      "/vol1/nspawn/debbox/rootfs",
		ConfDir:     "/vol1/nspawn/debbox/config",
		ServiceName: "nspawn-debbox.service",
		PhysicalNIC: "enp2s0",
	}
	sysUnit := RenderServiceUnit(sysCfg)
	if !strings.Contains(sysUnit, "Type=notify\n") || !strings.Contains(sysUnit, "--boot") {
		t.Fatalf("expected standard systemd unit to keep Type=notify and --boot:\n%s", sysUnit)
	}
	if strings.Contains(sysUnit, "/dev/net/tun") || strings.Contains(sysUnit, "/sbin/init") {
		t.Fatalf("standard systemd unit should not contain OpenWrt directives:\n%s", sysUnit)
	}
}

func TestEnsureOpenWrtLanDHCPIgnored(t *testing.T) {
	in := `config dnsmasq
	option domainneeded '1'

config dhcp 'lan'
	option interface 'lan'
	option start '100'
	option ignore '0'

config dhcp 'wan'
	option interface 'wan'
	option ignore '1'
`
	out := ensureOpenWrtLanDHCPIgnored(in)
	if strings.Contains(out, "option ignore '0'") {
		t.Fatalf("expected option ignore '0' to be replaced in lan section:\n%s", out)
	}
	if strings.Count(out, "option ignore '1'") != 2 {
		t.Fatalf("expected ignore '1' in both lan and wan sections:\n%s", out)
	}

	inNoIgnore := "config dhcp 'lan'\n\toption interface 'lan'\n\toption start '100'\n"
	out2 := ensureOpenWrtLanDHCPIgnored(inNoIgnore)
	if !strings.Contains(out2, "\toption ignore '1'") {
		t.Fatalf("expected option ignore '1' injected into lan section:\n%s", out2)
	}
}

func TestParseServiceFileOpenWrtTunNotInBindMounts(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, "ow-test")
	rootfs := filepath.Join(base, "rootfs")
	confDir := filepath.Join(base, "config")
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(confDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc/openwrt_release"), []byte("DISTRIB_ID='OpenWrt'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	svcPath := filepath.Join(confDir, "nspawn-ow-test.service")
	cfgInput := ContainerConfig{
		Machine:     "ow-test",
		Rootfs:      rootfs,
		ConfDir:     confDir,
		ServiceName: "nspawn-ow-test.service",
		ServicePath: svcPath,
		PhysicalNIC: "enp2s0",
		InitMode:    InitModeOpenWrt,
		EnableTun:   true,
	}
	if err := os.WriteFile(svcPath, []byte(RenderServiceUnit(cfgInput)), 0644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseServiceFile(svcPath, filepath.Join(tmp, "sys-ow-test.service"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.InitMode != InitModeOpenWrt || !parsed.EnableTun {
		t.Fatalf("expected InitMode=openwrt and EnableTun=true, got %+v", parsed)
	}
	if len(parsed.BindMounts) != 0 {
		t.Fatalf("expected /dev/net/tun not to appear in BindMounts, got %+v", parsed.BindMounts)
	}
}

func TestCronParserAndNextRuns(t *testing.T) {
	cs, err := ParseCron("30 3 * * 0")
	if err != nil {
		t.Fatal(err)
	}
	sun := time.Date(2026, 11, 29, 3, 30, 0, 0, time.Local) // Sunday
	if sun.Weekday() != time.Sunday || !cs.Matches(sun) {
		t.Fatalf("expected Sunday 03:30 to match, weekday=%v", sun.Weekday())
	}
	mon := time.Date(2026, 11, 30, 3, 30, 0, 0, time.Local) // Monday
	if cs.Matches(mon) {
		t.Fatalf("expected Monday 03:30 not to match")
	}

	stepCron, err := ParseCron("0 */6 * * *")
	if err != nil {
		t.Fatal(err)
	}
	runs := stepCron.NextRuns(time.Date(2026, 11, 27, 5, 15, 0, 0, time.Local), 3)
	if len(runs) != 3 {
		t.Fatalf("expected 3 next runs, got %d", len(runs))
	}
	if runs[0].Hour() != 6 || runs[1].Hour() != 12 || runs[2].Hour() != 18 {
		t.Fatalf("unexpected step runs: %v", runs)
	}

	if _, err := ParseCron("* * *"); err == nil {
		t.Fatalf("expected error for 3-field cron expression")
	}
}

func TestSelectExpiredBackupEntries(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.Local)
	names := []string{
		"devbox-manual-20261101-010000",          // different tag: MUST NEVER prune
		"devbox-before-upgrade-20261102-010000",  // different tag: MUST NEVER prune
		"devbox-auto-20261127-030000",            // newest auto (index 0)
		"devbox-auto-20261126-030000",            // index 1
		"devbox-auto-20261125-030000",            // index 2 (>= keepCount=2)
		"devbox-auto-20261001-030000.tar.zst",    // archive with ext, > 30 days
	}
	expired := selectExpiredBackupEntries("devbox", "auto", names, 2, 30, now)
	if len(expired) != 2 {
		t.Fatalf("expected 2 expired entries, got %v", expired)
	}
	if expired[0] != "devbox-auto-20261125-030000" || expired[1] != "devbox-auto-20261001-030000.tar.zst" {
		t.Fatalf("unexpected expired entries: %v", expired)
	}

	// Safety floor: never delete the single newest entry even if older than keepDays
	onlyOneOld := []string{"devbox-auto-20200101-030000"}
	if exp := selectExpiredBackupEntries("devbox", "auto", onlyOneOld, 1, 1, now); len(exp) != 0 {
		t.Fatalf("safety floor violated, got %v", exp)
	}
}

func TestContainerSpecBackupRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	specPath := filepath.Join(tmp, "devbox", "config", ContainerSpecFileName)
	spec := ContainerSpec{
		Machine: "devbox",
		System: SpecSystem{
			Hostname:    "devbox",
			Autostart:   true,
			ServiceName: "nspawn-devbox.service",
		},
		Backup: &SpecBackup{
			Snapshot: &SnapshotScheduleSpec{
				Enabled:   true,
				Cron:      "0 3 * * *",
				Tag:       "auto",
				ReadOnly:  true,
				KeepCount: 7,
			},
			Export: &ExportScheduleSpec{
				Enabled:   true,
				Cron:      "0 4 * * 0",
				Tag:       "auto",
				Format:    "tar.zst",
				KeepCount: 3,
				KeepDays:  30,
			},
		},
	}
	if err := WriteContainerSpec(specPath, spec); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadContainerSpec(specPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Backup == nil || loaded.Backup.Snapshot == nil || loaded.Backup.Export == nil {
		t.Fatalf("expected Backup round-trip to preserve Snapshot & Export, got %+v", loaded.Backup)
	}
	if loaded.Backup.Snapshot.Cron != "0 3 * * *" || loaded.Backup.Snapshot.KeepCount != 7 {
		t.Fatalf("unexpected Snapshot schedule: %+v", loaded.Backup.Snapshot)
	}
	if loaded.Backup.Export.Format != "tar.zst" || loaded.Backup.Export.KeepDays != 30 {
		t.Fatalf("unexpected Export schedule: %+v", loaded.Backup.Export)
	}

	// Verify ConfigToSpec without Backup preserves existing spec.Backup
	cfg := SpecToConfig(*loaded, filepath.Dir(filepath.Dir(specPath)), "")
	cfg.Backup = nil
	resaved := ConfigToSpec(*cfg, loaded)
	if resaved.Backup == nil || resaved.Backup.Snapshot == nil || !resaved.Backup.Snapshot.Enabled {
		t.Fatalf("expected ConfigToSpec to preserve existing spec.Backup")
	}
}

func TestMigrateOpenWrtAndHostnameSync(t *testing.T) {
	tmp := t.TempDir()

	// 1. OpenWrt rootfs migration identity & network preservation
	owRootfs := filepath.Join(tmp, "ow-rootfs")
	if err := os.MkdirAll(filepath.Join(owRootfs, "etc/config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owRootfs, "etc/openwrt_release"), []byte("DISTRIB_ID='OpenWrt'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owRootfs, "etc/config/system"), []byte("config system\n\toption hostname 'old-ow'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owRootfs, "etc/config/dhcp"), []byte("config dhcp 'lan'\n\toption interface 'lan'\n\toption ignore '0'\n"), 0644); err != nil {
		t.Fatal(err)
	}

	initMode, enableTun, distro, user := updateMigratedContainerIdentityAndNet(owRootfs, "old-ow", "new-ow", ContainerConfig{
		Machine: "old-ow",
		Layout:  LayoutExternal,
	})
	if initMode != InitModeOpenWrt || !enableTun || distro != "openwrt" || user != "root" {
		t.Fatalf("unexpected OpenWrt migration metadata: mode=%q tun=%v distro=%q user=%q", initMode, enableTun, distro, user)
	}
	if _, err := os.Stat(filepath.Join(owRootfs, "etc/systemd/network/20-host0.network")); err == nil {
		t.Fatalf("OpenWrt migration should not write systemd-networkd config")
	}
	sysCfg, _ := os.ReadFile(filepath.Join(owRootfs, "etc/config/system"))
	if !strings.Contains(string(sysCfg), "option hostname 'new-ow'") {
		t.Fatalf("expected UCI hostname updated to new-ow, got:\n%s", string(sysCfg))
	}
	dhcpCfg, _ := os.ReadFile(filepath.Join(owRootfs, "etc/config/dhcp"))
	if !strings.Contains(string(dhcpCfg), "option ignore '1'") {
		t.Fatalf("expected LAN DHCP ignore '1', got:\n%s", string(dhcpCfg))
	}

	// 2. Standard Debian rootfs rename updates both /etc/hostname and /etc/hosts
	debRootfs := filepath.Join(tmp, "deb-rootfs")
	if err := os.MkdirAll(filepath.Join(debRootfs, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(debRootfs, "etc/hosts"), []byte("127.0.0.1 localhost\n127.0.1.1 old-deb\n"), 0644); err != nil {
		t.Fatal(err)
	}
	debMode, debTun, _, _ := updateMigratedContainerIdentityAndNet(debRootfs, "old-deb", "new-deb", ContainerConfig{
		Machine: "old-deb",
		Distro:  "debian",
	})
	if debMode != "" || debTun {
		t.Fatalf("expected systemd defaults for Debian rootfs, got mode=%q tun=%v", debMode, debTun)
	}
	hostsBytes, _ := os.ReadFile(filepath.Join(debRootfs, "etc/hosts"))
	if !strings.Contains(string(hostsBytes), "127.0.1.1 new-deb") {
		t.Fatalf("expected /etc/hosts updated to new-deb, got:\n%s", string(hostsBytes))
	}
}

func TestLegacyContainerBackupPersistenceAndRefresh(t *testing.T) {
	tmp := t.TempDir()
	store, err := config.NewStore(filepath.Join(tmp, "etc"))
	if err != nil {
		t.Fatal(err)
	}
	baseRoot := filepath.Join(tmp, "legacy-pool")
	if err := store.Save(config.AppConfig{
		StandardBaseRoot: filepath.Join(tmp, "std-pool"),
		BaseRoots:        []string{baseRoot},
	}); err != nil {
		t.Fatal(err)
	}

	legacyConf := filepath.Join(baseRoot, "oldbox-conf")
	legacyRootfs := filepath.Join(baseRoot, "oldbox", "rootfs")
	if err := os.MkdirAll(legacyConf, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacyRootfs, 0755); err != nil {
		t.Fatal(err)
	}
	unit := RenderServiceUnit(ContainerConfig{
		Machine:     "oldbox",
		Rootfs:      legacyRootfs,
		ConfDir:     legacyConf,
		ServiceName: "nspawn-oldbox.service",
	})
	if err := os.WriteFile(filepath.Join(legacyConf, "nspawn-oldbox.service"), []byte(unit), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(store, NewTaskManager())
	res, err := mgr.SaveContainerBackup("oldbox", SpecBackup{
		Snapshot: &SnapshotScheduleSpec{
			Enabled:   true,
			Cron:      "0 2 * * *",
			Tag:       "nightly",
			ReadOnly:  true,
			KeepCount: 5,
		},
	})
	if err != nil {
		t.Fatalf("SaveContainerBackup on legacy container failed: %v", err)
	}
	if res.Backup.Snapshot == nil || !res.Backup.Snapshot.Enabled || res.Backup.Snapshot.Tag != "nightly" {
		t.Fatalf("expected legacy backup schedule to remain enabled after save, got %+v", res.Backup.Snapshot)
	}

	// Verify DiscoverContainers does NOT wipe legacy schedule
	if _, err := mgr.DiscoverContainers(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := mgr.GetContainerBackup("oldbox")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Backup.Snapshot == nil || !reloaded.Backup.Snapshot.Enabled {
		t.Fatalf("expected legacy schedule to survive DiscoverContainers, got %+v", reloaded.Backup.Snapshot)
	}

	// Verify fresh Manager.RefreshBackupSchedules picks up *-conf/container.json
	mgr2 := NewManager(store, NewTaskManager())
	mgr2.RefreshBackupSchedules()
	mgr2.schedMu.Lock()
	_, ok := mgr2.schedRules["oldbox"]
	mgr2.schedMu.Unlock()
	if !ok {
		t.Fatalf("expected RefreshBackupSchedules to load legacy container schedule")
	}
}

func TestParseServiceFileRespectsContainerJSONSourceOfTruth(t *testing.T) {
	tmp := t.TempDir()
	paths := BuildStandardPaths(filepath.Join(tmp, "nspawn"), "truthbox")
	for _, d := range []string{paths.Rootfs, paths.ConfDir, paths.SnapshotDir, paths.ExportsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Write stale .service and stale network-up.sh
	staleCfg := ContainerConfig{
		Machine:     "truthbox",
		Rootfs:      paths.Rootfs,
		ConfDir:     paths.ConfDir,
		ServiceName: paths.ServiceName,
		ServicePath: paths.ServicePath,
		PhysicalNIC: "eth0",
		TmpSize:     "1G",
		MemoryMax:   "2G",
	}
	if err := os.WriteFile(paths.ServicePath, []byte(RenderServiceUnit(staleCfg)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.ConfDir, "network-up.sh"), []byte(RenderNetworkUpScript("eth0", "ve-truthbox")), 0700); err != nil {
		t.Fatal(err)
	}

	// Write updated container.json (Single Source of Truth)
	spec := ContainerSpec{
		Machine: "truthbox",
		System: SpecSystem{
			ServiceName: paths.ServiceName,
			Autostart:   true,
		},
		Network: SpecNetwork{
			PhysicalNIC: "enp5s0",
			Veth:        "ve-truthbox",
		},
		Resources: SpecResources{
			TmpSize:   "8G",
			MemoryMax: "16G",
			CPUQuota:  "400%",
		},
		BindMounts: []BindMount{
			{HostPath: "/vol1/media", ContainerPath: "/media", ReadOnly: true},
		},
	}
	if err := WriteContainerSpec(paths.ConfigFile, spec); err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseServiceFile(paths.ServicePath, "")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.PhysicalNIC != "enp5s0" || parsed.TmpSize != "8G" || parsed.MemoryMax != "16G" || parsed.CPUQuota != "400%" {
		t.Fatalf("expected container.json fields to take precedence, got %+v", parsed)
	}
	if len(parsed.BindMounts) != 1 || parsed.BindMounts[0].HostPath != "/vol1/media" {
		t.Fatalf("expected BindMounts from container.json, got %+v", parsed.BindMounts)
	}
}

func TestInputValidatorsAndContainerOpMutex(t *testing.T) {
	if err := validateTmpSize("4G"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := validateTmpSize("4G\nExecStart=/bin/evil"); err == nil {
		t.Fatalf("expected newline in TmpSize to fail")
	}
	if err := validateMemoryMax("8G\nExecStartPost=/bin/sh"); err == nil {
		t.Fatalf("expected newline in MemoryMax to fail")
	}
	if err := validateCPUQuota("200%"); err != nil {
		t.Fatalf("expected 200%% CPUQuota to pass: %v", err)
	}
	if err := validateCPUQuota("200"); err == nil {
		t.Fatalf("expected CPUQuota without %% to fail")
	}
	if err := validatePhysicalNIC("enp2s0;id"); err == nil {
		t.Fatalf("expected shell chars in NIC to fail")
	}
	if err := validateSuite("../sid"); err == nil {
		t.Fatalf("expected path traversal suite to fail")
	}
	if err := validateMirrorURL("https://mirrors.ustc.edu.cn/debian"); err != nil {
		t.Fatalf("expected valid mirror URL to pass: %v", err)
	}
	if err := validateMirrorURL("https://evil.example/debian main\ndeb http://x"); err == nil {
		t.Fatalf("expected whitespace/newline in mirror URL to fail")
	}
	if err := validatePassword("secret\nroot:hacked"); err == nil {
		t.Fatalf("expected newline in password to fail")
	}

	tmp := t.TempDir()
	if _, err := validateBindMounts([]BindMount{{HostPath: tmp, ContainerPath: "/mnt/a:b"}}); err == nil {
		t.Fatalf("expected colon in ContainerPath to fail")
	}
	if binds, err := validateBindMounts([]BindMount{{HostPath: tmp, ContainerPath: "/mnt/data", ReadOnly: true}}); err != nil || len(binds) != 1 {
		t.Fatalf("expected valid BindMount to pass, got %v", err)
	}

	// RenderServiceUnit strips unexpected newlines defensively
	unit := RenderServiceUnit(ContainerConfig{
		Machine:   "box",
		Rootfs:    "/vol1/nspawn/box/rootfs",
		ConfDir:   "/vol1/nspawn/box/config",
		MemoryMax: "4G\nExecStartPost=/bin/evil",
	})
	if strings.Contains(unit, "\nExecStartPost=/bin/evil") {
		t.Fatalf("newline directive leaked into unit:\n%s", unit)
	}

	// Per-container mutex prevents concurrent operations on same container
	store, err := config.NewStore(filepath.Join(tmp, "etc"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store, NewTaskManager())
	rel1, err := mgr.acquireContainerOp("rollback", "devbox")
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	if _, err := mgr.acquireContainerOp("export-image", "devbox"); err == nil {
		t.Fatalf("expected second concurrent acquire on devbox to fail")
	}
	if relOther, err := mgr.acquireContainerOp("export-image", "otherbox"); err != nil {
		t.Fatalf("different container should not block: %v", err)
	} else {
		relOther()
	}
	rel1()
	if rel2, err := mgr.acquireContainerOp("export-image", "devbox"); err != nil {
		t.Fatalf("acquire after release should succeed: %v", err)
	} else {
		rel2()
	}
}
