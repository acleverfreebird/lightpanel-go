package sysinfo

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func waitTask(t *testing.T, m *TaskManager, task *Task) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		state := task.State
		m.mu.Unlock()
		if state == TaskDone || state == TaskError {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not finish (state %q)", state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTaskManagerLifecycle(t *testing.T) {
	m := &TaskManager{}
	task := m.Start("kind", "key", "title", func(ctx context.Context, append func(string)) error {
		append("step one\n")
		append("step two\n")
		return nil
	})
	if task.State != TaskRunning {
		t.Fatalf("fresh task state = %q, want running", task.State)
	}
	if task.ID == "" {
		t.Fatal("task must get an id")
	}
	if state := waitTask(t, m, task); state != TaskDone {
		t.Fatalf("task state = %q, want done", state)
	}
	if task.Output != "step one\nstep two\n" {
		t.Fatalf("output = %q", task.Output)
	}
}

func TestTaskManagerErrorKeepsOutput(t *testing.T) {
	m := &TaskManager{}
	task := m.Start("kind", "key", "title", func(ctx context.Context, append func(string)) error {
		append("partial\n")
		return errors.New("boom")
	})
	if state := waitTask(t, m, task); state != TaskError {
		t.Fatalf("task state = %q, want error", state)
	}
	if task.Error != "boom" || task.Output != "partial\n" {
		t.Fatalf("error/output = %q/%q", task.Error, task.Output)
	}
}

func TestTaskManagerRunningAndClear(t *testing.T) {
	block := make(chan struct{})
	m := &TaskManager{}
	running := m.Start("kind", "key", "busy", func(ctx context.Context, append func(string)) error {
		<-block
		return nil
	})
	if !m.Running("kind", "key") {
		t.Fatal("Running = false for active task")
	}
	if m.Running("kind", "other") {
		t.Fatal("Running = true for different key")
	}
	rec := httptest.NewRecorder()
	m.ClearFinished(rec, httptest.NewRequest("POST", "/api/tasks/clear", nil))
	if rec.Code != 200 {
		t.Fatalf("clear status = %d", rec.Code)
	}
	m.mu.Lock()
	count := len(m.tasks)
	m.mu.Unlock()
	if count != 1 {
		t.Fatalf("clear removed a running task (count = %d)", count)
	}
	close(block)
	if state := waitTask(t, m, running); state != TaskDone {
		t.Fatalf("task state = %q", state)
	}
	rec = httptest.NewRecorder()
	m.ClearFinished(rec, httptest.NewRequest("POST", "/api/tasks/clear", nil))
	m.mu.Lock()
	count = len(m.tasks)
	m.mu.Unlock()
	if count != 0 {
		t.Fatalf("clear left %d finished tasks", count)
	}
}

func TestTaskManagerPrune(t *testing.T) {
	m := &TaskManager{}
	for i := 0; i < taskHistoryLimit+10; i++ {
		task := m.Start("kind", "", "t", func(context.Context, func(string)) error { return nil })
		waitTask(t, m, task)
	}
	m.mu.Lock()
	count := len(m.tasks)
	m.mu.Unlock()
	if count != taskHistoryLimit {
		t.Fatalf("history = %d tasks, want %d", count, taskHistoryLimit)
	}
}

func TestTaskDetailUnknownID(t *testing.T) {
	m := &TaskManager{}
	rec := httptest.NewRecorder()
	m.TaskDetail(rec, httptest.NewRequest("GET", "/api/tasks/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown task status = %d, want 404", rec.Code)
	}
}
