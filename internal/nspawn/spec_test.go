package nspawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
