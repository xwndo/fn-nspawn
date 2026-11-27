package nspawn

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	reTmpSizeFmt   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[KMGTPkmgtp]?$`)
	reMemoryMaxFmt = regexp.MustCompile(`^(infinity|[0-9]+(\.[0-9]+)?([KMGTPBkmgtpb](i?[Bb])?|%))$`)
	reCPUQuotaFmt  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?%$`)
	reTasksMaxFmt  = regexp.MustCompile(`^(infinity|[0-9]+)$`)
	reSafeNICName  = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`)
	reSafeSuite    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
)

func hasControlChars(s string) bool {
	return strings.ContainsAny(s, "\r\n\x00")
}

func sanitizeSingleLineText(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(s))
}

func isAbsHostOrContainerPath(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)
}

func hasUnescapedBindColon(p string) bool {
	rest := p
	if vol := filepath.VolumeName(p); vol != "" {
		rest = strings.TrimPrefix(p, vol)
	}
	return strings.Contains(rest, ":")
}

func validateTmpSize(s string) error {
	val := strings.TrimSpace(s)
	if val == "" {
		return nil
	}
	if hasControlChars(val) || !reTmpSizeFmt.MatchString(val) {
		return fmt.Errorf("无效的 /tmp 容量格式: %s（示例：4G、512M）", s)
	}
	return nil
}

func validateMemoryMax(s string) error {
	val := strings.TrimSpace(s)
	if val == "" {
		return nil
	}
	if hasControlChars(val) || !reMemoryMaxFmt.MatchString(val) {
		return fmt.Errorf("无效的 MemoryMax 格式: %s（示例：4G、512M、80%%、infinity）", s)
	}
	return nil
}

func validateCPUQuota(s string) error {
	val := strings.TrimSpace(s)
	if val == "" {
		return nil
	}
	if hasControlChars(val) || !reCPUQuotaFmt.MatchString(val) {
		return fmt.Errorf("无效的 CPUQuota 格式: %s（示例：100%%、200%%）", s)
	}
	return nil
}

func validateTasksMax(s string) error {
	val := strings.TrimSpace(s)
	if val == "" {
		return nil
	}
	if hasControlChars(val) || !reTasksMaxFmt.MatchString(val) {
		return fmt.Errorf("无效的 TasksMax 格式: %s（示例：4096、infinity）", s)
	}
	return nil
}

func validatePhysicalNIC(nic string) error {
	val := strings.TrimSpace(nic)
	if val == "" {
		return nil
	}
	if hasControlChars(val) || !reSafeNICName.MatchString(val) {
		return fmt.Errorf("非法的物理网卡名称: %s", nic)
	}
	return nil
}

func validateSuite(suite string) error {
	val := strings.TrimSpace(suite)
	if val == "" {
		return nil
	}
	if hasControlChars(val) || !reSafeSuite.MatchString(val) {
		return fmt.Errorf("非法的系统版本代号 (Suite): %s", suite)
	}
	return nil
}

func validateMirrorURL(raw string) error {
	val := strings.TrimSpace(raw)
	if val == "" {
		return nil
	}
	if strings.ContainsAny(val, " \t\r\n\x00\"'") {
		return fmt.Errorf("软件源镜像地址包含非法空白或引号字符")
	}
	u, err := url.Parse(val)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("软件源镜像地址必须是有效的 http/https URL: %s", raw)
	}
	return nil
}

func validatePassword(pw string) error {
	if hasControlChars(pw) {
		return errors.New("密码不能包含换行符或空字节")
	}
	return nil
}

func validateBindMounts(binds []BindMount) ([]BindMount, error) {
	var clean []BindMount
	for _, b := range binds {
		hp := strings.TrimSpace(b.HostPath)
		cp := strings.TrimSpace(b.ContainerPath)
		if hp == "" || cp == "" {
			return nil, errors.New("Bind Mount 宿主机路径和容器路径均不能为空")
		}
		if hasControlChars(hp) || hasControlChars(cp) {
			return nil, errors.New("Bind Mount 路径不能包含换行符或控制字符")
		}
		if hasUnescapedBindColon(hp) || hasUnescapedBindColon(cp) {
			return nil, fmt.Errorf("Bind Mount 路径不能包含冒号 ':' 字符: %s -> %s", hp, cp)
		}
		if !isAbsHostOrContainerPath(hp) || !isAbsHostOrContainerPath(cp) {
			return nil, fmt.Errorf("Bind Mount 必须使用绝对路径: %s -> %s", hp, cp)
		}
		st, err := os.Stat(hp)
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("宿主机源目录不存在或不是目录: %s", hp)
		}
		clean = append(clean, BindMount{
			HostPath:      hp,
			ContainerPath: cp,
			ReadOnly:      b.ReadOnly,
		})
	}
	return clean, nil
}

func (m *Manager) acquireContainerOp(opName string, machines ...string) (func(), error) {
	var keys []string
	seen := make(map[string]bool)
	for _, raw := range machines {
		k := strings.TrimSpace(raw)
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return func() {}, nil
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.activeOps == nil {
		m.activeOps = make(map[string]string)
	}
	for _, k := range keys {
		if cur, busy := m.activeOps[k]; busy {
			return nil, fmt.Errorf("容器 %s 当前正在执行 [%s] 操作，请等待其完成后再试", k, cur)
		}
	}
	for _, k := range keys {
		m.activeOps[k] = opName
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			m.opMu.Lock()
			for _, k := range keys {
				delete(m.activeOps, k)
			}
			m.opMu.Unlock()
		})
	}, nil
}

const defaultProbeTimeout = 4 * time.Second

func execOutputTimeout(timeout time.Duration, name string, args ...string) ([]byte, error) {
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

func execRunTimeout(timeout time.Duration, name string, args ...string) error {
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run()
}
