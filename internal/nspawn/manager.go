package nspawn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fn-nspawn/internal/config"
)

var (
	AppVersion    = "1.4.2"
	reMachineName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	reUserName    = regexp.MustCompile(`^[a-z_][a-z0-9_-]*[$]?$`)
)

type Manager struct {
	cfgStore           *config.Store
	tasks              *TaskManager
	onlineCatalogMu    sync.Mutex
	onlineCatalogCache map[string]onlineCatalogCacheEntry
	schedMu            sync.Mutex
	schedRules         map[string]SpecBackup
	schedStatus        map[string]ScheduleRunStatus
	schedRunning       map[string]bool
	schedLastMinute    map[string]string
}

func NewManager(cfgStore *config.Store, tasks *TaskManager) *Manager {
	return &Manager{
		cfgStore:           cfgStore,
		tasks:              tasks,
		onlineCatalogCache: make(map[string]onlineCatalogCacheEntry),
		schedRules:         make(map[string]SpecBackup),
		schedStatus:        make(map[string]ScheduleRunStatus),
		schedRunning:       make(map[string]bool),
		schedLastMinute:    make(map[string]string),
	}
}

func (m *Manager) Tasks() *TaskManager {
	return m.tasks
}

func effectiveScanRoots(appCfg config.AppConfig) []string {
	seen := make(map[string]bool)
	var roots []string
	add := func(raw string) {
		c := filepath.Clean(strings.TrimSpace(raw))
		if c == "" || c == "." || seen[c] {
			return
		}
		seen[c] = true
		roots = append(roots, c)
	}
	add(appCfg.StandardBaseRoot)
	for _, r := range appCfg.BaseRoots {
		add(r)
	}
	return roots
}

func parseMachinectlListOutput(raw string) []string {
	var machines []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		name, class, service := fields[0], fields[1], fields[2]
		if class != "container" || service != "systemd-nspawn" || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		machines = append(machines, name)
	}
	return machines
}

func enrichConfigFromUnitText(raw string, cfg *ContainerConfig) {
	lines := joinContinuationLines(raw)
	var execStart string
	var execStartPost string
	section := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToUpper(trimmed)
			continue
		}
		if section != "[SERVICE]" {
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		val := strings.TrimSpace(trimmed[eq+1:])
		switch key {
		case "ExecStart":
			execStart = val
		case "ExecStartPost":
			execStartPost = strings.TrimPrefix(val, "-")
		case "MemoryMax":
			if val != "" {
				cfg.MemoryMax = val
			}
		case "CPUQuota":
			if val != "" {
				cfg.CPUQuota = val
			}
		case "TasksMax":
			if val != "" {
				cfg.TasksMax = val
			}
		}
	}
	if execStart != "" {
		origMachine := cfg.Machine
		origRootfs := cfg.Rootfs
		parseExecStartArgs(execStart, cfg)
		if origMachine != "" {
			cfg.Machine = origMachine
		}
		if origRootfs != "" && (cfg.Rootfs == "" || strings.Contains(cfg.Rootfs, "%")) {
			cfg.Rootfs = origRootfs
		}
	}
	if execStartPost != "" {
		fields := strings.Fields(execStartPost)
		if len(fields) > 0 && filepath.IsAbs(fields[0]) {
			if b, err := os.ReadFile(fields[0]); err == nil {
				if m := rePhysicalNIC.FindStringSubmatch(string(b)); len(m) > 1 {
					cfg.PhysicalNIC = m[1]
				}
				if m := reVeth.FindStringSubmatch(string(b)); len(m) > 1 && !strings.Contains(m[1], "%") {
					cfg.Veth = m[1]
				}
			}
		}
	}
}

func enrichConfigFromNspawnFile(nspawnPath string, cfg *ContainerConfig) {
	data, err := os.ReadFile(nspawnPath)
	if err != nil {
		return
	}
	section := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToUpper(trimmed)
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		val := strings.TrimSpace(trimmed[eq+1:])
		if section == "[FILES]" {
			switch key {
			case "Bind":
				if bm, ok := parseBindValue(val, false); ok {
					cfg.BindMounts = append(cfg.BindMounts, bm)
				}
			case "BindReadOnly":
				if bm, ok := parseBindValue(val, true); ok {
					cfg.BindMounts = append(cfg.BindMounts, bm)
				}
			}
		}
	}
}

func (m *Manager) inspectExternalMachinectlContainer(machine string, defaultNIC string) ContainerConfig {
	cfg := ContainerConfig{
		Machine:     machine,
		Layout:      LayoutExternal,
		TmpSize:     "4G",
		TasksMax:    "infinity",
		PhysicalNIC: defaultNIC,
		Veth:        "ve-" + machine,
		SourceType:  "external",
	}

	machinectlBin := lookBinary("machinectl", "/usr/bin/machinectl", "/bin/machinectl")
	if out, err := exec.Command(machinectlBin, "show", machine, "--property=Name,RootDirectory,Unit,Service,Leader,State", "--no-pager").Output(); err == nil {
		props := parseKeyValues(string(out))
		if u := strings.TrimSpace(props["Unit"]); u != "" {
			cfg.ServiceName = u
		}
		if rd := strings.TrimSpace(props["RootDirectory"]); rd != "" {
			cfg.Rootfs = filepath.Clean(rd)
		}
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = fmt.Sprintf("systemd-nspawn@%s.service", machine)
	}
	if cfg.Rootfs != "" && cfg.Rootfs != "/" {
		cfg.ContainerDir = filepath.Dir(cfg.Rootfs)
		cfg.BaseRoot = filepath.Dir(cfg.ContainerDir)
	}

	if strings.HasSuffix(cfg.ServiceName, ".service") {
		if unitOut, err := exec.Command("systemctl", "cat", cfg.ServiceName, "--no-pager").Output(); err == nil {
			enrichConfigFromUnitText(string(unitOut), &cfg)
		}
	}
	nspawnCfg := filepath.Join("/etc/systemd/nspawn", machine+".nspawn")
	if fileExists(nspawnCfg) {
		enrichConfigFromNspawnFile(nspawnCfg, &cfg)
	}
	return cfg
}

func (m *Manager) DiscoverContainers() ([]ContainerInfo, error) {
	seenServices := make(map[string]ContainerConfig)
	seenMachines := make(map[string]bool)
	recordUnit := func(key string, cfg ContainerConfig) {
		seenServices[key] = cfg
		if cfg.Machine != "" {
			seenMachines[cfg.Machine] = true
		}
	}

	// 1. Scan /etc/systemd/system/nspawn-*.service (synced or linked units)
	sysMatches, _ := filepath.Glob("/etc/systemd/system/nspawn-*.service")
	for _, linkPath := range sysMatches {
		targetPath := linkPath
		if resolved, err := os.Readlink(linkPath); err == nil {
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(linkPath), resolved)
			}
			targetPath = resolved
		}
		cfg, err := ParseServiceFile(targetPath, linkPath)
		if err != nil {
			continue
		}
		recordUnit(cfg.ServiceName, *cfg)
		if cfg.BaseRoot != "" {
			_ = m.cfgStore.AddBaseRoot(cfg.BaseRoot)
		}
	}

	// 2. Scan StandardBaseRoot first, then configured supplementary BaseRoots.
	// Standardized containers always take precedence over legacy *-conf/ regardless of order.
	appCfg := m.cfgStore.Get()
	scanRoots := effectiveScanRoots(appCfg)

	// 2a. Standardized layout with declarative container.json:
	// <Root>/<Machine>/config/container.json
	for _, baseRoot := range scanRoots {
		specMatches, _ := filepath.Glob(filepath.Join(baseRoot, "*", "config", ContainerSpecFileName))
		for _, specPath := range specMatches {
			spec, err := ReadContainerSpec(specPath)
			if err != nil {
				continue
			}
			confDir := filepath.Dir(specPath)
			containerDir := filepath.Dir(confDir)
			svcName := spec.System.ServiceName
			if svcName == "" {
				svcName = fmt.Sprintf("nspawn-%s.service", spec.Machine)
			}
			if _, exists := seenServices[svcName]; exists {
				continue
			}
			linkPath := filepath.Join("/etc/systemd/system", svcName)
			cfg := SpecToConfig(*spec, containerDir, linkPath)
			recordUnit(svcName, *cfg)
		}
	}

	// 2b. Standardized layout without container.json:
	// <Root>/<Machine>/config/nspawn-*.service
	for _, baseRoot := range scanRoots {
		stdSvcMatches, _ := filepath.Glob(filepath.Join(baseRoot, "*", "config", "nspawn-*.service"))
		for _, svcPath := range stdSvcMatches {
			svcName := filepath.Base(svcPath)
			if _, exists := seenServices[svcName]; exists {
				continue
			}
			linkPath := filepath.Join("/etc/systemd/system", svcName)
			cfg, err := ParseServiceFile(svcPath, linkPath)
			if err != nil {
				continue
			}
			recordUnit(svcName, *cfg)
		}
	}

	// 2c. Legacy layout: <Root>/<Machine>-conf/nspawn-*.service
	for _, baseRoot := range scanRoots {
		confDirs, _ := filepath.Glob(filepath.Join(baseRoot, "*-conf"))
		for _, confDir := range confDirs {
			svcFiles, _ := filepath.Glob(filepath.Join(confDir, "nspawn-*.service"))
			for _, svcPath := range svcFiles {
				svcName := filepath.Base(svcPath)
				if _, exists := seenServices[svcName]; exists {
					continue
				}
				linkPath := filepath.Join("/etc/systemd/system", svcName)
				cfg, err := ParseServiceFile(svcPath, linkPath)
				if err != nil {
					continue
				}
				recordUnit(svcName, *cfg)
			}
		}
	}

	// 3. Auto-discover running external systemd-nspawn containers via machinectl
	machinectlBin := lookBinary("machinectl", "/usr/bin/machinectl", "/bin/machinectl")
	if mOut, err := exec.Command(machinectlBin, "list", "--no-legend", "--no-pager").Output(); err == nil {
		for _, extMachine := range parseMachinectlListOutput(string(mOut)) {
			if seenMachines[extMachine] {
				continue
			}
			extCfg := m.inspectExternalMachinectlContainer(extMachine, appCfg.DefaultPhysicalNIC)
			recordUnit("ext:"+extMachine, extCfg)
		}
	}

	// Sort by service/machine name
	names := make([]string, 0, len(seenServices))
	for k := range seenServices {
		names = append(names, k)
	}
	sort.Strings(names)

	stdRoot := filepath.Clean(appCfg.StandardBaseRoot)
	out := make([]ContainerInfo, 0, len(names))
	for _, k := range names {
		cfg := seenServices[k]
		if cfg.Layout == LayoutStandard && stdRoot != "" && stdRoot != "." && cfg.BaseRoot != "" {
			cfg.InStandardRoot = filepath.Clean(cfg.BaseRoot) == stdRoot
		}
		cfg.CanMigrateToStandard = cfg.Rootfs != "" && cfg.Rootfs != "/" && !cfg.InStandardRoot
		rt := m.inspectRuntime(cfg)
		out = append(out, ContainerInfo{
			Config:  cfg,
			Runtime: rt,
		})
	}
	m.syncAllSchedulesFromContainers(out)
	return out, nil
}

func (m *Manager) GetContainer(machine string) (*ContainerInfo, error) {
	list, err := m.DiscoverContainers()
	if err != nil {
		return nil, err
	}
	for _, item := range list {
		if item.Config.Machine == machine {
			return &item, nil
		}
	}
	return nil, fmt.Errorf("未找到容器：%s", machine)
}

func (m *Manager) inspectRuntime(cfg ContainerConfig) ContainerRuntime {
	rt := ContainerRuntime{
		ActiveState:   "unknown",
		SubState:      "unknown",
		UnitFileState: "unknown",
	}

	if cfg.Rootfs != "" {
		if st, err := os.Stat(cfg.Rootfs); err == nil && st.IsDir() {
			rt.RootfsExists = true
		}
		if rt.RootfsExists {
			if err := exec.Command("btrfs", "subvolume", "show", cfg.Rootfs).Run(); err == nil {
				rt.RootfsIsBtrfs = true
			}
		}
	}

	// Read OS pretty name from rootfs /etc/os-release
	if rt.RootfsExists {
		if osRel, err := os.ReadFile(filepath.Join(cfg.Rootfs, "etc/os-release")); err == nil {
			for _, line := range strings.Split(string(osRel), "\n") {
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					rt.OSPrettyName = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), "\"'\r ")
					break
				}
			}
		}
	}

	// systemctl show
	out, err := exec.Command("systemctl", "show", cfg.ServiceName,
		"--property=ActiveState,SubState,LoadState,UnitFileState,MainPID,MemoryCurrent,CPUUsageNSec,ExecMainStartTimestamp,ExecMainStartTimestampUSec,ExecMainStartTimestampMonotonic").Output()
	if err == nil {
		props := parseKeyValues(string(out))
		if v := props["ActiveState"]; v != "" {
			rt.ActiveState = v
		}
		if v := props["SubState"]; v != "" {
			rt.SubState = v
		}
		if v := props["LoadState"]; v != "" {
			rt.LoadState = v
		}
		if v := props["UnitFileState"]; v != "" {
			rt.UnitFileState = v
			rt.Enabled = v == "enabled" || v == "enabled-runtime"
		}
		if !rt.Enabled && rt.UnitFileState != "disabled" && rt.UnitFileState != "disabled-runtime" && rt.UnitFileState != "masked" {
			if _, lErr := os.Lstat(filepath.Join("/etc/systemd/system/multi-user.target.wants", cfg.ServiceName)); lErr == nil {
				rt.Enabled = true
			}
		}
		if v, err := strconv.Atoi(props["MainPID"]); err == nil {
			rt.MainPID = v
		}
		if v, err := strconv.ParseUint(props["MemoryCurrent"], 10, 64); err == nil && v != ^uint64(0) {
			rt.MemoryCurrent = v
		}
		if v, err := strconv.ParseUint(props["CPUUsageNSec"], 10, 64); err == nil && v != ^uint64(0) {
			rt.CPUUsageNSec = v
		}
		rt.StartedAt = props["ExecMainStartTimestamp"]
		if v, err := strconv.ParseUint(props["ExecMainStartTimestampMonotonic"], 10, 64); err == nil {
			rt.ExecStartMonotonic = v
		}
		if rt.ActiveState == "active" {
			if usec, err := strconv.ParseInt(props["ExecMainStartTimestampUSec"], 10, 64); err == nil && usec > 0 {
				up := time.Now().Unix() - usec/1_000_000
				if up < 0 {
					up = 0
				}
				rt.UptimeSec = up
			}
		}
	}
	rt.MemoryLimitBytes = parseByteSize(cfg.MemoryMax)

	machinectlBin := lookBinary("machinectl", "/usr/bin/machinectl", "/bin/machinectl")
	// machinectl show
	if mOut, err := exec.Command(machinectlBin, "show", cfg.Machine, "--property=Leader,State", "--no-pager").Output(); err == nil {
		mProps := parseKeyValues(string(mOut))
		if v, err := strconv.Atoi(mProps["Leader"]); err == nil {
			rt.LeaderPID = v
		}
		if mProps["State"] == "running" || rt.LeaderPID > 0 {
			rt.MachinectlUp = true
		}
	}

	// Fallback: locate container leader child PID under MainPID when machinectl show returns no Leader
	if rt.LeaderPID == 0 && rt.MainPID > 0 && rt.ActiveState == "active" {
		if pid := findContainerLeaderPID(rt.MainPID); pid > 0 {
			rt.LeaderPID = pid
		}
	}

	// OVS check
	if ovsPath, err := exec.LookPath("ovs-vsctl"); err == nil {
		if cfg.Veth != "" {
			if brOut, err := exec.Command(ovsPath, "port-to-br", cfg.Veth).Output(); err == nil {
				br := strings.TrimSpace(string(brOut))
				if br != "" {
					rt.OVSBridge = br
					rt.VethAttached = true
				}
			}
		}
		if rt.OVSBridge == "" && cfg.PhysicalNIC != "" {
			if brOut, err := exec.Command(ovsPath, "port-to-br", cfg.PhysicalNIC).Output(); err == nil {
				rt.OVSBridge = strings.TrimSpace(string(brOut))
			}
		}
	}

	// Inspect container host0 MAC/IPs via nsenter into LeaderPID netns
	if rt.LeaderPID > 0 {
		fillNetNSInfo(rt.LeaderPID, &rt)
	}

	// Fallback: machinectl status Address lines if IPv4 is still empty while running
	if (rt.ActiveState == "active" || rt.MachinectlUp) && len(rt.IPv4Addrs) == 0 {
		if stOut, err := exec.Command(machinectlBin, "status", cfg.Machine, "--no-pager").Output(); err == nil {
			v4s, v6s := parseMachinectlStatusAddresses(string(stOut))
			for _, ip := range v4s {
				rt.IPv4Addrs = appendUnique(rt.IPv4Addrs, ip)
			}
			for _, ip := range v6s {
				rt.IPv6Addrs = appendUnique(rt.IPv6Addrs, ip)
			}
		}
	}

	// Host-side veth statistics: tx_bytes = container download (Rx), rx_bytes = container upload (Tx)
	if cfg.Veth != "" {
		statsDir := filepath.Join("/sys/class/net", cfg.Veth, "statistics")
		rt.NetRxBytes = readUint64File(filepath.Join(statsDir, "tx_bytes"))
		rt.NetTxBytes = readUint64File(filepath.Join(statsDir, "rx_bytes"))
	}

	return rt
}

func readUint64File(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func parseMemTotalKB(content string) uint64 {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "MemTotal:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(trimmed, "MemTotal:"))
		if len(fields) == 0 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

func parseByteSize(s string) uint64 {
	trimmed := strings.TrimSpace(strings.ToLower(s))
	if trimmed == "" || trimmed == "infinity" || strings.HasSuffix(trimmed, "%") {
		return 0
	}
	idx := 0
	for idx < len(trimmed) && ((trimmed[idx] >= '0' && trimmed[idx] <= '9') || trimmed[idx] == '.') {
		idx++
	}
	if idx == 0 {
		return 0
	}
	num, err := strconv.ParseFloat(trimmed[:idx], 64)
	if err != nil || num <= 0 {
		return 0
	}
	unit := strings.TrimSpace(trimmed[idx:])
	var mult float64 = 1
	switch unit {
	case "", "b":
		mult = 1
	case "k", "kb", "kib":
		mult = 1024
	case "m", "mb", "mib":
		mult = 1024 * 1024
	case "g", "gb", "gib":
		mult = 1024 * 1024 * 1024
	case "t", "tb", "tib":
		mult = 1024 * 1024 * 1024 * 1024
	default:
		return 0
	}
	return uint64(num * mult)
}

type ipAddrLink struct {
	IfName   string `json:"ifname"`
	Address  string `json:"address"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
		Scope     string `json:"scope"`
	} `json:"addr_info"`
}

func lookBinary(name string, fallbacks ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, fb := range fallbacks {
		if _, err := os.Stat(fb); err == nil {
			return fb
		}
	}
	if len(fallbacks) > 0 {
		return fallbacks[0]
	}
	return name
}

func appendUnique(items []string, val string) []string {
	if val == "" {
		return items
	}
	for _, existing := range items {
		if existing == val {
			return items
		}
	}
	return append(items, val)
}

func findContainerLeaderPID(mainPID int) int {
	mainNetNS, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", mainPID))
	var candidates []int

	childrenPath := fmt.Sprintf("/proc/%d/task/%d/children", mainPID, mainPID)
	if b, err := os.ReadFile(childrenPath); err == nil {
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
				candidates = append(candidates, pid)
			}
		}
	}

	if len(candidates) == 0 {
		if entries, err := os.ReadDir("/proc"); err == nil {
			ppidPrefix := "PPid:\t" + strconv.Itoa(mainPID)
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				pid, err := strconv.Atoi(e.Name())
				if err != nil || pid <= 0 {
					continue
				}
				if statusBytes, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status")); err == nil {
					for _, line := range strings.Split(string(statusBytes), "\n") {
						if strings.TrimSpace(line) == ppidPrefix {
							candidates = append(candidates, pid)
							break
						}
					}
				}
			}
		}
	}

	for _, pid := range candidates {
		if childNS, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid)); err == nil && mainNetNS != "" && childNS != mainNetNS {
			return pid
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return 0
}

func parseIPAddrJSON(data []byte) (mac string, ipv4s, ipv6s []string) {
	var links []ipAddrLink
	if json.Unmarshal(data, &links) != nil {
		return "", nil, nil
	}

	var fallbackMAC string
	var ipMAC string
	for _, link := range links {
		if link.IfName == "lo" {
			continue
		}
		validMAC := link.Address != "" && link.Address != "00:00:00:00:00:00"
		if link.IfName == "host0" && validMAC {
			mac = link.Address
		} else if fallbackMAC == "" && validMAC {
			fallbackMAC = link.Address
		}

		hasIPv4 := false
		for _, ai := range link.AddrInfo {
			local := strings.TrimSpace(ai.Local)
			if local == "" {
				continue
			}
			if ai.Family == "inet" {
				if ai.Scope == "host" || strings.HasPrefix(local, "127.") {
					continue
				}
				if ip := net.ParseIP(local); ip != nil && ip.To4() != nil {
					ipv4s = appendUnique(ipv4s, local)
					hasIPv4 = true
				}
			} else if ai.Family == "inet6" {
				lower := strings.ToLower(local)
				if ai.Scope != "global" || strings.HasPrefix(lower, "fe80:") || local == "::1" {
					continue
				}
				if ip := net.ParseIP(local); ip != nil {
					ipv6s = appendUnique(ipv6s, local)
				}
			}
		}
		if hasIPv4 && ipMAC == "" && validMAC {
			ipMAC = link.Address
		}
	}

	if mac == "" {
		mac = ipMAC
	}
	if mac == "" {
		mac = fallbackMAC
	}
	return mac, ipv4s, ipv6s
}

func parseIPAddrText(text string) (mac string, ipv4s, ipv6s []string) {
	var fallbackMAC string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		ifName := strings.SplitN(fields[1], "@", 2)[0]
		ifName = strings.TrimSuffix(ifName, ":")
		if ifName == "lo" {
			continue
		}
		for i := 2; i < len(fields)-1; i++ {
			switch fields[i] {
			case "link/ether":
				addr := fields[i+1]
				if addr != "" && addr != "00:00:00:00:00:00" {
					if ifName == "host0" {
						mac = addr
					} else if fallbackMAC == "" {
						fallbackMAC = addr
					}
				}
			case "inet":
				if strings.Contains(line, "scope host") {
					continue
				}
				ipStr := strings.SplitN(fields[i+1], "/", 2)[0]
				if strings.HasPrefix(ipStr, "127.") {
					continue
				}
				if ip := net.ParseIP(ipStr); ip != nil && ip.To4() != nil {
					ipv4s = appendUnique(ipv4s, ipStr)
				}
			case "inet6":
				if !strings.Contains(line, "scope global") {
					continue
				}
				ipStr := strings.SplitN(fields[i+1], "/", 2)[0]
				lower := strings.ToLower(ipStr)
				if strings.HasPrefix(lower, "fe80:") || ipStr == "::1" {
					continue
				}
				if ip := net.ParseIP(ipStr); ip != nil {
					ipv6s = appendUnique(ipv6s, ipStr)
				}
			}
		}
	}
	if mac == "" {
		mac = fallbackMAC
	}
	return mac, ipv4s, ipv6s
}

func parseMachinectlStatusAddresses(out string) (ipv4s, ipv6s []string) {
	inAddr := false
	for _, rawLine := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" {
			inAddr = false
			continue
		}
		var tokens []string
		if strings.HasPrefix(trimmed, "Address:") {
			inAddr = true
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "Address:"))
			tokens = strings.Fields(rest)
		} else if inAddr {
			fields := strings.Fields(trimmed)
			if len(fields) > 0 && strings.HasSuffix(fields[0], ":") && net.ParseIP(strings.TrimSuffix(fields[0], ":")) == nil {
				inAddr = false
				continue
			}
			tokens = fields
		}
		for _, tok := range tokens {
			tok = strings.SplitN(tok, "/", 2)[0]
			tok = strings.SplitN(tok, "%", 2)[0]
			ip := net.ParseIP(tok)
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			if ip.To4() != nil {
				ipv4s = appendUnique(ipv4s, ip.String())
			} else {
				ipv6s = appendUnique(ipv6s, ip.String())
			}
		}
	}
	return ipv4s, ipv6s
}

func fillNetNSInfo(leaderPID int, rt *ContainerRuntime) {
	ipBin := lookBinary("ip", "/usr/sbin/ip", "/sbin/ip", "/usr/bin/ip", "/bin/ip")
	nsenterBin := lookBinary("nsenter", "/usr/bin/nsenter", "/bin/nsenter")

	pidStr := strconv.Itoa(leaderPID)
	if out, err := exec.Command(nsenterBin, "-t", pidStr, "-n", ipBin, "-j", "addr", "show").Output(); err == nil {
		mac, v4s, v6s := parseIPAddrJSON(out)
		if mac != "" {
			rt.Host0MAC = mac
		}
		rt.IPv4Addrs = v4s
		rt.IPv6Addrs = v6s
	}

	if len(rt.IPv4Addrs) == 0 || rt.Host0MAC == "" {
		if txtOut, err := exec.Command(nsenterBin, "-t", pidStr, "-n", ipBin, "-o", "addr", "show").Output(); err == nil {
			mac, v4s, v6s := parseIPAddrText(string(txtOut))
			if rt.Host0MAC == "" && mac != "" {
				rt.Host0MAC = mac
			}
			for _, ip := range v4s {
				rt.IPv4Addrs = appendUnique(rt.IPv4Addrs, ip)
			}
			for _, ip := range v6s {
				rt.IPv6Addrs = appendUnique(rt.IPv6Addrs, ip)
			}
		}
	}

	if rt.Host0MAC == "" {
		if macOut, err := exec.Command(nsenterBin, "-t", pidStr, "-n", "cat", "/sys/class/net/host0/address").Output(); err == nil {
			if m := strings.TrimSpace(string(macOut)); m != "" && m != "00:00:00:00:00:00" {
				rt.Host0MAC = m
			}
		}
	}

	routeOut, err := exec.Command(nsenterBin, "-t", pidStr, "-n", ipBin, "route", "show", "default").Output()
	if err == nil {
		fields := strings.Fields(string(routeOut))
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "via" {
				rt.DefaultGateway = fields[i+1]
				break
			}
		}
	}
}

func parseKeyValues(s string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if eq := strings.IndexByte(line, '='); eq > 0 {
			m[line[:eq]] = line[eq+1:]
		}
	}
	return m
}

// ensureSystemdLink installs a regular unit file copy into /etc/systemd/system/
// (instead of a symlink into /vol*) so systemd loads enabled containers even before
// NAS storage pools finish mounting during early boot.
func ensureSystemdLink(servicePath, serviceName string) error {
	if servicePath == "" || !fileExists(servicePath) {
		return fmt.Errorf("systemd service 文件不存在: %s", servicePath)
	}
	data, err := os.ReadFile(servicePath)
	if err != nil {
		return fmt.Errorf("读取 systemd service 文件失败: %v", err)
	}
	_ = os.Chmod(servicePath, 0644)
	sysUnit := filepath.Join("/etc/systemd/system", serviceName)
	_ = os.Remove(sysUnit)
	tmp := sysUnit + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("同步 systemd 单元文件失败: %v", err)
	}
	if err := os.Rename(tmp, sysUnit); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("安装 systemd 单元文件失败: %v", err)
	}
	return nil
}

// applySystemdAutostart configures multi-user.target autostart while keeping the
// /etc/systemd/system/<service> regular file intact and removing stale symlinks.
func applySystemdAutostart(servicePath, serviceName string, autostart bool) error {
	if err := ensureSystemdLink(servicePath, serviceName); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join("/etc/systemd/system/multi-user.target.wants", serviceName))
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload 失败: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if autostart {
		if out, err := exec.Command("systemctl", "enable", serviceName).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl enable 失败: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		if err := ensureSystemdLink(servicePath, serviceName); err != nil {
			return err
		}
	} else {
		_ = exec.Command("systemctl", "disable", serviceName).Run()
		_ = os.Remove(filepath.Join("/etc/systemd/system/multi-user.target.wants", serviceName))
		if err := ensureSystemdLink(servicePath, serviceName); err != nil {
			return err
		}
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload 失败: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func syncStandardUnitFiles(cfg ContainerConfig) error {
	if cfg.Layout != LayoutStandard || cfg.ConfDir == "" || cfg.ServicePath == "" {
		return nil
	}
	specPath := cfg.ConfigFile
	if specPath == "" {
		specPath = filepath.Join(cfg.ConfDir, ContainerSpecFileName)
	}
	spec, err := ReadContainerSpec(specPath)
	if err != nil {
		return err
	}
	reconciled := *SpecToConfig(*spec, cfg.ContainerDir, filepath.Join("/etc/systemd/system", cfg.ServiceName))
	if err := os.MkdirAll(reconciled.ConfDir, 0755); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(reconciled.ConfDir, "network-up.sh"), []byte(RenderNetworkUpScript(reconciled.PhysicalNIC, reconciled.Veth)), 0700)
	_ = os.WriteFile(filepath.Join(reconciled.ConfDir, "network-down.sh"), []byte(RenderNetworkDownScript(reconciled.Veth)), 0700)
	_ = os.WriteFile(filepath.Join(reconciled.ConfDir, "restore.sh"), []byte(RenderRestoreScript(reconciled.Machine, reconciled.PhysicalNIC, reconciled.Rootfs, reconciled.ConfDir, reconciled.ServiceName, reconciled.Autostart)), 0700)

	origServiceBytes, _ := os.ReadFile(reconciled.ServicePath)
	if err := os.WriteFile(reconciled.ServicePath, []byte(RenderServiceUnit(reconciled)), 0644); err != nil {
		return err
	}
	if _, err := exec.LookPath("systemd-analyze"); err == nil {
		if verifyOut, err := exec.Command("systemd-analyze", "verify", reconciled.ServicePath).CombinedOutput(); err != nil {
			if len(origServiceBytes) > 0 {
				_ = os.WriteFile(reconciled.ServicePath, origServiceBytes, 0644)
			}
			return fmt.Errorf("systemd-analyze verify 失败: %s", strings.TrimSpace(string(verifyOut)))
		}
	}
	return nil
}

func (m *Manager) ControlService(machine string, action string) error {
	info, err := m.GetContainer(machine)
	if err != nil {
		return err
	}
	cfg := info.Config
	if cfg.Layout == LayoutExternal {
		switch action {
		case "start", "stop", "restart", "enable", "disable":
			if strings.HasSuffix(cfg.ServiceName, ".service") {
				out, err := exec.Command("systemctl", action, cfg.ServiceName).CombinedOutput()
				if err != nil {
					return fmt.Errorf("systemctl %s 失败: %v (%s)", action, err, strings.TrimSpace(string(out)))
				}
				return nil
			}
			if action == "stop" {
				machinectlBin := lookBinary("machinectl", "/usr/bin/machinectl", "/bin/machinectl")
				out, err := exec.Command(machinectlBin, "poweroff", cfg.Machine).CombinedOutput()
				if err != nil {
					return fmt.Errorf("machinectl poweroff 失败: %v (%s)", err, strings.TrimSpace(string(out)))
				}
				return nil
			}
			return fmt.Errorf("外部临时容器不支持通过 systemd %s，请先使用“迁移为标准容器”", action)
		default:
			return fmt.Errorf("不支持的操作: %s", action)
		}
	}
	switch action {
	case "start", "stop", "restart":
		if (action == "start" || action == "restart") && (!cfg.LinkHealthy || info.Runtime.LoadState == "not-found") {
			if cfg.Layout == LayoutStandard && cfg.HasConfigFile {
				_ = syncStandardUnitFiles(cfg)
			}
			if fileExists(cfg.ServicePath) {
				if err := applySystemdAutostart(cfg.ServicePath, cfg.ServiceName, cfg.Autostart || info.Runtime.Enabled); err != nil {
					return err
				}
			}
		}
		out, err := exec.Command("systemctl", action, cfg.ServiceName).CombinedOutput()
		if err != nil && (action == "start" || action == "restart") && strings.Contains(strings.ToLower(string(out)), "not found") && fileExists(cfg.ServicePath) {
			if err := applySystemdAutostart(cfg.ServicePath, cfg.ServiceName, cfg.Autostart || info.Runtime.Enabled); err != nil {
				return err
			}
			out, err = exec.Command("systemctl", action, cfg.ServiceName).CombinedOutput()
		}
		if err != nil {
			return fmt.Errorf("systemctl %s 失败: %v (%s)", action, err, strings.TrimSpace(string(out)))
		}
		return nil
	case "enable", "disable":
		autostart := action == "enable"
		if err := applySystemdAutostart(cfg.ServicePath, cfg.ServiceName, autostart); err != nil {
			return err
		}
		cfg.Autostart = autostart
		if cfg.Layout == LayoutStandard && cfg.ConfDir != "" {
			specPath := cfg.ConfigFile
			if specPath == "" {
				specPath = filepath.Join(cfg.ConfDir, ContainerSpecFileName)
			}
			var existing *ContainerSpec
			if sp, rErr := ReadContainerSpec(specPath); rErr == nil {
				existing = sp
			}
			spec := ConfigToSpec(cfg, existing)
			_ = WriteContainerSpec(specPath, spec)
			_ = os.WriteFile(filepath.Join(cfg.ConfDir, "restore.sh"),
				[]byte(RenderRestoreScript(cfg.Machine, cfg.PhysicalNIC, cfg.Rootfs, cfg.ConfDir, cfg.ServiceName, cfg.Autostart)), 0700)
		}
		return nil
	default:
		return fmt.Errorf("不支持的操作: %s", action)
	}
}

func (m *Manager) UpdateConfig(machine string, req UpdateConfigRequest) (*ContainerInfo, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	cfg := info.Config
	if cfg.Layout == LayoutExternal {
		return nil, fmt.Errorf("外部容器不支持直接修改配置，请先使用“迁移为标准容器”")
	}

	if req.Description != "" || cfg.HasConfigFile {
		cfg.Description = strings.TrimSpace(req.Description)
	}

	if strings.TrimSpace(req.TmpSize) != "" {
		if !regexp.MustCompile(`^[0-9]+[KMGTP]?$`).MatchString(strings.TrimSpace(req.TmpSize)) {
			return nil, fmt.Errorf("无效的 /tmp 容量格式: %s（示例：4G、512M）", req.TmpSize)
		}
		cfg.TmpSize = strings.TrimSpace(req.TmpSize)
	}

	if strings.TrimSpace(req.PhysicalNIC) != "" {
		cfg.PhysicalNIC = strings.TrimSpace(req.PhysicalNIC)
	}

	// Validate bind mounts
	var cleanBinds []BindMount
	for _, b := range req.BindMounts {
		hp := strings.TrimSpace(b.HostPath)
		cp := strings.TrimSpace(b.ContainerPath)
		if hp == "" || cp == "" {
			return nil, errors.New("Bind Mount 宿主机路径和容器路径均不能为空")
		}
		if !filepath.IsAbs(hp) || !filepath.IsAbs(cp) {
			return nil, fmt.Errorf("Bind Mount 必须使用绝对路径: %s -> %s", hp, cp)
		}
		st, err := os.Stat(hp)
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("宿主机源目录不存在或不是目录: %s", hp)
		}
		cleanBinds = append(cleanBinds, BindMount{
			HostPath:      hp,
			ContainerPath: cp,
			ReadOnly:      b.ReadOnly,
		})
	}
	cfg.BindMounts = cleanBinds
	cfg.MemoryMax = strings.TrimSpace(req.MemoryMax)
	cfg.CPUQuota = strings.TrimSpace(req.CPUQuota)
	if req.Autostart != nil {
		cfg.Autostart = *req.Autostart
	}

	if err := os.MkdirAll(cfg.ConfDir, 0755); err != nil {
		return nil, err
	}

	// Update network-up.sh / network-down.sh / restore.sh
	netUpPath := filepath.Join(cfg.ConfDir, "network-up.sh")
	netDownPath := filepath.Join(cfg.ConfDir, "network-down.sh")
	restorePath := filepath.Join(cfg.ConfDir, "restore.sh")

	if err := os.WriteFile(netUpPath, []byte(RenderNetworkUpScript(cfg.PhysicalNIC, cfg.Veth)), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(netDownPath, []byte(RenderNetworkDownScript(cfg.Veth)), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(restorePath, []byte(RenderRestoreScript(cfg.Machine, cfg.PhysicalNIC, cfg.Rootfs, cfg.ConfDir, cfg.ServiceName, cfg.Autostart)), 0700); err != nil {
		return nil, err
	}

	// Backup existing service file and container.json
	origServiceBytes, _ := os.ReadFile(cfg.ServicePath)
	bakPath := cfg.ServicePath + ".bak"
	if len(origServiceBytes) > 0 {
		_ = os.WriteFile(bakPath, origServiceBytes, 0644)
	}

	unitContent := RenderServiceUnit(cfg)
	if err := os.WriteFile(cfg.ServicePath, []byte(unitContent), 0644); err != nil {
		return nil, err
	}

	// Verify with systemd-analyze verify before applying
	if verifyOut, err := exec.Command("systemd-analyze", "verify", cfg.ServicePath).CombinedOutput(); err != nil {
		if len(origServiceBytes) > 0 {
			_ = os.WriteFile(cfg.ServicePath, origServiceBytes, 0644)
		}
		return nil, fmt.Errorf("systemd-analyze verify 校验失败（已自动回退原配置）: %s", strings.TrimSpace(string(verifyOut)))
	}

	// Persist formatted container.json for standard layout (or if config file already exists)
	if cfg.Layout == LayoutStandard || cfg.HasConfigFile {
		specPath := cfg.ConfigFile
		if specPath == "" {
			specPath = filepath.Join(cfg.ConfDir, ContainerSpecFileName)
		}
		var existing *ContainerSpec
		if sp, rErr := ReadContainerSpec(specPath); rErr == nil {
			existing = sp
		}
		spec := ConfigToSpec(cfg, existing)
		if err := WriteContainerSpec(specPath, spec); err != nil {
			return nil, fmt.Errorf("写入容器配置文件失败 (%s): %v", specPath, err)
		}
	}

	if req.Autostart != nil {
		if err := applySystemdAutostart(cfg.ServicePath, cfg.ServiceName, *req.Autostart); err != nil {
			return nil, fmt.Errorf("设置开机自启失败: %v", err)
		}
	} else {
		if err := ensureSystemdLink(cfg.ServicePath, cfg.ServiceName); err != nil {
			return nil, err
		}
		if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("systemctl daemon-reload 失败: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	if req.RestartNow {
		if out, err := exec.Command("systemctl", "restart", cfg.ServiceName).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("配置已保存，但重启失败: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	return m.GetContainer(machine)
}

func (m *Manager) GetContainerSpecRaw(machine string) (map[string]interface{}, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	cfg := info.Config
	var spec ContainerSpec
	fromDisk := false
	if cfg.ConfigFile != "" {
		if sp, err := ReadContainerSpec(cfg.ConfigFile); err == nil {
			spec = *sp
			fromDisk = true
		}
	}
	if !fromDisk {
		spec = ConfigToSpec(cfg, nil)
	}
	pretty, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"machine":       cfg.Machine,
		"layout":        cfg.Layout,
		"config_file":   cfg.ConfigFile,
		"exists_on_disk": fromDisk,
		"spec":          spec,
		"raw_json":      string(pretty) + "\n",
	}, nil
}

func (m *Manager) DeleteContainer(machine string, deleteRootfs bool, deleteImages bool) error {
	info, err := m.GetContainer(machine)
	if err != nil {
		return err
	}
	cfg := info.Config
	if cfg.Layout == LayoutExternal {
		return fmt.Errorf("外部容器不支持在本管理器中删除，请先迁移或手动管理")
	}

	_ = exec.Command("systemctl", "disable", "--now", cfg.ServiceName).Run()
	_ = os.Remove(filepath.Join("/etc/systemd/system/multi-user.target.wants", cfg.ServiceName))
	if cfg.SystemdLink != "" {
		_ = os.Remove(cfg.SystemdLink)
	}
	_ = os.Remove(filepath.Join("/etc/systemd/system", cfg.ServiceName))
	_ = exec.Command("systemctl", "daemon-reload").Run()

	if deleteRootfs && cfg.Rootfs != "" {
		if err := exec.Command("btrfs", "subvolume", "delete", cfg.Rootfs).Run(); err != nil {
			_ = os.RemoveAll(cfg.Rootfs)
		}
	}

	if deleteImages {
		if cfg.Layout == LayoutStandard {
			if cfg.SnapshotDir != "" {
				if entries, err := os.ReadDir(cfg.SnapshotDir); err == nil {
					for _, e := range entries {
						sp := filepath.Join(cfg.SnapshotDir, e.Name())
						if err := exec.Command("btrfs", "subvolume", "delete", sp).Run(); err != nil {
							_ = os.RemoveAll(sp)
						}
					}
				}
			}
			if cfg.ImagesDir != "" {
				_ = os.RemoveAll(cfg.ImagesDir)
			}
		} else if cfg.SnapshotDir != "" {
			snaps, _ := filepath.Glob(filepath.Join(cfg.SnapshotDir, cfg.Machine+"-*"))
			for _, sp := range snaps {
				if err := exec.Command("btrfs", "subvolume", "delete", sp).Run(); err != nil {
					_ = os.RemoveAll(sp)
				}
			}
		}
	}

	if cfg.ConfDir != "" {
		_ = os.RemoveAll(cfg.ConfDir)
	}
	if cfg.ContainerDir != "" {
		_ = os.Remove(cfg.ContainerDir) // Only removes if empty
	} else if cfg.Rootfs != "" {
		_ = os.Remove(filepath.Dir(cfg.Rootfs))
	}

	return nil
}

func (m *Manager) GetJournalLogs(machine string, lines int) (string, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return "", err
	}
	if lines <= 0 || lines > 2000 {
		lines = 200
	}
	out, err := exec.Command("journalctl", "-u", info.Config.ServiceName, "-n", strconv.Itoa(lines), "--no-pager", "-o", "short-iso").CombinedOutput()
	if err != nil && len(out) == 0 {
		return "", err
	}
	return string(out), nil
}

func (m *Manager) InspectHostEnvironment() HostEnvironment {
	env := HostEnvironment{
		AppVersion: AppVersion,
		Arch:       runtime.GOARCH,
		CPUCores:   runtime.NumCPU(),
	}
	if memBytes, err := os.ReadFile("/proc/meminfo"); err == nil {
		env.TotalMemoryBytes = parseMemTotalKB(string(memBytes))
	}
	if hn, err := os.Hostname(); err == nil {
		env.Hostname = hn
	}
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		env.Kernel = strings.TrimSpace(string(out))
	}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				env.OSVersion = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), "\"'\r ")
				break
			}
		}
	}

	_, err := exec.LookPath("systemd-nspawn")
	env.HasSystemdNspawn = err == nil
	_, err = exec.LookPath("machinectl")
	env.HasMachinectl = err == nil
	_, err = exec.LookPath("debootstrap")
	env.HasDebootstrap = err == nil
	_, err = exec.LookPath("btrfs")
	env.HasBtrfs = err == nil
	_, err = exec.LookPath("zstd")
	env.HasZstd = err == nil
	ovsPath, err := exec.LookPath("ovs-vsctl")
	env.HasOVS = err == nil

	bridgeSet := make(map[string]bool)
	if env.HasOVS {
		if out, err := exec.Command(ovsPath, "list-br").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					env.OVSBridges = append(env.OVSBridges, line)
					bridgeSet[line] = true
				}
			}
		}
	}

	// List physical/ethernet NICs from /sys/class/net
	if entries, err := os.ReadDir("/sys/class/net"); err == nil {
		for _, e := range entries {
			name := e.Name()
			if name == "lo" || name == "ovs-system" || bridgeSet[name] || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "ve-") || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") {
				continue
			}
			nic := NICInfo{Name: name}
			if b, err := os.ReadFile(filepath.Join("/sys/class/net", name, "operstate")); err == nil {
				nic.OperState = strings.TrimSpace(string(b))
			}
			if b, err := os.ReadFile(filepath.Join("/sys/class/net", name, "address")); err == nil {
				nic.MAC = strings.TrimSpace(string(b))
			}
			if env.HasOVS {
				if brOut, err := exec.Command(ovsPath, "port-to-br", name).Output(); err == nil {
					br := strings.TrimSpace(string(brOut))
					if br != "" {
						nic.OVSBridge = br
						nic.IsOVSPort = true
					}
				}
			}
			env.NICs = append(env.NICs, nic)
		}
	}

	// Discover fnOS storage volumes (/vol1, /vol2, ...)
	if vols, err := filepath.Glob("/vol*"); err == nil {
		for _, v := range vols {
			if st, err := os.Stat(v); err == nil && st.IsDir() {
				env.Volumes = append(env.Volumes, v)
			}
		}
	}

	return env
}

// PreferredOVSNIC returns the best physical NIC currently attached to an OVS bridge.
func (m *Manager) PreferredOVSNIC() string {
	env := m.InspectHostEnvironment()
	for _, n := range env.NICs {
		if n.IsOVSPort && n.OperState == "up" {
			return n.Name
		}
	}
	for _, n := range env.NICs {
		if n.IsOVSPort {
			return n.Name
		}
	}
	return ""
}

// EnsureDefaultOVSNIC aligns default_physical_nic in AppConfig with an active OVS port on startup.
func (m *Manager) EnsureDefaultOVSNIC() {
	env := m.InspectHostEnvironment()
	cfg := m.cfgStore.Get()
	currentValid := false
	var firstUp, firstOVS string
	for _, n := range env.NICs {
		if !n.IsOVSPort {
			continue
		}
		if firstOVS == "" {
			firstOVS = n.Name
		}
		if firstUp == "" && n.OperState == "up" {
			firstUp = n.Name
		}
		if n.Name == cfg.DefaultPhysicalNIC {
			currentValid = true
		}
	}
	if currentValid {
		return
	}
	best := firstUp
	if best == "" {
		best = firstOVS
	}
	if best != "" && best != cfg.DefaultPhysicalNIC {
		cfg.DefaultPhysicalNIC = best
		if err := m.cfgStore.Save(cfg); err != nil {
			log.Printf("自动保存默认 OVS 网卡 (%s) 失败: %v", best, err)
		} else {
			log.Printf("已自动将默认物理网卡设置为已开启 OVS 的网卡: %s", best)
		}
	}
}

func (m *Manager) BrowseFS(reqPath string) (*FSBrowseResult, error) {
	target := strings.TrimSpace(reqPath)
	appCfg := m.cfgStore.Get()
	scanRoots := effectiveScanRoots(appCfg)
	if target == "" {
		if len(scanRoots) > 0 {
			if _, err := os.Stat(scanRoots[0]); err == nil {
				target = scanRoots[0]
			}
		}
		if target == "" {
			target = "/"
		}
	}
	target = filepath.Clean(target)
	st, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("无法访问路径 %s: %v", target, err)
	}
	if !st.IsDir() {
		target = filepath.Dir(target)
	}

	dirEntries, err := os.ReadDir(target)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败 %s: %v", target, err)
	}

	var dirs []FSEntry
	var files []FSEntry
	for _, de := range dirEntries {
		name := de.Name()
		fullPath := filepath.Join(target, name)
		isDir := de.IsDir()
		if de.Type()&os.ModeSymlink != 0 {
			if lst, err := os.Stat(fullPath); err == nil {
				isDir = lst.IsDir()
			}
		}
		if isDir {
			dirs = append(dirs, FSEntry{
				Name:  name,
				Path:  fullPath,
				IsDir: true,
			})
			continue
		}
		lower := strings.ToLower(name)
		if name == ContainerSpecFileName ||
			(strings.HasPrefix(name, "nspawn-") && strings.HasSuffix(name, ".service")) ||
			strings.HasSuffix(lower, ".tar") ||
			strings.HasSuffix(lower, ".tar.gz") ||
			strings.HasSuffix(lower, ".tgz") ||
			strings.HasSuffix(lower, ".tar.zst") ||
			strings.HasSuffix(lower, ".tar.xz") {
			var size int64
			if fi, err := de.Info(); err == nil {
				size = fi.Size()
			}
			files = append(files, FSEntry{
				Name:  name,
				Path:  fullPath,
				IsDir: false,
				Size:  size,
			})
		}
	}

	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	bookmarks := []string{"/"}
	seenBM := map[string]bool{"/": true}
	for _, r := range scanRoots {
		cr := filepath.Clean(r)
		if !seenBM[cr] {
			if _, err := os.Stat(cr); err == nil {
				bookmarks = append(bookmarks, cr)
				seenBM[cr] = true
			}
		}
	}
	if vols, err := filepath.Glob("/vol*"); err == nil {
		for _, v := range vols {
			cv := filepath.Clean(v)
			if !seenBM[cv] {
				if st, err := os.Stat(cv); err == nil && st.IsDir() {
					bookmarks = append(bookmarks, cv)
					seenBM[cv] = true
				}
			}
		}
	}

	parent := filepath.Dir(target)
	if parent == target {
		parent = ""
	}
	return &FSBrowseResult{
		Path:      target,
		Parent:    parent,
		Bookmarks: bookmarks,
		Entries:   append(dirs, files...),
	}, nil
}

func IsBtrfsPath(path string) bool {
	out, err := exec.Command("stat", "-f", "-c", "%T", path).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "btrfs"
}

func (m *Manager) ReconcileOnStartup() {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return
	}
	time.Sleep(2 * time.Second)
	list, err := m.DiscoverContainers()
	if err != nil {
		log.Printf("[reconcile] 扫描容器失败: %v", err)
		return
	}
	for _, item := range list {
		cfg := item.Config
		if cfg.Layout == LayoutExternal {
			continue
		}
		rt := item.Runtime
		wantAuto := cfg.Autostart || rt.Enabled

		if (!cfg.LinkHealthy || rt.LoadState == "not-found") && fileExists(cfg.ServicePath) {
			if cfg.Layout == LayoutStandard && cfg.HasConfigFile {
				if err := syncStandardUnitFiles(cfg); err != nil {
					log.Printf("[reconcile] 更新 %s 标准配置脚本失败: %v", cfg.Machine, err)
				}
			}
			if err := applySystemdAutostart(cfg.ServicePath, cfg.ServiceName, wantAuto); err != nil {
				log.Printf("[reconcile] 同步 %s systemd 单元失败: %v", cfg.Machine, err)
				continue
			}
			log.Printf("[reconcile] 已将 %s 同步为实体 systemd 单元 (autostart=%v)", cfg.ServiceName, wantAuto)
		}

		if wantAuto && rt.ActiveState != "active" && rt.ActiveState != "activating" && rt.ExecStartMonotonic == 0 && fileExists(cfg.ServicePath) {
			go func(c ContainerConfig) {
				ready := false
				for i := 0; i < 90; i++ {
					rootfsReady := false
					if st, err := os.Stat(filepath.Join(c.Rootfs, "etc")); err == nil && st.IsDir() {
						rootfsReady = true
					}
					ovsReady := exec.Command("ovs-vsctl", "port-to-br", c.PhysicalNIC).Run() == nil
					if rootfsReady && ovsReady {
						ready = true
						break
					}
					time.Sleep(2 * time.Second)
				}
				if !ready {
					log.Printf("[reconcile] %s 开机等待 rootfs(%s) 或 OVS(%s) 就绪超时", c.Machine, c.Rootfs, c.PhysicalNIC)
					return
				}
				if exec.Command("systemctl", "is-active", "--quiet", c.ServiceName).Run() == nil {
					return
				}
				if out, err := exec.Command("systemctl", "start", c.ServiceName).CombinedOutput(); err != nil {
					log.Printf("[reconcile] %s 开机兜底启动失败: %v (%s)", c.Machine, err, strings.TrimSpace(string(out)))
				} else {
					log.Printf("[reconcile] %s 已通过开机兜底巡检拉起", c.Machine)
				}
			}(cfg)
		}
	}
}

func runCmdLogged(logFn func(string), name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	err := cmd.Run()
	if s := strings.TrimSpace(outBuf.String()); s != "" {
		for _, line := range strings.Split(s, "\n") {
			logFn(line)
		}
	}
	return err
}
