package nspawn

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type TaskManager struct {
	mu    sync.RWMutex
	tasks map[string]*TaskRecord
	order []string
	seq   uint64
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		tasks: make(map[string]*TaskRecord),
	}
}

func (tm *TaskManager) Create(taskType, target string) *TaskRecord {
	idNum := atomic.AddUint64(&tm.seq, 1)
	id := fmt.Sprintf("task-%d-%d", time.Now().Unix(), idNum)
	rec := &TaskRecord{
		ID:        id,
		Type:      taskType,
		Target:    target,
		Status:    TaskRunning,
		StartedAt: time.Now(),
		Logs:      make([]string, 0, 64),
	}

	tm.mu.Lock()
	tm.tasks[id] = rec
	tm.order = append(tm.order, id)
	// Keep up to 50 recent tasks in memory
	if len(tm.order) > 50 {
		oldest := tm.order[0]
		tm.order = tm.order[1:]
		delete(tm.tasks, oldest)
	}
	tm.mu.Unlock()
	return rec
}

func (tm *TaskManager) AppendLog(id string, line string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if rec, ok := tm.tasks[id]; ok {
		ts := time.Now().Format("15:04:05")
		rec.Logs = append(rec.Logs, fmt.Sprintf("[%s] %s", ts, line))
	}
}

func (tm *TaskManager) Finish(id string, err error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if rec, ok := tm.tasks[id]; ok {
		now := time.Now()
		rec.EndedAt = &now
		if err != nil {
			rec.Status = TaskFailed
			rec.Error = err.Error()
			rec.Logs = append(rec.Logs, fmt.Sprintf("[%s] [ERROR] %v", now.Format("15:04:05"), err))
		} else {
			rec.Status = TaskSuccess
			rec.Logs = append(rec.Logs, fmt.Sprintf("[%s] [DONE] 任务完成", now.Format("15:04:05")))
		}
	}
}

func (tm *TaskManager) Get(id string) (*TaskRecord, bool) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	rec, ok := tm.tasks[id]
	if !ok {
		return nil, false
	}
	cp := *rec
	cp.Logs = append([]string(nil), rec.Logs...)
	return &cp, true
}

func (tm *TaskManager) List() []TaskRecord {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	out := make([]TaskRecord, 0, len(tm.order))
	for i := len(tm.order) - 1; i >= 0; i-- {
		if rec, ok := tm.tasks[tm.order[i]]; ok {
			cp := *rec
			cp.Logs = append([]string(nil), rec.Logs...)
			out = append(out, cp)
		}
	}
	return out
}
