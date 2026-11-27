package nspawn

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

var (
	rePhysicalNIC = regexp.MustCompile(`(?m)^\s*PHYSICAL_NIC=["']?([^"'\s#]+)["']?`)
	reVeth        = regexp.MustCompile(`(?m)^\s*VETH=["']?([^"'\s#]+)["']?`)
	reTmpSize     = regexp.MustCompile(`size=([^,\s]+)`)
)

func ParseServiceFile(servicePath string, systemdLinkPath string) (*ContainerConfig, error) {
	rawBytes, err := os.ReadFile(servicePath)
	if err != nil {
		return nil, err
	}
	raw := string(rawBytes)

	cfg := &ContainerConfig{
		ServiceName: filepath.Base(servicePath),
		ServicePath: servicePath,
		SystemdLink: systemdLinkPath,
		TmpSize:     "4G",
		PhysicalNIC: "enp2s0",
		TasksMax:    "infinity",
		Autostart:   true,
	}

	lines := joinContinuationLines(raw)
	var execStart string
	var execStartPost string
	var execStopPost string
	var declaredSource string

	section := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# fn-nspawn-source=") {
			declaredSource = strings.TrimSpace(strings.TrimPrefix(trimmed, "# fn-nspawn-source="))
			continue
		}
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
		case "ExecStopPost":
			execStopPost = strings.TrimPrefix(val, "-")
		case "MemoryMax":
			cfg.MemoryMax = val
		case "CPUQuota":
			cfg.CPUQuota = val
		case "TasksMax":
			cfg.TasksMax = val
		}
	}

	parseExecStartArgs(execStart, cfg)

	if cfg.Machine == "" {
		name := strings.TrimSuffix(cfg.ServiceName, ".service")
		name = strings.TrimPrefix(name, "nspawn-")
		cfg.Machine = name
	}
	if cfg.Veth == "" && cfg.Machine != "" {
		cfg.Veth = "ve-" + cfg.Machine
	}

	sourceServicePath := filepath.Clean(servicePath)
	if declaredSource != "" && filepath.IsAbs(declaredSource) {
		sourceServicePath = filepath.Clean(declaredSource)
	} else if filepath.Base(filepath.Dir(sourceServicePath)) == "system" && execStartPost != "" {
		postFields := strings.Fields(execStartPost)
		if len(postFields) > 0 && filepath.IsAbs(postFields[0]) {
			inferredConf := filepath.Dir(postFields[0])
			sourceServicePath = filepath.Join(inferredConf, cfg.ServiceName)
		}
	}
	cfg.ServicePath = sourceServicePath
	cfg.LinkHealthy = isSystemdUnitSynced(systemdLinkPath, cfg.ServicePath)

	cfg.ConfDir = filepath.Dir(cfg.ServicePath)
	if filepath.Base(cfg.ConfDir) == "config" {
		// Standardized self-contained layout: <BaseRoot>/<Machine>/{rootfs,config,images}
		cfg.Layout = LayoutStandard
		cfg.ContainerDir = filepath.Dir(cfg.ConfDir)
		cfg.BaseRoot = filepath.Dir(cfg.ContainerDir)
		cfg.ImagesDir = filepath.Join(cfg.ContainerDir, "images")
		cfg.SnapshotDir = filepath.Join(cfg.ImagesDir, "snapshots")
		cfg.ExportsDir = filepath.Join(cfg.ImagesDir, "exports")
		cfg.ConfigFile = filepath.Join(cfg.ConfDir, ContainerSpecFileName)

		if spec, err := ReadContainerSpec(cfg.ConfigFile); err == nil {
			cfg.HasConfigFile = true
			cfg.Description = spec.Description
			cfg.Autostart = spec.System.Autostart
			cfg.ContainerUser = spec.System.ContainerUser
			cfg.SourceType = spec.Image.SourceType
			cfg.Distro = spec.Image.Distro
			cfg.Suite = spec.Image.Suite
			cfg.Arch = spec.Image.Arch
			cfg.Mirror = spec.Image.Mirror
			cfg.SecurityMirror = spec.Image.SecurityMirror
			cfg.SourceArchive = spec.Image.SourceArchive
		}
	} else {
		// Legacy layout: <BaseRoot>/<Machine>/rootfs + <BaseRoot>/<Machine>-conf/
		cfg.Layout = LayoutLegacy
		if cfg.Rootfs != "" {
			machineDir := filepath.Dir(filepath.Clean(cfg.Rootfs))
			cfg.ContainerDir = machineDir
			cfg.BaseRoot = filepath.Dir(machineDir)
		} else {
			cfg.BaseRoot = filepath.Dir(cfg.ConfDir)
			cfg.ContainerDir = filepath.Join(cfg.BaseRoot, cfg.Machine)
		}
		cfg.SnapshotDir = filepath.Join(cfg.BaseRoot, "snapshots")
	}

	if execStartPost != "" {
		postScript := strings.Fields(execStartPost)[0]
		if _, err := os.Stat(postScript); err == nil {
			cfg.HasNetUp = true
			if b, err := os.ReadFile(postScript); err == nil {
				if m := rePhysicalNIC.FindStringSubmatch(string(b)); len(m) > 1 {
					cfg.PhysicalNIC = m[1]
				}
				if m := reVeth.FindStringSubmatch(string(b)); len(m) > 1 {
					cfg.Veth = m[1]
				}
			}
		}
	} else {
		netUp := filepath.Join(cfg.ConfDir, "network-up.sh")
		if b, err := os.ReadFile(netUp); err == nil {
			cfg.HasNetUp = true
			if m := rePhysicalNIC.FindStringSubmatch(string(b)); len(m) > 1 {
				cfg.PhysicalNIC = m[1]
			}
			if m := reVeth.FindStringSubmatch(string(b)); len(m) > 1 {
				cfg.Veth = m[1]
			}
		}
	}

	if execStopPost != "" {
		stopScript := strings.Fields(execStopPost)[0]
		if _, err := os.Stat(stopScript); err == nil {
			cfg.HasNetDown = true
		}
	} else if _, err := os.Stat(filepath.Join(cfg.ConfDir, "network-down.sh")); err == nil {
		cfg.HasNetDown = true
	}

	if _, err := os.Stat(filepath.Join(cfg.ConfDir, "restore.sh")); err == nil {
		cfg.HasRestore = true
	}

	return cfg, nil
}

func joinContinuationLines(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	rawLines := strings.Split(raw, "\n")
	var out []string
	var buf strings.Builder
	for _, l := range rawLines {
		if strings.HasSuffix(l, "\\") {
			buf.WriteString(strings.TrimSuffix(l, "\\"))
			buf.WriteByte(' ')
			continue
		}
		if buf.Len() > 0 {
			buf.WriteString(l)
			out = append(out, buf.String())
			buf.Reset()
		} else {
			out = append(out, l)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

func splitShellArgs(s string) []string {
	var args []string
	var cur strings.Builder
	inToken := false
	var quote rune

	for _, r := range s {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			inToken = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			inToken = true
			continue
		}
		if unicode.IsSpace(r) {
			if inToken {
				args = append(args, cur.String())
				cur.Reset()
				inToken = false
			}
			continue
		}
		cur.WriteRune(r)
		inToken = true
	}
	if inToken {
		args = append(args, cur.String())
	}
	return args
}

func parseExecStartArgs(execStart string, cfg *ContainerConfig) {
	args := splitShellArgs(execStart)
	for i := 0; i < len(args); i++ {
		a := args[i]
		nextVal := func(flag string) (string, bool) {
			if strings.HasPrefix(a, flag+"=") {
				return strings.TrimPrefix(a, flag+"="), true
			}
			if a == flag && i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}

		if v, ok := nextVal("--machine"); ok {
			cfg.Machine = v
			continue
		}
		if v, ok := nextVal("-M"); ok {
			cfg.Machine = v
			continue
		}
		if v, ok := nextVal("--directory"); ok {
			cfg.Rootfs = v
			continue
		}
		if v, ok := nextVal("-D"); ok {
			cfg.Rootfs = v
			continue
		}
		if v, ok := nextVal("--tmpfs"); ok {
			if strings.HasPrefix(v, "/tmp:") {
				if m := reTmpSize.FindStringSubmatch(v); len(m) > 1 {
					cfg.TmpSize = m[1]
				}
			}
			continue
		}
		if v, ok := nextVal("--bind"); ok {
			if bm, ok := parseBindValue(v, false); ok {
				cfg.BindMounts = append(cfg.BindMounts, bm)
			}
			continue
		}
		if v, ok := nextVal("--bind-ro"); ok {
			if bm, ok := parseBindValue(v, true); ok {
				cfg.BindMounts = append(cfg.BindMounts, bm)
			}
			continue
		}
	}
}

func parseBindValue(v string, readOnly bool) (BindMount, bool) {
	parts := strings.SplitN(v, ":", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return BindMount{}, false
	}
	return BindMount{
		HostPath:      parts[0],
		ContainerPath: parts[1],
		ReadOnly:      readOnly,
	}, true
}
