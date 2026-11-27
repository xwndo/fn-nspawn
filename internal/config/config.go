package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type AppConfig struct {
	StandardBaseRoot      string   `json:"standard_base_root"`
	BaseRoots             []string `json:"base_roots"`
	DefaultPhysicalNIC    string   `json:"default_physical_nic"`
	DefaultMirror         string   `json:"default_mirror"`
	DefaultSecurityMirror string   `json:"default_security_mirror"`
	DefaultSuite          string   `json:"default_suite"`
	DefaultTmpSize        string   `json:"default_tmp_size"`
}

type Store struct {
	mu       sync.RWMutex
	filePath string
	cfg      AppConfig
}

func DefaultAppConfig() AppConfig {
	return AppConfig{
		StandardBaseRoot:      "/vol1/nspawn",
		BaseRoots:             []string{"/vol1/nspawn"},
		DefaultPhysicalNIC:    "",
		DefaultMirror:         "https://mirrors.ustc.edu.cn/debian",
		DefaultSecurityMirror: "https://mirrors.ustc.edu.cn/debian-security",
		DefaultSuite:          "trixie",
		DefaultTmpSize:        "4G",
	}
}

func sanitizeSingleLine(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(s))
}

func isAbsHostPath(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)
}

func NewStore(etcDir string) (*Store, error) {
	if etcDir == "" {
		etcDir = "./data"
	}
	if err := os.MkdirAll(etcDir, 0755); err != nil {
		return nil, err
	}
	fp := filepath.Join(etcDir, "config.json")
	s := &Store{
		filePath: fp,
		cfg:      DefaultAppConfig(),
	}
	if data, err := os.ReadFile(fp); err == nil {
		var loaded AppConfig
		if uErr := json.Unmarshal(data, &loaded); uErr == nil {
			s.cfg = mergeDefaults(loaded)
		} else {
			bak := fp + ".corrupt"
			_ = os.WriteFile(bak, data, 0600)
			log.Printf("[WARN] 解析配置文件 %s 失败 (%v)，已备份至 %s 并回退默认配置", fp, uErr, bak)
			_ = s.Save(s.cfg)
		}
	} else {
		// Initialize defaults from wizard env if present
		if base := sanitizeSingleLine(os.Getenv("wizard_base_root")); base != "" {
			clean := filepath.Clean(base)
			s.cfg.StandardBaseRoot = clean
			s.cfg.BaseRoots = []string{clean}
		}
		_ = s.Save(s.cfg)
	}
	return s, nil
}

func mergeDefaults(c AppConfig) AppConfig {
	def := DefaultAppConfig()
	c.StandardBaseRoot = sanitizeSingleLine(c.StandardBaseRoot)
	c.DefaultPhysicalNIC = sanitizeSingleLine(c.DefaultPhysicalNIC)
	c.DefaultMirror = sanitizeSingleLine(c.DefaultMirror)
	c.DefaultSecurityMirror = sanitizeSingleLine(c.DefaultSecurityMirror)
	c.DefaultSuite = sanitizeSingleLine(c.DefaultSuite)
	c.DefaultTmpSize = sanitizeSingleLine(c.DefaultTmpSize)

	if c.StandardBaseRoot == "" || c.StandardBaseRoot == "." {
		for _, r := range c.BaseRoots {
			clean := filepath.Clean(sanitizeSingleLine(r))
			if filepath.Base(clean) == "nspawn-std" {
				c.StandardBaseRoot = clean
				break
			}
		}
		if (c.StandardBaseRoot == "" || c.StandardBaseRoot == ".") && len(c.BaseRoots) > 0 {
			clean := filepath.Clean(sanitizeSingleLine(c.BaseRoots[0]))
			if clean != "" && clean != "." {
				c.StandardBaseRoot = clean
			}
		}
		if c.StandardBaseRoot == "" || c.StandardBaseRoot == "." {
			c.StandardBaseRoot = def.StandardBaseRoot
		}
	} else {
		c.StandardBaseRoot = filepath.Clean(c.StandardBaseRoot)
	}
	if c.DefaultMirror == "" {
		c.DefaultMirror = def.DefaultMirror
	}
	if c.DefaultSecurityMirror == "" {
		c.DefaultSecurityMirror = def.DefaultSecurityMirror
	}
	if c.DefaultSuite == "" {
		c.DefaultSuite = def.DefaultSuite
	}
	if c.DefaultTmpSize == "" {
		c.DefaultTmpSize = def.DefaultTmpSize
	}
	return c
}

func (s *Store) Get() AppConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.cfg
	out.BaseRoots = append([]string(nil), s.cfg.BaseRoots...)
	return out
}

func (s *Store) Save(next AppConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.ContainsAny(next.StandardBaseRoot, "\r\n\x00") {
		return fmt.Errorf("标准容器根目录包含非法控制字符")
	}
	next = mergeDefaults(next)
	if next.StandardBaseRoot != "" && !isAbsHostPath(next.StandardBaseRoot) {
		return fmt.Errorf("标准容器根目录必须是绝对路径: %s", next.StandardBaseRoot)
	}

	seen := make(map[string]bool)
	cleanRoots := make([]string, 0, len(next.BaseRoots))
	for _, r := range next.BaseRoots {
		trimmed := sanitizeSingleLine(r)
		if trimmed == "" {
			continue
		}
		c := filepath.Clean(trimmed)
		if c == "." || c == "" || seen[c] {
			continue
		}
		if !isAbsHostPath(c) {
			return fmt.Errorf("补充扫描目录必须是绝对路径: %s", r)
		}
		seen[c] = true
		cleanRoots = append(cleanRoots, c)
	}
	next.BaseRoots = cleanRoots

	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.filePath); err != nil {
		return err
	}
	s.cfg = next
	return nil
}

func (s *Store) AddBaseRoot(root string) error {
	cfg := s.Get()
	c := filepath.Clean(sanitizeSingleLine(root))
	if c == "" || c == "." || !isAbsHostPath(c) {
		return nil
	}
	for _, existing := range cfg.BaseRoots {
		if existing == c {
			return nil
		}
	}
	cfg.BaseRoots = append(cfg.BaseRoots, c)
	return s.Save(cfg)
}
