package nspawn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type OnlineImageSource struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	BaseURL     string `json:"base_url"`
	Format      string `json:"format"` // simplestreams | proxmox
}

type OnlineImageItem struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	Distro       string `json:"distro"`
	DistroLabel  string `json:"distro_label"`
	Release      string `json:"release"`
	ReleaseTitle string `json:"release_title"`
	Arch         string `json:"arch"`
	BuildVersion string `json:"build_version"`
	Filename     string `json:"filename"`
	URL          string `json:"url"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256,omitempty"`
	Cached       bool   `json:"cached"`
}

type OnlineImageCatalog struct {
	Source    string              `json:"source"`
	Arch      string              `json:"arch"`
	FetchedAt string              `json:"fetched_at"`
	Sources   []OnlineImageSource `json:"sources"`
	Images    []OnlineImageItem   `json:"images"`
}

type OnlineImageCacheStats struct {
	CacheDir   string `json:"cache_dir"`
	FileCount  int    `json:"file_count"`
	TotalBytes int64  `json:"total_bytes"`
}

type OnlineImageCacheClearResult struct {
	CacheDir     string `json:"cache_dir"`
	RemovedFiles int    `json:"removed_files"`
	FreedBytes   int64  `json:"freed_bytes"`
}

type onlineCatalogCacheEntry struct {
	fetchedAt time.Time
	items     []OnlineImageItem
}

var distroMetaMap = map[string]struct {
	Label string
	Order int
}{
	"debian":     {Label: "Debian", Order: 1},
	"ubuntu":     {Label: "Ubuntu", Order: 2},
	"archlinux":  {Label: "Arch Linux", Order: 3},
	"fedora":     {Label: "Fedora", Order: 4},
	"rockylinux": {Label: "Rocky Linux", Order: 5},
	"almalinux":  {Label: "AlmaLinux", Order: 6},
	"centos":     {Label: "CentOS Stream", Order: 7},
	"opensuse":   {Label: "openSUSE", Order: 8},
	"kali":       {Label: "Kali Linux", Order: 9},
	"openeuler":  {Label: "openEuler", Order: 10},
	"openwrt":    {Label: "OpenWrt (旁路由)", Order: 11},
}

var (
	reProxmoxHref     = regexp.MustCompile(`(?i)href="([^"]+\.tar\.(?:zst|xz|gz))"`)
	reProxmoxFilename = regexp.MustCompile(`^([a-z0-9]+)-([a-z0-9._-]+?)_([^_]+)_(amd64|arm64)\.(tar\.(?:zst|xz|gz))$`)
)

func OnlineImageSources() []OnlineImageSource {
	return []OnlineImageSource{
		{
			ID:          "tuna",
			Name:        "清华大学 TUNA 镜像站 (LXC Images)",
			Description: "支持 Debian / Ubuntu / Arch / Fedora / Rocky / AlmaLinux / CentOS / openSUSE / Kali / OpenWrt",
			BaseURL:     "https://mirrors.tuna.tsinghua.edu.cn/lxc-images",
			Format:      "simplestreams",
		},
		{
			ID:          "ustc",
			Name:        "中科大 USTC 镜像站 (Proxmox 系统模板)",
			Description: "支持 Debian / Ubuntu / Arch Linux / Fedora / Rocky / AlmaLinux / CentOS / openSUSE / openEuler",
			BaseURL:     "https://mirrors.ustc.edu.cn/proxmox/images/system",
			Format:      "proxmox",
		},
		{
			ID:          "bfsu",
			Name:        "北京外国语大学 BFSU 镜像站 (LXC Images)",
			Description: "清华同源高校镜像，支持多发行版及 OpenWrt 镜像高速下载",
			BaseURL:     "https://mirrors.bfsu.edu.cn/lxc-images",
			Format:      "simplestreams",
		},
		{
			ID:          "official",
			Name:        "Linux Containers 官方源",
			Description: "上游官方镜像索引（images.linuxcontainers.org），适合海外网络或检查最新镜像",
			BaseURL:     "https://images.linuxcontainers.org",
			Format:      "simplestreams",
		},
	}
}

func findOnlineImageSource(id string) (OnlineImageSource, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, s := range OnlineImageSources() {
		if s.ID == id {
			return s, true
		}
	}
	return OnlineImageSource{}, false
}

func IsAllowedOnlineImageURL(rawURL string) bool {
	u := strings.TrimSpace(rawURL)
	if !strings.HasPrefix(u, "https://") {
		return false
	}
	for _, src := range OnlineImageSources() {
		prefix := strings.TrimRight(src.BaseURL, "/") + "/"
		if strings.HasPrefix(u, prefix) {
			return true
		}
	}
	return false
}

func onlineCachePath(baseRoot, sourceID, filename string) string {
	cleanRoot := filepath.Clean(strings.TrimSpace(baseRoot))
	if cleanRoot == "" || cleanRoot == "." {
		cleanRoot = "/vol1/nspawn"
	}
	safeSource := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(strings.TrimSpace(sourceID))
	if safeSource == "" {
		safeSource = "online"
	}
	safeFile := filepath.Base(strings.TrimSpace(filename))
	if safeFile == "" || safeFile == "." || safeFile == "/" {
		safeFile = "rootfs.tar.xz"
	}
	return filepath.Join(cleanRoot, ".image-cache", safeSource, safeFile)
}

type simpleStreamsIndex struct {
	Products map[string]simpleStreamsProduct `json:"products"`
}

type simpleStreamsProduct struct {
	OS           string                          `json:"os"`
	Release      string                          `json:"release"`
	ReleaseTitle string                          `json:"release_title"`
	Arch         string                          `json:"arch"`
	Variant      string                          `json:"variant"`
	Versions     map[string]simpleStreamsVersion `json:"versions"`
}

type simpleStreamsVersion struct {
	Items map[string]simpleStreamsItem `json:"items"`
}

type simpleStreamsItem struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func parseSimpleStreamsCatalog(data []byte, sourceID, baseURL, targetArch string) ([]OnlineImageItem, error) {
	var idx simpleStreamsIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("解析 SimpleStreams 索引失败: %w", err)
	}
	targetArch = strings.ToLower(strings.TrimSpace(targetArch))
	baseTrim := strings.TrimRight(baseURL, "/")

	var items []OnlineImageItem
	for prodKey, prod := range idx.Products {
		distro := strings.ToLower(strings.TrimSpace(prod.OS))
		if distro == "" {
			if parts := strings.Split(prodKey, ":"); len(parts) > 0 {
				distro = strings.ToLower(parts[0])
			}
		}
		meta, ok := distroMetaMap[distro]
		if !ok {
			continue
		}
		arch := strings.ToLower(strings.TrimSpace(prod.Arch))
		if arch == "" {
			if parts := strings.Split(prodKey, ":"); len(parts) >= 3 {
				arch = strings.ToLower(parts[2])
			}
		}
		if targetArch != "" && arch != targetArch {
			continue
		}
		variant := strings.ToLower(strings.TrimSpace(prod.Variant))
		if variant != "" && variant != "default" {
			continue
		}

		vKeys := make([]string, 0, len(prod.Versions))
		for k := range prod.Versions {
			vKeys = append(vKeys, k)
		}
		sort.Slice(vKeys, func(i, j int) bool { return vKeys[i] > vKeys[j] })

		var chosenVer string
		var chosenRoot simpleStreamsItem
		for _, vk := range vKeys {
			ver := prod.Versions[vk]
			if rootItem, exists := ver.Items["root.tar.xz"]; exists && rootItem.Path != "" {
				chosenVer = vk
				chosenRoot = rootItem
				break
			}
		}
		if chosenVer == "" {
			continue
		}

		rel := strings.TrimSpace(prod.Release)
		relTitle := strings.TrimSpace(prod.ReleaseTitle)
		if relTitle == "" {
			relTitle = rel
		}
		fullURL := baseTrim + "/" + strings.TrimLeft(chosenRoot.Path, "/")
		buildSanitized := strings.NewReplacer(":", "-", "/", "-").Replace(chosenVer)
		safeRel := strings.NewReplacer("/", "-", ":", "-").Replace(rel)
		filename := fmt.Sprintf("%s-%s-%s-%s.tar.xz", distro, safeRel, arch, buildSanitized)

		items = append(items, OnlineImageItem{
			ID:           fmt.Sprintf("%s:%s:%s:%s", sourceID, distro, rel, arch),
			Source:       sourceID,
			Distro:       distro,
			DistroLabel:  meta.Label,
			Release:      rel,
			ReleaseTitle: relTitle,
			Arch:         arch,
			BuildVersion: chosenVer,
			Filename:     filename,
			URL:          fullURL,
			SizeBytes:    chosenRoot.Size,
			SHA256:       strings.ToLower(strings.TrimSpace(chosenRoot.SHA256)),
		})
	}

	sortOnlineImageItems(items)
	return items, nil
}

func parseProxmoxIndexCatalog(htmlStr, sourceID, baseURL, targetArch string) []OnlineImageItem {
	targetArch = strings.ToLower(strings.TrimSpace(targetArch))
	baseTrim := strings.TrimRight(baseURL, "/")
	matches := reProxmoxHref.FindAllStringSubmatch(htmlStr, -1)

	seenFiles := make(map[string]bool)
	bestByKey := make(map[string]OnlineImageItem)

	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		fname := filepath.Base(strings.TrimSpace(m[1]))
		if fname == "" || seenFiles[fname] {
			continue
		}
		seenFiles[fname] = true

		parts := reProxmoxFilename.FindStringSubmatch(fname)
		if len(parts) < 6 {
			continue
		}
		distro := strings.ToLower(parts[1])
		flavor := strings.ToLower(parts[2])
		buildVer := parts[3]
		arch := strings.ToLower(parts[4])

		meta, ok := distroMetaMap[distro]
		if !ok {
			continue
		}
		if strings.Contains(flavor, "openrc") {
			continue
		}
		if targetArch != "" && arch != targetArch {
			continue
		}

		rel := flavor
		for _, suffix := range []string{"-default", "-standard", "-base"} {
			if strings.HasSuffix(rel, suffix) {
				rel = strings.TrimSuffix(rel, suffix)
				break
			}
		}
		if rel == "" || (distro == "archlinux" && flavor == "base") {
			rel = "current"
		}
		relTitle := rel
		if rel == "12" && distro == "debian" {
			relTitle = "12 (bookworm)"
		} else if rel == "13" && distro == "debian" {
			relTitle = "13 (trixie)"
		} else if rel == "11" && distro == "debian" {
			relTitle = "11 (bullseye)"
		}

		item := OnlineImageItem{
			ID:           fmt.Sprintf("%s:%s:%s:%s", sourceID, distro, rel, arch),
			Source:       sourceID,
			Distro:       distro,
			DistroLabel:  meta.Label,
			Release:      rel,
			ReleaseTitle: relTitle,
			Arch:         arch,
			BuildVersion: buildVer,
			Filename:     fname,
			URL:          baseTrim + "/" + fname,
		}

		key := distro + "|" + rel + "|" + arch
		if prev, exists := bestByKey[key]; !exists || compareProxmoxCandidate(item, prev) > 0 {
			bestByKey[key] = item
		}
	}

	items := make([]OnlineImageItem, 0, len(bestByKey))
	for _, it := range bestByKey {
		items = append(items, it)
	}
	sortOnlineImageItems(items)
	return items
}

func compareProxmoxCandidate(a, b OnlineImageItem) int {
	if a.BuildVersion != b.BuildVersion {
		if a.BuildVersion > b.BuildVersion {
			return 1
		}
		return -1
	}
	extScore := func(name string) int {
		switch {
		case strings.HasSuffix(name, ".tar.zst"):
			return 3
		case strings.HasSuffix(name, ".tar.xz"):
			return 2
		default:
			return 1
		}
	}
	sa, sb := extScore(a.Filename), extScore(b.Filename)
	if sa > sb {
		return 1
	}
	if sa < sb {
		return -1
	}
	return 0
}

func sortOnlineImageItems(items []OnlineImageItem) {
	sort.Slice(items, func(i, j int) bool {
		oi := distroMetaMap[items[i].Distro].Order
		oj := distroMetaMap[items[j].Distro].Order
		if oi != oj {
			return oi < oj
		}
		return compareReleaseDesc(items[i].Release, items[j].Release)
	})
}

func compareReleaseDesc(a, b string) bool {
	na, erra := parseLeadingNumber(a)
	nb, errb := parseLeadingNumber(b)
	if erra == nil && errb == nil && na != nb {
		return na > nb
	}
	return a > b
}

func parseLeadingNumber(s string) (float64, error) {
	var num strings.Builder
	dotSeen := false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			num.WriteRune(r)
		} else if r == '.' && !dotSeen && num.Len() > 0 {
			dotSeen = true
			num.WriteRune(r)
		} else {
			break
		}
	}
	if num.Len() == 0 {
		return 0, fmt.Errorf("not a number")
	}
	return strconv.ParseFloat(num.String(), 64)
}

func (m *Manager) ListOnlineImageCatalog(sourceID, arch string, forceRefresh bool) (*OnlineImageCatalog, error) {
	if strings.TrimSpace(sourceID) == "" {
		sourceID = "tuna"
	}
	src, ok := findOnlineImageSource(sourceID)
	if !ok {
		return nil, fmt.Errorf("不支持的在线镜像源: %s", sourceID)
	}

	arch = strings.ToLower(strings.TrimSpace(arch))
	if arch == "" {
		if runtime.GOARCH == "arm64" {
			arch = "arm64"
		} else {
			arch = "amd64"
		}
	}

	cacheKey := src.ID + ":" + arch
	m.onlineCatalogMu.Lock()
	cachedEntry, hasCache := m.onlineCatalogCache[cacheKey]
	m.onlineCatalogMu.Unlock()

	var items []OnlineImageItem
	var fetchedAt time.Time

	if hasCache && !forceRefresh && time.Since(cachedEntry.fetchedAt) < 10*time.Minute {
		items = append([]OnlineImageItem(nil), cachedEntry.items...)
		fetchedAt = cachedEntry.fetchedAt
	} else {
		client := &http.Client{Timeout: 25 * time.Second}
		indexURL := strings.TrimRight(src.BaseURL, "/") + "/streams/v1/images.json"
		if src.Format == "proxmox" {
			indexURL = strings.TrimRight(src.BaseURL, "/") + "/"
		}
		req, err := http.NewRequest(http.MethodGet, indexURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "fn-nspawn/"+AppVersion)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("获取 %s 镜像目录失败: %w", src.Name, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("镜像源 %s 返回 HTTP %d", src.Name, resp.StatusCode)
		}

		const maxIndexSize = 16 << 20 // 16 MiB safety bound
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexSize))
		if err != nil {
			return nil, fmt.Errorf("读取 %s 镜像目录失败: %w", src.Name, err)
		}

		if src.Format == "proxmox" {
			items = parseProxmoxIndexCatalog(string(body), src.ID, src.BaseURL, arch)
		} else {
			items, err = parseSimpleStreamsCatalog(body, src.ID, src.BaseURL, arch)
			if err != nil {
				return nil, err
			}
		}
		fetchedAt = time.Now()
		m.onlineCatalogMu.Lock()
		if m.onlineCatalogCache == nil {
			m.onlineCatalogCache = make(map[string]onlineCatalogCacheEntry)
		}
		m.onlineCatalogCache[cacheKey] = onlineCatalogCacheEntry{
			fetchedAt: fetchedAt,
			items:     append([]OnlineImageItem(nil), items...),
		}
		m.onlineCatalogMu.Unlock()
	}

	stdRoot := m.cfgStore.Get().StandardBaseRoot
	for i := range items {
		cp := onlineCachePath(stdRoot, items[i].Source, items[i].Filename)
		if st, err := os.Stat(cp); err == nil && !st.IsDir() && st.Size() > 0 {
			items[i].Cached = true
		} else {
			items[i].Cached = false
		}
	}

	return &OnlineImageCatalog{
		Source:    src.ID,
		Arch:      arch,
		FetchedAt: fetchedAt.Format("2006-01-02 15:04:05"),
		Sources:   OnlineImageSources(),
		Images:    items,
	}, nil
}

func (m *Manager) lockImageDownload(destPath string) func() {
	m.downloadMu.Lock()
	if m.downloadLocks == nil {
		m.downloadLocks = make(map[string]*sync.Mutex)
	}
	dlMu, ok := m.downloadLocks[destPath]
	if !ok {
		dlMu = &sync.Mutex{}
		m.downloadLocks[destPath] = dlMu
	}
	m.downloadMu.Unlock()

	dlMu.Lock()
	return func() {
		dlMu.Unlock()
	}
}

func (m *Manager) InspectOnlineImageCache() OnlineImageCacheStats {
	cleanRoot := filepath.Clean(strings.TrimSpace(m.cfgStore.Get().StandardBaseRoot))
	if cleanRoot == "" || cleanRoot == "." {
		cleanRoot = "/vol1/nspawn"
	}
	cacheDir := filepath.Join(cleanRoot, ".image-cache")
	stats := OnlineImageCacheStats{CacheDir: cacheDir}
	_ = filepath.WalkDir(cacheDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".download-") || strings.HasSuffix(name, ".tmp") {
			return nil
		}
		if fi, iErr := d.Info(); iErr == nil {
			stats.FileCount++
			stats.TotalBytes += fi.Size()
		}
		return nil
	})
	return stats
}

func (m *Manager) ClearOnlineImageCache() (OnlineImageCacheClearResult, error) {
	cleanRoot := filepath.Clean(strings.TrimSpace(m.cfgStore.Get().StandardBaseRoot))
	if cleanRoot == "" || cleanRoot == "." {
		cleanRoot = "/vol1/nspawn"
	}
	cacheDir := filepath.Join(cleanRoot, ".image-cache")
	res := OnlineImageCacheClearResult{CacheDir: cacheDir}
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		return res, nil
	}
	err := filepath.WalkDir(cacheDir, func(path string, d os.DirEntry, wErr error) error {
		if wErr != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".download-") || strings.HasSuffix(name, ".tmp") {
			return nil
		}
		var size int64
		if fi, iErr := d.Info(); iErr == nil {
			size = fi.Size()
		}
		if rErr := os.Remove(path); rErr == nil {
			res.RemovedFiles++
			res.FreedBytes += size
		}
		return nil
	})
	return res, err
}

func (m *Manager) DownloadOnlineImage(logFn func(string), rawURL, sourceID, filename, expectedSHA256, baseRoot string) (string, error) {
	if !IsAllowedOnlineImageURL(rawURL) {
		return "", fmt.Errorf("非法的在线镜像下载地址（仅允许内置可信镜像源 HTTPS 地址）")
	}
	if filename == "" {
		filename = filepath.Base(rawURL)
	}
	destPath := onlineCachePath(baseRoot, sourceID, filename)
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return "", err
	}

	unlock := m.lockImageDownload(destPath)
	defer unlock()

	expectedSHA256 = strings.ToLower(strings.TrimSpace(expectedSHA256))
	if st, err := os.Stat(destPath); err == nil && !st.IsDir() && st.Size() > 0 {
		if expectedSHA256 == "" {
			logFn("[OK] 命中本地镜像缓存，直接复用: " + destPath)
			return destPath, nil
		}
		if sum, err := fileSHA256Hex(destPath); err == nil && sum == expectedSHA256 {
			logFn("[OK] 命中本地镜像缓存且 SHA256 校验通过: " + destPath)
			return destPath, nil
		}
		logFn("[WARN] 本地缓存镜像 SHA256 与索引不一致，将重新下载...")
		_ = os.Remove(destPath)
	}

	logFn("   正在下载在线系统镜像: " + rawURL)
	client := &http.Client{Timeout: 15 * time.Minute}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "fn-nspawn/"+AppVersion)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载镜像失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载镜像返回 HTTP %d", resp.StatusCode)
	}

	outFile, err := os.CreateTemp(filepath.Dir(destPath), ".download-*")
	if err != nil {
		return "", err
	}
	tmpPath := outFile.Name()
	cleanupTmp := true
	defer func() {
		_ = outFile.Close()
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	total := resp.ContentLength
	var downloaded int64
	lastLogAt := time.Now()
	lastPct := -1
	buf := make([]byte, 64*1024)

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := outFile.Write(buf[:n]); err != nil {
				return "", err
			}
			_, _ = hasher.Write(buf[:n])
			downloaded += int64(n)

			now := time.Now()
			if total > 0 {
				pct := int(downloaded * 100 / total)
				if pct >= lastPct+15 || now.Sub(lastLogAt) >= 4*time.Second {
					logFn(fmt.Sprintf("   下载进度: %d%% (%.1f MiB / %.1f MiB)", pct, float64(downloaded)/(1024*1024), float64(total)/(1024*1024)))
					lastPct = pct
					lastLogAt = now
				}
			} else if now.Sub(lastLogAt) >= 4*time.Second {
				logFn(fmt.Sprintf("   已下载: %.1f MiB...", float64(downloaded)/(1024*1024)))
				lastLogAt = now
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("读取镜像数据流失败: %w", readErr)
		}
	}

	if err := outFile.Close(); err != nil {
		return "", err
	}
	if total > 0 && downloaded != total {
		return "", fmt.Errorf("镜像下载字节数不完整: 已接收 %d / 预期 %d", downloaded, total)
	}
	if downloaded == 0 {
		return "", fmt.Errorf("下载到的镜像文件为空")
	}

	gotSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if expectedSHA256 != "" && gotSHA256 != expectedSHA256 {
		return "", fmt.Errorf("镜像 SHA256 校验失败: got %s, want %s", gotSHA256, expectedSHA256)
	}

	_ = os.Chmod(tmpPath, 0644)
	if err := os.Rename(tmpPath, destPath); err != nil {
		return "", err
	}
	cleanupTmp = false
	logFn(fmt.Sprintf("[OK] 镜像下载完成 (%.1f MiB): %s", float64(downloaded)/(1024*1024), destPath))
	return destPath, nil
}

func fileSHA256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
