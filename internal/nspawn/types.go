package nspawn

import "time"

type BindMount struct {
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"`
	ReadOnly      bool   `json:"read_only,omitempty"`
}

type ContainerConfig struct {
	Machine              string      `json:"machine"`
	Description          string      `json:"description,omitempty"`
	Layout               string      `json:"layout"` // standard | legacy | external
	InStandardRoot       bool        `json:"in_standard_root"`
	CanMigrateToStandard bool        `json:"can_migrate_to_standard"`
	ServiceName          string      `json:"service_name"`
	ServicePath    string      `json:"service_path"`
	SystemdLink    string      `json:"systemd_link"`
	LinkHealthy    bool        `json:"link_healthy"`
	BaseRoot       string      `json:"base_root"`
	ContainerDir   string      `json:"container_dir"`
	ConfDir        string      `json:"conf_dir"`
	ConfigFile     string      `json:"config_file"`
	HasConfigFile  bool        `json:"has_config_file"`
	Rootfs         string      `json:"rootfs"`
	ImagesDir      string      `json:"images_dir"`
	SnapshotDir    string      `json:"snapshot_dir"`
	ExportsDir     string      `json:"exports_dir"`
	TmpSize        string      `json:"tmp_size"`
	PhysicalNIC    string      `json:"physical_nic"`
	Veth           string      `json:"veth"`
	BindMounts     []BindMount `json:"bind_mounts"`
	MemoryMax      string      `json:"memory_max"`
	CPUQuota       string      `json:"cpu_quota"`
	TasksMax       string      `json:"tasks_max"`
	Autostart      bool        `json:"autostart"`
	ContainerUser  string      `json:"container_user,omitempty"`
	InitMode       string      `json:"init_mode,omitempty"`
	EnableTun      bool        `json:"enable_tun,omitempty"`
	SourceType     string      `json:"source_type,omitempty"`
	Distro         string      `json:"distro,omitempty"`
	Suite          string      `json:"suite,omitempty"`
	Arch           string      `json:"arch,omitempty"`
	Mirror         string      `json:"mirror,omitempty"`
	SecurityMirror string      `json:"security_mirror,omitempty"`
	SourceArchive  string      `json:"source_archive,omitempty"`
	HasRestore     bool        `json:"has_restore"`
	HasNetUp       bool        `json:"has_net_up"`
	HasNetDown     bool        `json:"has_net_down"`
	Backup         *SpecBackup `json:"backup,omitempty"`
}

type ContainerRuntime struct {
	ActiveState        string   `json:"active_state"`
	SubState           string   `json:"sub_state"`
	LoadState          string   `json:"load_state,omitempty"`
	Enabled            bool     `json:"enabled"`
	UnitFileState      string   `json:"unit_file_state"`
	MainPID            int      `json:"main_pid"`
	LeaderPID          int      `json:"leader_pid"`
	MemoryCurrent      uint64   `json:"memory_current"`
	MemoryLimitBytes   uint64   `json:"memory_limit_bytes"`
	CPUUsageNSec       uint64   `json:"cpu_usage_nsec"`
	StartedAt          string   `json:"started_at"`
	UptimeSec          int64    `json:"uptime_sec"`
	ExecStartMonotonic uint64   `json:"-"`
	MachinectlUp     bool     `json:"machinectl_up"`
	OSPrettyName     string   `json:"os_pretty_name"`
	RootfsIsBtrfs    bool     `json:"rootfs_is_btrfs"`
	RootfsExists     bool     `json:"rootfs_exists"`
	OVSBridge        string   `json:"ovs_bridge"`
	VethAttached     bool     `json:"veth_attached"`
	Host0MAC         string   `json:"host0_mac"`
	IPv4Addrs        []string `json:"ipv4_addrs"`
	IPv6Addrs        []string `json:"ipv6_addrs"`
	DefaultGateway   string   `json:"default_gateway"`
	NetRxBytes       uint64   `json:"net_rx_bytes"`
	NetTxBytes       uint64   `json:"net_tx_bytes"`
}

type ContainerInfo struct {
	Config  ContainerConfig  `json:"config"`
	Runtime ContainerRuntime `json:"runtime"`
}

type SnapshotInfo struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	ReadOnly   bool   `json:"read_only"`
	CreatedAt  string `json:"created_at"`
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parent_uuid"`
}

type ImageArchiveInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Format    string `json:"format"`
	CreatedAt string `json:"created_at"`
}

type NICInfo struct {
	Name      string `json:"name"`
	OperState string `json:"oper_state"`
	MAC       string `json:"mac"`
	OVSBridge string `json:"ovs_bridge"`
	IsOVSPort bool   `json:"is_ovs_port"`
}

type HostEnvironment struct {
	AppVersion       string    `json:"app_version"`
	Hostname         string    `json:"hostname"`
	OSVersion        string    `json:"os_version"`
	Kernel           string    `json:"kernel"`
	Arch             string    `json:"arch"`
	CPUCores         int       `json:"cpu_cores"`
	TotalMemoryBytes uint64    `json:"total_memory_bytes"`
	HasSystemdNspawn bool      `json:"has_systemd_nspawn"`
	HasMachinectl    bool      `json:"has_machinectl"`
	HasDebootstrap   bool      `json:"has_debootstrap"`
	HasBtrfs         bool      `json:"has_btrfs"`
	HasOVS           bool      `json:"has_ovs"`
	HasZstd          bool      `json:"has_zstd"`
	NICs             []NICInfo `json:"nics"`
	OVSBridges       []string  `json:"ovs_bridges"`
	Volumes          []string  `json:"volumes"`
}

type UpdateConfigRequest struct {
	Description string      `json:"description,omitempty"`
	TmpSize     string      `json:"tmp_size"`
	PhysicalNIC string      `json:"physical_nic"`
	MemoryMax   string      `json:"memory_max"`
	CPUQuota    string      `json:"cpu_quota"`
	BindMounts  []BindMount `json:"bind_mounts"`
	Autostart   *bool       `json:"autostart,omitempty"`
	RestartNow  bool        `json:"restart_now"`
}

type CreateContainerRequest struct {
	Machine        string      `json:"machine"`
	Description    string      `json:"description,omitempty"`
	ContainerUser  string      `json:"container_user"`
	Password       string      `json:"password"`
	BaseRoot       string      `json:"base_root"`
	PhysicalNIC    string      `json:"physical_nic"`
	TmpSize        string      `json:"tmp_size"`
	MemoryMax      string      `json:"memory_max,omitempty"`
	CPUQuota       string      `json:"cpu_quota,omitempty"`
	SourceType          string      `json:"source_type,omitempty"` // online_image | debootstrap | archive
	SourceArchive       string      `json:"source_archive,omitempty"`
	OnlineSource        string      `json:"online_source,omitempty"`
	OnlineImageURL      string      `json:"online_image_url,omitempty"`
	OnlineImageSHA256   string      `json:"online_image_sha256,omitempty"`
	OnlineImageFilename string      `json:"online_image_filename,omitempty"`
	OnlineRelease       string      `json:"online_release,omitempty"`
	Distro              string      `json:"distro"`          // debian | ubuntu | ...
	Suite          string      `json:"suite"`           // trixie, bookworm, noble, jammy
	Arch           string      `json:"arch"`            // amd64 | arm64
	Mirror         string      `json:"mirror"`          // e.g. https://mirrors.ustc.edu.cn/debian
	SecurityMirror string      `json:"security_mirror"` // e.g. https://mirrors.ustc.edu.cn/debian-security
	BindMounts     []BindMount `json:"bind_mounts"`
	InstallExtras  bool        `json:"install_extras"`
	Autostart      *bool       `json:"autostart,omitempty"`
	AutoStart      bool        `json:"auto_start"`
}

type CreateSnapshotRequest struct {
	Tag       string `json:"tag"`
	ReadOnly  bool   `json:"read_only"`
	StopFirst bool   `json:"stop_first"`
}

type RollbackRequest struct {
	SnapshotName string `json:"snapshot_name"`
	SafetyBackup bool   `json:"safety_backup"`
}

type ExportImageRequest struct {
	Tag            string `json:"tag"`
	Format         string `json:"format"` // tar.gz | tar.zst
	SourceSnapshot string `json:"source_snapshot,omitempty"`
	StopFirst      bool   `json:"stop_first"`
}

type ImportPreviewRequest struct {
	ConfigPath string `json:"config_path"`
}

type ImportPreviewResponse struct {
	ResolvedConfigPath string        `json:"resolved_config_path"`
	ConfigType         string        `json:"config_type"` // container_json | service_unit
	ContainerDir       string        `json:"container_dir"`
	BaseRoot           string        `json:"base_root"`
	RootfsPath         string        `json:"rootfs_path"`
	RootfsExists       bool          `json:"rootfs_exists"`
	RootfsIsBtrfs      bool          `json:"rootfs_is_btrfs"`
	AlreadyRegistered  bool          `json:"already_registered"`
	Spec               ContainerSpec `json:"spec"`
}

type ImportContainerRequest struct {
	ConfigPath     string `json:"config_path"`
	PhysicalNIC    string `json:"physical_nic,omitempty"`
	ApplyAutostart *bool  `json:"apply_autostart,omitempty"`
	StartNow       bool   `json:"start_now"`
}

type MigrateContainerRequest struct {
	TargetMachine  string `json:"target_machine,omitempty"`
	TargetBaseRoot string `json:"target_base_root,omitempty"`
	PhysicalNIC    string `json:"physical_nic"`
	Autostart      *bool  `json:"autostart,omitempty"`
	StartNow       bool   `json:"start_now"`
}

type FSEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
}

type FSBrowseResult struct {
	Path      string    `json:"path"`
	Parent    string    `json:"parent"`
	Bookmarks []string  `json:"bookmarks"`
	Entries   []FSEntry `json:"entries"`
}

type TaskStatus string

const (
	TaskRunning TaskStatus = "running"
	TaskSuccess TaskStatus = "success"
	TaskFailed  TaskStatus = "failed"
)

type TaskRecord struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	Target    string     `json:"target"`
	Status    TaskStatus `json:"status"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	Logs      []string   `json:"logs"`
}

type ScheduleRunStatus struct {
	LastRunAt   string   `json:"last_run_at,omitempty"`
	LastStatus  string   `json:"last_status,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
	LastSummary string   `json:"last_summary,omitempty"`
	NextRuns    []string `json:"next_runs,omitempty"`
}

type ContainerBackupResponse struct {
	Machine        string            `json:"machine"`
	Backup         SpecBackup        `json:"backup"`
	SnapshotStatus ScheduleRunStatus `json:"snapshot_status"`
	ExportStatus   ScheduleRunStatus `json:"export_status"`
}
