package sysinfo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// Task center: a small in-memory registry of background operations. Long
// work (package installs, certificate issuance) runs in a goroutine instead
// of inside the HTTP request, and the admin watches progress from the task
// center dialog. Tasks are state only — nothing is persisted, so a panel
// restart clears the history.

const (
	TaskRunning = "running"
	TaskDone    = "done"
	TaskError   = "error"
)

const taskHistoryLimit = 100

// Task is one background operation. Output is the combined command log,
// appended incrementally when the execution path supports it.
type Task struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	State     string    `json:"state"` // running, done, error
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Output    string    `json:"output,omitempty"`
	// Key identifies the operation target (app name, domain) so handlers can
	// reject duplicate concurrent work; it is never exposed to clients.
	Key string `json:"-"`
}

// TaskFunc runs the task body. It owns its own deadlines and reports log
// chunks through append, which is safe to call from any goroutine.
type TaskFunc func(ctx context.Context, append func(string)) error

// TaskManager holds tasks newest first. The zero value is ready to use.
type TaskManager struct {
	mu    sync.Mutex
	tasks []*Task
}

// Start registers a task and runs it in the background, returning the
// created task (state "running") immediately.
func (m *TaskManager) Start(kind, key, title string, run TaskFunc) *Task {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	task := &Task{
		ID:        hex.EncodeToString(id),
		Kind:      kind,
		Key:       key,
		Title:     title,
		State:     TaskRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	m.mu.Lock()
	m.tasks = append([]*Task{task}, m.tasks...)
	m.pruneLocked()
	m.mu.Unlock()
	go func() {
		appendOut := func(chunk string) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if left := outputLimit - len(task.Output); left > 0 {
				if len(chunk) > left {
					chunk = chunk[:left]
				}
				task.Output += chunk
			}
			task.UpdatedAt = time.Now()
		}
		err := run(context.Background(), appendOut)
		m.mu.Lock()
		defer m.mu.Unlock()
		task.UpdatedAt = time.Now()
		if err != nil {
			task.State, task.Error = TaskError, err.Error()
		} else {
			task.State = TaskDone
		}
	}()
	return task
}

// Running reports whether a task of the given kind and key is active.
func (m *TaskManager) Running(kind, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.Kind == kind && t.Key == key && t.State == TaskRunning {
			return true
		}
	}
	return false
}

// ClearFinished removes done and error tasks, keeping running ones.
func (m *TaskManager) ClearFinished(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	kept := m.tasks[:0]
	for _, t := range m.tasks {
		if t.State == TaskRunning {
			kept = append(kept, t)
		}
	}
	m.tasks = kept
	m.mu.Unlock()
	JSON(w, map[string]string{"message": "cleared"})
}

// Tasks lists tasks newest first without output bodies.
func (m *TaskManager) Tasks(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	list := make([]Task, len(m.tasks))
	for i, t := range m.tasks {
		list[i] = *t
		list[i].Output = ""
	}
	m.mu.Unlock()
	JSON(w, struct {
		Tasks []Task `json:"tasks"`
	}{list})
}

// TaskDetail returns one task including its output log.
func (m *TaskManager) TaskDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.ID == id {
			JSON(w, *t)
			return
		}
	}
	http.Error(w, "unknown task", 404)
}

// pruneLocked drops the oldest finished tasks once the history cap is
// exceeded; running tasks are never removed.
func (m *TaskManager) pruneLocked() {
	finished := 0
	for _, t := range m.tasks {
		if t.State != TaskRunning {
			finished++
		}
	}
	for finished >= taskHistoryLimit {
		dropped := false
		for i := len(m.tasks) - 1; i >= 0; i-- {
			if m.tasks[i].State != TaskRunning {
				m.tasks = append(m.tasks[:i], m.tasks[i+1:]...)
				finished--
				dropped = true
				break
			}
		}
		if !dropped {
			return
		}
	}
}
