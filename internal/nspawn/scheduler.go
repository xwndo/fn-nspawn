package nspawn

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type CronSchedule struct {
	Expr     string
	Minutes  [60]bool
	Hours    [24]bool
	DOM      [32]bool // 1..31
	Months   [13]bool // 1..12
	DOW      [7]bool  // 0..6 (0 = Sunday)
	DomWild  bool
	DowWild  bool
}

func ParseCron(expr string) (*CronSchedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("Cron 表达式必须包含 5 个字段（分 时 日 月 周），当前共 %d 个", len(fields))
	}
	cs := &CronSchedule{
		Expr:    strings.Join(fields, " "),
		DomWild: fields[2] == "*",
		DowWild: fields[4] == "*",
	}
	if err := parseCronField(fields[0], 0, 59, false, cs.Minutes[:]); err != nil {
		return nil, fmt.Errorf("分钟字段错误 (%s): %w", fields[0], err)
	}
	if err := parseCronField(fields[1], 0, 23, false, cs.Hours[:]); err != nil {
		return nil, fmt.Errorf("小时字段错误 (%s): %w", fields[1], err)
	}
	if err := parseCronField(fields[2], 1, 31, false, cs.DOM[:]); err != nil {
		return nil, fmt.Errorf("日期字段错误 (%s): %w", fields[2], err)
	}
	if err := parseCronField(fields[3], 1, 12, false, cs.Months[:]); err != nil {
		return nil, fmt.Errorf("月份字段错误 (%s): %w", fields[3], err)
	}
	if err := parseCronField(fields[4], 0, 7, true, cs.DOW[:]); err != nil {
		return nil, fmt.Errorf("星期字段错误 (%s): %w", fields[4], err)
	}
	return cs, nil
}

func parseCronField(field string, minVal, maxVal int, isDOW bool, target []bool) error {
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("存在空列表项")
		}
		step := 1
		rangeExpr := part
		if slashIdx := strings.IndexByte(part, '/'); slashIdx >= 0 {
			rangeExpr = part[:slashIdx]
			stepStr := part[slashIdx+1:]
			s, err := strconv.Atoi(stepStr)
			if err != nil || s <= 0 {
				return fmt.Errorf("非法步长: %s", stepStr)
			}
			step = s
		}

		start, end := minVal, maxVal
		if rangeExpr == "*" {
			// full range
		} else if dashIdx := strings.IndexByte(rangeExpr, '-'); dashIdx >= 0 {
			a, err1 := strconv.Atoi(rangeExpr[:dashIdx])
			b, err2 := strconv.Atoi(rangeExpr[dashIdx+1:])
			if err1 != nil || err2 != nil || a < minVal || b > maxVal || a > b {
				return fmt.Errorf("非法区间: %s", rangeExpr)
			}
			start, end = a, b
		} else {
			v, err := strconv.Atoi(rangeExpr)
			if err != nil || v < minVal || v > maxVal {
				return fmt.Errorf("数值越界 (%d-%d): %s", minVal, maxVal, rangeExpr)
			}
			start = v
			if strings.Contains(part, "/") {
				end = maxVal
			} else {
				end = v
			}
		}

		for v := start; v <= end; v += step {
			idx := v
			if isDOW && idx == 7 {
				idx = 0
			}
			if idx >= 0 && idx < len(target) {
				target[idx] = true
			}
		}
	}
	return nil
}

func (c *CronSchedule) Matches(t time.Time) bool {
	if c == nil {
		return false
	}
	if !c.Minutes[t.Minute()] || !c.Hours[t.Hour()] || !c.Months[int(t.Month())] {
		return false
	}
	domMatch := c.DOM[t.Day()]
	dowMatch := c.DOW[int(t.Weekday())]
	if c.DomWild && c.DowWild {
		return true
	}
	if c.DomWild {
		return dowMatch
	}
	if c.DowWild {
		return domMatch
	}
	return domMatch || dowMatch
}

func (c *CronSchedule) NextRuns(from time.Time, count int) []time.Time {
	if c == nil || count <= 0 {
		return nil
	}
	cur := from.Truncate(time.Minute).Add(time.Minute)
	limit := cur.Add(2 * 366 * 24 * time.Hour)
	var out []time.Time
	for cur.Before(limit) && len(out) < count {
		if !c.Months[int(cur.Month())] {
			cur = time.Date(cur.Year(), cur.Month(), 1, 0, 0, 0, 0, cur.Location()).AddDate(0, 1, 0)
			continue
		}
		domMatch := c.DOM[cur.Day()]
		dowMatch := c.DOW[int(cur.Weekday())]
		dayOK := (c.DomWild && c.DowWild) || (c.DomWild && dowMatch) || (c.DowWild && domMatch) || (!c.DomWild && !c.DowWild && (domMatch || dowMatch))
		if !dayOK {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, cur.Location()).AddDate(0, 0, 1)
			continue
		}
		if !c.Hours[cur.Hour()] {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour(), 0, 0, 0, cur.Location()).Add(time.Hour)
			continue
		}
		if c.Minutes[cur.Minute()] {
			out = append(out, cur)
		}
		cur = cur.Add(time.Minute)
	}
	return out
}

func PreviewCronRuns(expr string, count int) ([]string, error) {
	cs, err := ParseCron(expr)
	if err != nil {
		return nil, err
	}
	if count <= 0 {
		count = 3
	}
	times := cs.NextRuns(time.Now(), count)
	out := make([]string, 0, len(times))
	for _, t := range times {
		out = append(out, t.Format("2006-01-02 15:04 (Mon)"))
	}
	return out, nil
}

func parseBackupEntryTimestamp(machine, tag, entryName string) (time.Time, bool) {
	cleanTag := sanitizeTag(tag, "auto")
	prefix := machine + "-" + cleanTag + "-"
	if !strings.HasPrefix(entryName, prefix) {
		return time.Time{}, false
	}
	rest := strings.TrimPrefix(entryName, prefix)
	for _, ext := range []string{".tar.zst", ".tar.gz", ".tar.xz", ".tgz", ".tar"} {
		rest = strings.TrimSuffix(rest, ext)
	}
	if len(rest) != 15 {
		return time.Time{}, false
	}
	ts, err := time.ParseInLocation("20060102-150405", rest, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

func selectExpiredBackupEntries(machine, tag string, names []string, keepCount, keepDays int, now time.Time) []string {
	if keepCount <= 0 && keepDays <= 0 {
		return nil
	}
	type candidate struct {
		name string
		ts   time.Time
	}
	var matched []candidate
	for _, n := range names {
		if ts, ok := parseBackupEntryTimestamp(machine, tag, n); ok {
			matched = append(matched, candidate{name: n, ts: ts})
		}
	}
	if len(matched) <= 1 {
		// Safety floor: always keep at least the single newest matching backup.
		return nil
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].ts.Equal(matched[j].ts) {
			return matched[i].ts.After(matched[j].ts)
		}
		return matched[i].name > matched[j].name
	})

	var expired []string
	maxAge := time.Duration(keepDays) * 24 * time.Hour
	for i := 1; i < len(matched); i++ {
		byCount := keepCount > 0 && i >= keepCount
		byAge := keepDays > 0 && now.Sub(matched[i].ts) > maxAge
		if byCount || byAge {
			expired = append(expired, matched[i].name)
		}
	}
	return expired
}

func normalizeSpecBackup(in SpecBackup) (SpecBackup, error) {
	var out SpecBackup
	if in.Snapshot != nil {
		s := *in.Snapshot
		s.Cron = strings.TrimSpace(s.Cron)
		s.Tag = sanitizeTag(s.Tag, "auto")
		if s.KeepCount < 0 || s.KeepDays < 0 {
			return SpecBackup{}, fmt.Errorf("快照保留份数和保留天数不能为负数")
		}
		if s.Enabled {
			if _, err := ParseCron(s.Cron); err != nil {
				return SpecBackup{}, fmt.Errorf("快照 Cron 表达式无效: %w", err)
			}
		} else if s.Cron != "" {
			if _, err := ParseCron(s.Cron); err != nil {
				return SpecBackup{}, fmt.Errorf("快照 Cron 表达式无效: %w", err)
			}
		}
		out.Snapshot = &s
	}
	if in.Export != nil {
		e := *in.Export
		e.Cron = strings.TrimSpace(e.Cron)
		e.Tag = sanitizeTag(e.Tag, "auto")
		e.Format = strings.ToLower(strings.TrimSpace(e.Format))
		if e.Format == "" {
			e.Format = "tar.zst"
		}
		if e.Format != "tar.zst" && e.Format != "tar.gz" {
			return SpecBackup{}, fmt.Errorf("不支持的归档格式: %s（支持 tar.zst / tar.gz）", e.Format)
		}
		if e.KeepCount < 0 || e.KeepDays < 0 {
			return SpecBackup{}, fmt.Errorf("镜像归档保留份数和保留天数不能为负数")
		}
		if e.Enabled {
			if _, err := ParseCron(e.Cron); err != nil {
				return SpecBackup{}, fmt.Errorf("镜像归档 Cron 表达式无效: %w", err)
			}
		} else if e.Cron != "" {
			if _, err := ParseCron(e.Cron); err != nil {
				return SpecBackup{}, fmt.Errorf("镜像归档 Cron 表达式无效: %w", err)
			}
		}
		out.Export = &e
	}
	return out, nil
}

func defaultBackupWithDefaults(b *SpecBackup) SpecBackup {
	var out SpecBackup
	if b != nil && b.Snapshot != nil {
		s := *b.Snapshot
		if s.Tag == "" {
			s.Tag = "auto"
		}
		out.Snapshot = &s
	} else {
		out.Snapshot = &SnapshotScheduleSpec{
			Enabled:   false,
			Cron:      "0 3 * * *",
			Tag:       "auto",
			ReadOnly:  true,
			KeepCount: 7,
		}
	}
	if b != nil && b.Export != nil {
		e := *b.Export
		if e.Tag == "" {
			e.Tag = "auto"
		}
		if e.Format == "" {
			e.Format = "tar.zst"
		}
		out.Export = &e
	} else {
		out.Export = &ExportScheduleSpec{
			Enabled:   false,
			Cron:      "0 4 * * 0",
			Tag:       "auto",
			Format:    "tar.zst",
			KeepCount: 3,
		}
	}
	return out
}

func (m *Manager) syncContainerSchedule(machine string, backup *SpecBackup) {
	if machine == "" {
		return
	}
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	if m.schedRules == nil {
		m.schedRules = make(map[string]SpecBackup)
	}
	hasEnabled := backup != nil && ((backup.Snapshot != nil && backup.Snapshot.Enabled) || (backup.Export != nil && backup.Export.Enabled))
	if hasEnabled {
		m.schedRules[machine] = *cloneSpecBackup(backup)
	} else {
		delete(m.schedRules, machine)
	}
}

func (m *Manager) syncAllSchedulesFromContainers(list []ContainerInfo) {
	next := make(map[string]SpecBackup)
	for _, item := range list {
		b := item.Config.Backup
		if b == nil {
			continue
		}
		if (b.Snapshot != nil && b.Snapshot.Enabled) || (b.Export != nil && b.Export.Enabled) {
			next[item.Config.Machine] = *cloneSpecBackup(b)
		}
	}
	m.schedMu.Lock()
	m.schedRules = next
	m.schedMu.Unlock()
}

func (m *Manager) RefreshBackupSchedules() {
	scanRoots := effectiveScanRoots(m.cfgStore.Get())
	next := make(map[string]SpecBackup)
	for _, baseRoot := range scanRoots {
		matches, _ := filepath.Glob(filepath.Join(baseRoot, "*", "config", ContainerSpecFileName))
		for _, specPath := range matches {
			spec, err := ReadContainerSpec(specPath)
			if err != nil || spec.Backup == nil {
				continue
			}
			b := spec.Backup
			if (b.Snapshot != nil && b.Snapshot.Enabled) || (b.Export != nil && b.Export.Enabled) {
				next[spec.Machine] = *cloneSpecBackup(b)
			}
		}
	}
	m.schedMu.Lock()
	m.schedRules = next
	m.schedMu.Unlock()
}

func (m *Manager) GetContainerBackup(machine string) (*ContainerBackupResponse, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	b := defaultBackupWithDefaults(info.Config.Backup)

	m.schedMu.Lock()
	snapStat := m.schedStatus[machine+":snapshot"]
	expStat := m.schedStatus[machine+":export"]
	m.schedMu.Unlock()

	if b.Snapshot != nil && b.Snapshot.Cron != "" {
		if runs, err := PreviewCronRuns(b.Snapshot.Cron, 3); err == nil {
			snapStat.NextRuns = runs
		}
	}
	if b.Export != nil && b.Export.Cron != "" {
		if runs, err := PreviewCronRuns(b.Export.Cron, 3); err == nil {
			expStat.NextRuns = runs
		}
	}

	return &ContainerBackupResponse{
		Machine:        machine,
		Backup:         b,
		SnapshotStatus: snapStat,
		ExportStatus:   expStat,
	}, nil
}

func (m *Manager) SaveContainerBackup(machine string, raw SpecBackup) (*ContainerBackupResponse, error) {
	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	cfg := info.Config
	if cfg.Layout == LayoutExternal {
		return nil, fmt.Errorf("外部运行容器不支持配置定时备份，请先迁移为标准容器")
	}
	if cfg.ConfDir == "" {
		return nil, fmt.Errorf("当前容器缺少配置目录 (config/)")
	}

	clean, err := normalizeSpecBackup(raw)
	if err != nil {
		return nil, err
	}

	specPath := cfg.ConfigFile
	if specPath == "" {
		specPath = filepath.Join(cfg.ConfDir, ContainerSpecFileName)
	}
	var existingSpec *ContainerSpec
	if sp, err := ReadContainerSpec(specPath); err == nil {
		existingSpec = sp
	}
	cfg.Backup = &clean
	spec := ConfigToSpec(cfg, existingSpec)
	spec.Backup = &clean
	if err := WriteContainerSpec(specPath, spec); err != nil {
		return nil, fmt.Errorf("写入 container.json 失败: %w", err)
	}

	m.syncContainerSchedule(machine, &clean)
	return m.GetContainerBackup(machine)
}

func (m *Manager) StartScheduledBackupTask(machine, kind, trigger string) (*TaskRecord, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "snapshot" && kind != "export" {
		return nil, fmt.Errorf("不支持的定时任务类型: %s", kind)
	}

	info, err := m.GetContainer(machine)
	if err != nil {
		return nil, err
	}
	if info.Config.Layout == LayoutExternal {
		return nil, fmt.Errorf("外部运行容器不支持自动备份任务")
	}
	b := defaultBackupWithDefaults(info.Config.Backup)

	runKey := machine + ":" + kind
	m.schedMu.Lock()
	if m.schedRunning == nil {
		m.schedRunning = make(map[string]bool)
	}
	if m.schedRunning[runKey] {
		m.schedMu.Unlock()
		return nil, fmt.Errorf("容器 %s 当前已有正在执行的 %s 任务，已跳过本次触发", machine, kind)
	}
	m.schedRunning[runKey] = true
	m.schedMu.Unlock()

	taskType := "auto-snapshot"
	if kind == "export" {
		taskType = "auto-export"
	}
	task := m.tasks.Create(taskType, machine)

	go func() {
		var taskErr error
		var summary string
		started := time.Now()
		defer func() {
			m.schedMu.Lock()
			delete(m.schedRunning, runKey)
			if m.schedStatus == nil {
				m.schedStatus = make(map[string]ScheduleRunStatus)
			}
			st := ScheduleRunStatus{
				LastRunAt:   started.Format("2006-01-02 15:04:05"),
				LastStatus:  "success",
				LastSummary: summary,
			}
			if taskErr != nil {
				st.LastStatus = "failed"
				st.LastError = taskErr.Error()
			}
			m.schedStatus[runKey] = st
			m.schedMu.Unlock()
			m.tasks.Finish(task.ID, taskErr)
		}()

		logFn := func(msg string) {
			m.tasks.AppendLog(task.ID, msg)
		}
		trigLabel := "定时调度"
		if trigger == "manual" {
			trigLabel = "手动触发"
		}

		if kind == "snapshot" {
			rule := b.Snapshot
			logFn(fmt.Sprintf("[%s] 开始执行 Btrfs 自动快照 (tag=%s, ro=%v, keepCount=%d, keepDays=%d)",
				trigLabel, rule.Tag, rule.ReadOnly, rule.KeepCount, rule.KeepDays))
			sn, err := m.CreateSnapshot(machine, CreateSnapshotRequest{
				Tag:       rule.Tag,
				ReadOnly:  rule.ReadOnly,
				StopFirst: rule.StopFirst,
			})
			if err != nil {
				taskErr = err
				return
			}
			logFn("[OK] 已创建快照: " + sn.Name)

			snaps, err := m.ListSnapshots(machine)
			pruned := 0
			if err == nil {
				names := make([]string, 0, len(snaps))
				for _, item := range snaps {
					names = append(names, item.Name)
				}
				expired := selectExpiredBackupEntries(machine, rule.Tag, names, rule.KeepCount, rule.KeepDays, time.Now())
				for _, oldName := range expired {
					if delErr := m.DeleteSnapshot(machine, oldName); delErr != nil {
						logFn(fmt.Sprintf("[WARN] 清理过期快照 %s 失败: %v", oldName, delErr))
					} else {
						pruned++
						logFn("[PRUNE] 已按保留策略删除过期快照: " + oldName)
					}
				}
			}
			summary = fmt.Sprintf("新建 %s（清理 %d 份过期快照）", sn.Name, pruned)
			logFn("[DONE] " + summary)
			return
		}

		// kind == "export"
		rule := b.Export
		logFn(fmt.Sprintf("[%s] 开始执行镜像自动归档 (tag=%s, format=%s, keepCount=%d, keepDays=%d)",
			trigLabel, rule.Tag, rule.Format, rule.KeepCount, rule.KeepDays))
		outPath, err := m.exportImageSync(logFn, info, ExportImageRequest{
			Tag:       rule.Tag,
			Format:    rule.Format,
			StopFirst: rule.StopFirst,
		})
		if err != nil {
			taskErr = err
			return
		}
		imgs, err := m.ListImages(machine)
		pruned := 0
		if err == nil {
			names := make([]string, 0, len(imgs))
			for _, item := range imgs {
				names = append(names, item.Name)
			}
			expired := selectExpiredBackupEntries(machine, rule.Tag, names, rule.KeepCount, rule.KeepDays, time.Now())
			for _, oldName := range expired {
				if delErr := m.DeleteImage(machine, oldName); delErr != nil {
					logFn(fmt.Sprintf("[WARN] 清理过期归档 %s 失败: %v", oldName, delErr))
				} else {
					pruned++
					logFn("[PRUNE] 已按保留策略删除过期归档: " + oldName)
				}
			}
		}
		summary = fmt.Sprintf("导出 %s（清理 %d 份过期归档）", filepath.Base(outPath), pruned)
		logFn("[DONE] " + summary)
	}()

	return task, nil
}

func (m *Manager) StartScheduler(ctx context.Context) {
	m.RefreshBackupSchedules()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.runSchedulerTick(now)
		}
	}
}

func (m *Manager) runSchedulerTick(now time.Time) {
	minuteKey := now.Format("2006-01-02 15:04")

	m.schedMu.Lock()
	if m.schedLastMinute == nil {
		m.schedLastMinute = make(map[string]string)
	}
	type dueJob struct {
		machine string
		kind    string
	}
	var due []dueJob
	for machine, b := range m.schedRules {
		if b.Snapshot != nil && b.Snapshot.Enabled {
			if cs, err := ParseCron(b.Snapshot.Cron); err == nil && cs.Matches(now) {
				k := machine + ":snapshot"
				if m.schedLastMinute[k] != minuteKey {
					m.schedLastMinute[k] = minuteKey
					due = append(due, dueJob{machine: machine, kind: "snapshot"})
				}
			}
		}
		if b.Export != nil && b.Export.Enabled {
			if cs, err := ParseCron(b.Export.Cron); err == nil && cs.Matches(now) {
				k := machine + ":export"
				if m.schedLastMinute[k] != minuteKey {
					m.schedLastMinute[k] = minuteKey
					due = append(due, dueJob{machine: machine, kind: "export"})
				}
			}
		}
	}
	m.schedMu.Unlock()

	for _, job := range due {
		if _, err := m.StartScheduledBackupTask(job.machine, job.kind, "cron"); err != nil {
			log.Printf("[scheduler] 跳过触发 %s (%s): %v", job.machine, job.kind, err)
		}
	}
}
