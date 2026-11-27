package nspawn

import (
	"fmt"
	"path/filepath"
	"strings"
)

func escapeSystemdPercent(s string) string {
	return strings.ReplaceAll(sanitizeSingleLineText(s), "%", "%%")
}

func quoteSystemdArg(s string) string {
	s = escapeSystemdPercent(s)
	if strings.ContainsAny(s, " \t\"'\\") {
		escaped := strings.ReplaceAll(s, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		return `"` + escaped + `"`
	}
	return s
}

func RenderServiceUnit(cfg ContainerConfig) string {
	cfg.Machine = sanitizeSingleLineText(cfg.Machine)
	cfg.TmpSize = sanitizeSingleLineText(cfg.TmpSize)
	cfg.TasksMax = sanitizeSingleLineText(cfg.TasksMax)
	cfg.MemoryMax = sanitizeSingleLineText(cfg.MemoryMax)
	cfg.CPUQuota = sanitizeSingleLineText(cfg.CPUQuota)
	cfg.PhysicalNIC = sanitizeSingleLineText(cfg.PhysicalNIC)
	if cfg.TmpSize == "" {
		cfg.TmpSize = "4G"
	}
	if cfg.TasksMax == "" {
		cfg.TasksMax = "infinity"
	}
	if strings.TrimSpace(cfg.PhysicalNIC) == "" {
		cfg.PhysicalNIC = "enp2s0"
	}

	svcName := sanitizeSingleLineText(cfg.ServiceName)
	if svcName == "" && cfg.Machine != "" {
		svcName = fmt.Sprintf("nspawn-%s.service", cfg.Machine)
	}
	sourcePath := sanitizeSingleLineText(cfg.ServicePath)
	if sourcePath == "" && cfg.ConfDir != "" && svcName != "" {
		sourcePath = filepath.Join(cfg.ConfDir, svcName)
	}

	isOpenWrt := cfg.InitMode == InitModeOpenWrt
	var execArgs []string
	execArgs = append(execArgs,
		"/usr/bin/systemd-nspawn",
		"--quiet",
		"--keep-unit",
	)
	if !isOpenWrt {
		execArgs = append(execArgs, "--boot")
	}
	execArgs = append(execArgs,
		"--machine="+cfg.Machine,
		"--directory="+quoteSystemdArg(cfg.Rootfs),
		"--private-network",
		"--network-veth",
		fmt.Sprintf("--tmpfs=/tmp:mode=1777,nosuid,nodev,size=%s", cfg.TmpSize),
	)
	if cfg.EnableTun {
		execArgs = append(execArgs, "--bind=/dev/net/tun")
	}

	for _, b := range cfg.BindMounts {
		flag := "--bind="
		if b.ReadOnly {
			flag = "--bind-ro="
		}
		execArgs = append(execArgs, flag+quoteSystemdArg(b.HostPath+":"+b.ContainerPath))
	}
	if isOpenWrt {
		execArgs = append(execArgs, "/sbin/init")
	}

	netUp := filepath.Join(cfg.ConfDir, "network-up.sh")
	netDown := filepath.Join(cfg.ConfDir, "network-down.sh")

	var sb strings.Builder
	if sourcePath != "" {
		sb.WriteString("# fn-nspawn-source=" + sourcePath + "\n")
	}
	sb.WriteString("[Unit]\n")
	sb.WriteString(fmt.Sprintf("Description=systemd-nspawn container %s\n", cfg.Machine))
	sb.WriteString("After=network-online.target openvswitch-switch.service local-fs.target\n")
	sb.WriteString("Wants=network-online.target\n")
	sb.WriteString("StartLimitIntervalSec=0\n\n")
	sb.WriteString("[Service]\n")
	if isOpenWrt {
		sb.WriteString("Type=simple\n")
	} else {
		sb.WriteString("Type=notify\n")
		sb.WriteString("NotifyAccess=all\n")
	}
	sb.WriteString("TimeoutStartSec=240\n\n")
	sb.WriteString("# 开机早期存储池与 OVS 可能尚未挂载/就绪，先于 / 上轮询等待 rootfs 与 OVS 网桥就绪。\n")
	tunPrep := ""
	if cfg.EnableTun {
		tunPrep = "modprobe tun >/dev/null 2>&1 || true; "
	}
	sb.WriteString(fmt.Sprintf(
		"ExecStartPre=/bin/bash -c '%sfor i in $(seq 1 180); do [ -d \"$1/etc\" ] && ovs-vsctl port-to-br \"$2\" >/dev/null 2>&1 && exit 0; sleep 1; done; echo \"[ERROR] 等待 rootfs($1) 挂载或 OVS($2) 就绪超时\" >&2; exit 1' _ %s %s\n\n",
		tunPrep, quoteSystemdArg(cfg.Rootfs), quoteSystemdArg(cfg.PhysicalNIC),
	))
	sb.WriteString("# --keep-unit：由本 systemd unit 直接管理容器 cgroup/lifecycle。\n")
	sb.WriteString("# RestartForceExitStatus=133 只在容器内 reboot 时强制重启，poweroff 正常停止。\n")
	sb.WriteString("ExecStart=" + strings.Join(execArgs, " ") + "\n\n")
	sb.WriteString("# 每次启动都重新把 veth 接入当前 OVS Bridge。\n")
	sb.WriteString("ExecStartPost=" + quoteSystemdArg(netUp) + "\n")
	sb.WriteString("ExecStopPost=" + quoteSystemdArg(netDown) + "\n\n")
	sb.WriteString("KillMode=mixed\n")
	sb.WriteString("Delegate=yes\n")
	if cfg.EnableTun {
		sb.WriteString("DeviceAllow=/dev/net/tun rwm\n")
	}
	sb.WriteString("TasksMax=" + cfg.TasksMax + "\n")
	if strings.TrimSpace(cfg.MemoryMax) != "" {
		sb.WriteString("MemoryMax=" + strings.TrimSpace(cfg.MemoryMax) + "\n")
	}
	if strings.TrimSpace(cfg.CPUQuota) != "" {
		sb.WriteString("CPUQuota=" + strings.TrimSpace(cfg.CPUQuota) + "\n")
	}
	sb.WriteString("\nRestart=on-failure\n")
	sb.WriteString("RestartForceExitStatus=133\n")
	sb.WriteString("SuccessExitStatus=133\n")
	sb.WriteString("RestartSec=3\n\n")
	sb.WriteString("[Install]\n")
	sb.WriteString("WantedBy=multi-user.target\n")
	return sb.String()
}

func RenderNetworkUpScript(physicalNIC, veth string) string {
	return fmt.Sprintf(`#!/bin/bash
set -Eeuo pipefail

PHYSICAL_NIC=%q
VETH=%q

command -v ovs-vsctl >/dev/null 2>&1 || {
  echo "[ERROR] ovs-vsctl 不存在" >&2
  exit 1
}

ip link show "$PHYSICAL_NIC" >/dev/null 2>&1 || {
  echo "[ERROR] 物理网卡不存在：$PHYSICAL_NIC" >&2
  exit 1
}

OVS_BRIDGE=""
for _ in {1..60}; do
  OVS_BRIDGE="$(ovs-vsctl port-to-br "$PHYSICAL_NIC" 2>/dev/null || true)"
  [[ -n "$OVS_BRIDGE" ]] && break
  sleep 1
done

[[ -n "$OVS_BRIDGE" ]] || {
  echo "[ERROR] $PHYSICAL_NIC 当前没有加入 OVS Bridge；可能 fnOS OVS 尚未恢复。" >&2
  exit 1
}

ovs-vsctl br-exists "$OVS_BRIDGE" || {
  echo "[ERROR] OVS Bridge 不存在：$OVS_BRIDGE" >&2
  exit 1
}

for _ in {1..30}; do
  ip link show "$VETH" >/dev/null 2>&1 && break
  sleep 1
done

ip link show "$VETH" >/dev/null 2>&1 || {
  echo "[ERROR] 等待 veth 超时：$VETH" >&2
  exit 1
}

ip link set "$VETH" up
ovs-vsctl --may-exist add-port "$OVS_BRIDGE" "$VETH"

echo "[OK] $VETH 已接入 OVS Bridge：$OVS_BRIDGE"
`, physicalNIC, veth)
}

func RenderNetworkDownScript(veth string) string {
	return fmt.Sprintf(`#!/bin/bash
set -Eeuo pipefail

VETH=%q

command -v ovs-vsctl >/dev/null 2>&1 || exit 0

CURRENT_BRIDGE="$(ovs-vsctl port-to-br "$VETH" 2>/dev/null || true)"

if [[ -n "$CURRENT_BRIDGE" ]]; then
  ovs-vsctl --if-exists del-port "$CURRENT_BRIDGE" "$VETH"
fi
`, veth)
}

func RenderRestoreScript(machine, physicalNIC, rootfs, confDir, serviceName string, autostart bool) string {
	autoFlag := "false"
	if autostart {
		autoFlag = "true"
	}
	return fmt.Sprintf(`#!/bin/bash
set -Eeuo pipefail

MACHINE=%q
PHYSICAL_NIC=%q
ROOTFS=%q
CONFIG=%q
AUTOSTART=%s

NSPAWN_SERVICE_NAME=%q
OLD_NETWORK_SERVICE_NAME="nspawn-${MACHINE}-network.service"

[[ $EUID -eq 0 ]] || {
  echo "[ERROR] 请使用 sudo bash $0" >&2
  exit 1
}

[[ -d "$ROOTFS" ]] || {
  echo "[ERROR] rootfs 不存在：$ROOTFS" >&2
  exit 1
}

if ! btrfs subvolume show "$ROOTFS" >/dev/null 2>&1; then
  echo "[WARN] rootfs 不是 Btrfs subvolume（将跳过 Btrfs 快照校验，继续恢复）：$ROOTFS"
fi

command -v ovs-vsctl >/dev/null 2>&1 || {
  echo "[ERROR] ovs-vsctl 不存在；请先在 fnOS 启用 OVS。" >&2
  exit 1
}

OVS_BRIDGE="$(ovs-vsctl port-to-br "$PHYSICAL_NIC" 2>/dev/null || true)"

[[ -n "$OVS_BRIDGE" ]] || {
  echo "[ERROR] $PHYSICAL_NIC 当前没有 OVS Bridge。" >&2
  exit 1
}

ovs-vsctl br-exists "$OVS_BRIDGE" || {
  echo "[ERROR] OVS Bridge 不存在：$OVS_BRIDGE" >&2
  exit 1
}

echo "[OK] 当前 OVS Bridge：$OVS_BRIDGE"

systemctl disable --now "$OLD_NETWORK_SERVICE_NAME" >/dev/null 2>&1 || true
rm -f "/etc/systemd/system/$OLD_NETWORK_SERVICE_NAME"

chmod 644 "$CONFIG/$NSPAWN_SERVICE_NAME" 2>/dev/null || true
rm -f "/etc/systemd/system/$NSPAWN_SERVICE_NAME"
rm -f "/etc/systemd/system/multi-user.target.wants/$NSPAWN_SERVICE_NAME"
install -m 644 "$CONFIG/$NSPAWN_SERVICE_NAME" "/etc/systemd/system/$NSPAWN_SERVICE_NAME"

systemctl daemon-reload
if [[ "$AUTOSTART" == "true" ]]; then
  systemctl enable "$NSPAWN_SERVICE_NAME"
else
  systemctl disable "$NSPAWN_SERVICE_NAME" >/dev/null 2>&1 || true
  rm -f "/etc/systemd/system/multi-user.target.wants/$NSPAWN_SERVICE_NAME"
  if [[ ! -f "/etc/systemd/system/$NSPAWN_SERVICE_NAME" || -L "/etc/systemd/system/$NSPAWN_SERVICE_NAME" ]]; then
    rm -f "/etc/systemd/system/$NSPAWN_SERVICE_NAME"
    install -m 644 "$CONFIG/$NSPAWN_SERVICE_NAME" "/etc/systemd/system/$NSPAWN_SERVICE_NAME"
  fi
fi
systemctl daemon-reload
systemctl restart "$NSPAWN_SERVICE_NAME"

echo "[OK] $MACHINE 已恢复并重新接入 OVS。"
machinectl list
`, machine, physicalNIC, rootfs, confDir, autoFlag, serviceName)
}
