package nspawn

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ContainerSpecSchemaVersion = 1
	ContainerSpecFileName      = "container.json"
	LayoutStandard             = "standard"
	LayoutLegacy               = "legacy"
	LayoutExternal             = "external"
	InitModeSystemd            = "systemd"
	InitModeOpenWrt            = "openwrt"
)

type ContainerSpec struct {
	SchemaVersion int           `json:"schema_version"`
	Machine       string        `json:"machine"`
	Description   string        `json:"description,omitempty"`
	CreatedAt     string        `json:"created_at"`
	UpdatedAt     string        `json:"updated_at"`
	Layout        SpecLayout    `json:"layout"`
	System        SpecSystem    `json:"system"`
	Image         SpecImage     `json:"image"`
	Network       SpecNetwork   `json:"network"`
	Resources     SpecResources `json:"resources"`
	BindMounts    []BindMount   `json:"bind_mounts"`
	Backup        *SpecBackup   `json:"backup,omitempty"`
}

type SnapshotScheduleSpec struct {
	Enabled   bool   `json:"enabled"`
	Cron      string `json:"cron"`
	Tag       string `json:"tag,omitempty"`
	ReadOnly  bool   `json:"read_only"`
	StopFirst bool   `json:"stop_first,omitempty"`
	KeepCount int    `json:"keep_count,omitempty"`
	KeepDays  int    `json:"keep_days,omitempty"`
}

type ExportScheduleSpec struct {
	Enabled   bool   `json:"enabled"`
	Cron      string `json:"cron"`
	Tag       string `json:"tag,omitempty"`
	Format    string `json:"format,omitempty"`
	StopFirst bool   `json:"stop_first,omitempty"`
	KeepCount int    `json:"keep_count,omitempty"`
	KeepDays  int    `json:"keep_days,omitempty"`
}

type SpecBackup struct {
	Snapshot *SnapshotScheduleSpec `json:"snapshot,omitempty"`
	Export   *ExportScheduleSpec   `json:"export,omitempty"`
}

func cloneSpecBackup(b *SpecBackup) *SpecBackup {
	if b == nil {
		return nil
	}
	out := &SpecBackup{}
	if b.Snapshot != nil {
		s := *b.Snapshot
		out.Snapshot = &s
	}
	if b.Export != nil {
		e := *b.Export
		out.Export = &e
	}
	return out
}

type SpecLayout struct {
	RootfsDir    string `json:"rootfs_dir"`
	ConfigDir    string `json:"config_dir"`
	ImagesDir    string `json:"images_dir"`
	SnapshotsDir string `json:"snapshots_dir"`
	ExportsDir   string `json:"exports_dir"`
}

type SpecSystem struct {
	ContainerUser string `json:"container_user,omitempty"`
	Hostname      string `json:"hostname"`
	Autostart     bool   `json:"autostart"`
	ServiceName   string `json:"service_name"`
	InitMode      string `json:"init_mode,omitempty"`
	EnableTun     bool   `json:"enable_tun,omitempty"`
}

type SpecImage struct {
	SourceType     string `json:"source_type"`
	Distro         string `json:"distro,omitempty"`
	Suite          string `json:"suite,omitempty"`
	Arch           string `json:"arch,omitempty"`
	Mirror         string `json:"mirror,omitempty"`
	SecurityMirror string `json:"security_mirror,omitempty"`
	SourceArchive  string `json:"source_archive,omitempty"`
}

type SpecNetwork struct {
	Mode        string `json:"mode"`
	PhysicalNIC string `json:"physical_nic"`
	Veth        string `json:"veth"`
	DHCP        bool   `json:"dhcp"`
}

type SpecResources struct {
	TmpSize   string `json:"tmp_size"`
	MemoryMax string `json:"memory_max,omitempty"`
	CPUQuota  string `json:"cpu_quota,omitempty"`
	TasksMax  string `json:"tasks_max"`
}

type StandardPaths struct {
	BaseRoot     string
	ContainerDir string
	Rootfs       string
	ConfDir      string
	ConfigFile   string
	ServiceName  string
	ServicePath  string
	ImagesDir    string
	SnapshotDir  string
	ExportsDir   string
}

func BuildStandardPaths(baseRoot, machine string) StandardPaths {
	baseRoot = filepath.Clean(baseRoot)
	containerDir := filepath.Join(baseRoot, machine)
	confDir := filepath.Join(containerDir, "config")
	imagesDir := filepath.Join(containerDir, "images")
	svcName := fmt.Sprintf("nspawn-%s.service", machine)
	return StandardPaths{
		BaseRoot:     baseRoot,
		ContainerDir: containerDir,
		Rootfs:       filepath.Join(containerDir, "rootfs"),
		ConfDir:      confDir,
		ConfigFile:   filepath.Join(confDir, ContainerSpecFileName),
		ServiceName:  svcName,
		ServicePath:  filepath.Join(confDir, svcName),
		ImagesDir:    imagesDir,
		SnapshotDir:  filepath.Join(imagesDir, "snapshots"),
		ExportsDir:   filepath.Join(imagesDir, "exports"),
	}
}

func DefaultSpecLayout() SpecLayout {
	return SpecLayout{
		RootfsDir:    "rootfs",
		ConfigDir:    "config",
		ImagesDir:    "images",
		SnapshotsDir: "images/snapshots",
		ExportsDir:   "images/exports",
	}
}

func NormalizeSpec(spec *ContainerSpec) {
	if spec.SchemaVersion <= 0 {
		spec.SchemaVersion = ContainerSpecSchemaVersion
	}
	if spec.Layout.RootfsDir == "" {
		spec.Layout.RootfsDir = "rootfs"
	}
	if spec.Layout.ConfigDir == "" {
		spec.Layout.ConfigDir = "config"
	}
	if spec.Layout.ImagesDir == "" {
		spec.Layout.ImagesDir = "images"
	}
	if spec.Layout.SnapshotsDir == "" {
		spec.Layout.SnapshotsDir = "images/snapshots"
	}
	if spec.Layout.ExportsDir == "" {
		spec.Layout.ExportsDir = "images/exports"
	}
	if spec.System.Hostname == "" && spec.Machine != "" {
		spec.System.Hostname = spec.Machine
	}
	if spec.System.ServiceName == "" && spec.Machine != "" {
		spec.System.ServiceName = fmt.Sprintf("nspawn-%s.service", spec.Machine)
	}
	if spec.Network.Mode == "" {
		spec.Network.Mode = "ovs-veth"
	}
	if spec.Network.Veth == "" && spec.Machine != "" {
		spec.Network.Veth = "ve-" + spec.Machine
	}
	if spec.Network.PhysicalNIC == "" {
		spec.Network.PhysicalNIC = "enp2s0"
	}
	if spec.Resources.TmpSize == "" {
		spec.Resources.TmpSize = "4G"
	}
	if spec.Resources.TasksMax == "" {
		spec.Resources.TasksMax = "infinity"
	}
	if spec.Image.SourceType == "" {
		spec.Image.SourceType = "debootstrap"
	}
	if spec.BindMounts == nil {
		spec.BindMounts = []BindMount{}
	}
	now := time.Now().Format(time.RFC3339)
	if spec.CreatedAt == "" {
		spec.CreatedAt = now
	}
	if spec.UpdatedAt == "" {
		spec.UpdatedAt = now
	}
}

func ReadContainerSpec(specPath string) (*ContainerSpec, error) {
	data, err := os.ReadFile(specPath)
	if err != nil {
		return nil, err
	}
	var spec ContainerSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("解析容器配置文件失败 (%s): %w", specPath, err)
	}
	if strings.TrimSpace(spec.Machine) == "" {
		return nil, fmt.Errorf("容器配置文件缺少 machine 字段: %s", specPath)
	}
	NormalizeSpec(&spec)
	return &spec, nil
}

func WriteContainerSpec(specPath string, spec ContainerSpec) error {
	NormalizeSpec(&spec)
	spec.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := os.MkdirAll(filepath.Dir(specPath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := specPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, specPath)
}

func resolveSpecPath(containerDir, relOrAbs, fallbackRel string) string {
	val := strings.TrimSpace(relOrAbs)
	if val == "" {
		val = fallbackRel
	}
	if filepath.IsAbs(val) {
		return filepath.Clean(val)
	}
	return filepath.Clean(filepath.Join(containerDir, filepath.FromSlash(val)))
}

func isSystemdUnitSynced(systemdUnitPath, sourceServicePath string) bool {
	if strings.TrimSpace(systemdUnitPath) == "" || strings.TrimSpace(sourceServicePath) == "" {
		return false
	}
	st, err := os.Lstat(systemdUnitPath)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	sysBytes, err := os.ReadFile(systemdUnitPath)
	if err != nil {
		return false
	}
	srcBytes, err := os.ReadFile(sourceServicePath)
	if err != nil || len(srcBytes) == 0 {
		return false
	}
	return bytes.Equal(sysBytes, srcBytes)
}

func SpecToConfig(spec ContainerSpec, containerDir string, systemdLinkPath string) *ContainerConfig {
	NormalizeSpec(&spec)
	containerDir = filepath.Clean(containerDir)
	confDir := resolveSpecPath(containerDir, spec.Layout.ConfigDir, "config")
	rootfs := resolveSpecPath(containerDir, spec.Layout.RootfsDir, "rootfs")
	imagesDir := resolveSpecPath(containerDir, spec.Layout.ImagesDir, "images")
	snapshotDir := resolveSpecPath(containerDir, spec.Layout.SnapshotsDir, "images/snapshots")
	exportsDir := resolveSpecPath(containerDir, spec.Layout.ExportsDir, "images/exports")

	svcName := spec.System.ServiceName
	if svcName == "" {
		svcName = fmt.Sprintf("nspawn-%s.service", spec.Machine)
	}
	svcPath := filepath.Join(confDir, svcName)
	configFile := filepath.Join(confDir, ContainerSpecFileName)

	cfg := &ContainerConfig{
		Machine:        spec.Machine,
		Description:    spec.Description,
		Layout:         LayoutStandard,
		ServiceName:    svcName,
		ServicePath:    svcPath,
		SystemdLink:    systemdLinkPath,
		BaseRoot:       filepath.Dir(containerDir),
		ContainerDir:   containerDir,
		ConfDir:        confDir,
		ConfigFile:     configFile,
		HasConfigFile:  true,
		Rootfs:         rootfs,
		ImagesDir:      imagesDir,
		SnapshotDir:    snapshotDir,
		ExportsDir:     exportsDir,
		TmpSize:        spec.Resources.TmpSize,
		PhysicalNIC:    spec.Network.PhysicalNIC,
		Veth:           spec.Network.Veth,
		BindMounts:     append([]BindMount(nil), spec.BindMounts...),
		MemoryMax:      spec.Resources.MemoryMax,
		CPUQuota:       spec.Resources.CPUQuota,
		TasksMax:       spec.Resources.TasksMax,
		Autostart:      spec.System.Autostart,
		ContainerUser:  spec.System.ContainerUser,
		InitMode:       spec.System.InitMode,
		EnableTun:      spec.System.EnableTun,
		SourceType:     spec.Image.SourceType,
		Distro:         spec.Image.Distro,
		Suite:          spec.Image.Suite,
		Arch:           spec.Image.Arch,
		Mirror:         spec.Image.Mirror,
		SecurityMirror: spec.Image.SecurityMirror,
		SourceArchive:  spec.Image.SourceArchive,
		Backup:         cloneSpecBackup(spec.Backup),
	}

	cfg.LinkHealthy = isSystemdUnitSynced(systemdLinkPath, svcPath)

	if _, err := os.Stat(filepath.Join(confDir, "network-up.sh")); err == nil {
		cfg.HasNetUp = true
	}
	if _, err := os.Stat(filepath.Join(confDir, "network-down.sh")); err == nil {
		cfg.HasNetDown = true
	}
	if _, err := os.Stat(filepath.Join(confDir, "restore.sh")); err == nil {
		cfg.HasRestore = true
	}
	return cfg
}

func ConfigToSpec(cfg ContainerConfig, existing *ContainerSpec) ContainerSpec {
	var spec ContainerSpec
	if existing != nil {
		spec = *existing
	} else {
		spec = ContainerSpec{
			SchemaVersion: ContainerSpecSchemaVersion,
			Layout:        DefaultSpecLayout(),
			Network: SpecNetwork{
				Mode: "ovs-veth",
				DHCP: true,
			},
			Image: SpecImage{
				SourceType: "existing",
			},
		}
	}

	spec.Machine = cfg.Machine
	spec.Description = cfg.Description
	spec.Layout = DefaultSpecLayout()
	spec.System.Hostname = cfg.Machine
	spec.System.ServiceName = cfg.ServiceName
	spec.System.Autostart = cfg.Autostart
	if cfg.ContainerUser != "" {
		spec.System.ContainerUser = cfg.ContainerUser
	}
	if cfg.InitMode != "" {
		spec.System.InitMode = cfg.InitMode
	}
	if cfg.EnableTun {
		spec.System.EnableTun = true
	}
	if cfg.SourceType != "" {
		spec.Image.SourceType = cfg.SourceType
	}
	if cfg.Distro != "" {
		spec.Image.Distro = cfg.Distro
	}
	if cfg.Suite != "" {
		spec.Image.Suite = cfg.Suite
	}
	if cfg.Arch != "" {
		spec.Image.Arch = cfg.Arch
	}
	if cfg.Mirror != "" {
		spec.Image.Mirror = cfg.Mirror
	}
	if cfg.SecurityMirror != "" {
		spec.Image.SecurityMirror = cfg.SecurityMirror
	}
	if cfg.SourceArchive != "" {
		spec.Image.SourceArchive = cfg.SourceArchive
	}
	spec.Network.Mode = "ovs-veth"
	spec.Network.PhysicalNIC = cfg.PhysicalNIC
	spec.Network.Veth = cfg.Veth
	spec.Network.DHCP = true
	spec.Resources.TmpSize = cfg.TmpSize
	spec.Resources.MemoryMax = cfg.MemoryMax
	spec.Resources.CPUQuota = cfg.CPUQuota
	spec.Resources.TasksMax = cfg.TasksMax
	spec.BindMounts = append([]BindMount(nil), cfg.BindMounts...)
	if cfg.Backup != nil {
		spec.Backup = cloneSpecBackup(cfg.Backup)
	}
	NormalizeSpec(&spec)
	return spec
}
