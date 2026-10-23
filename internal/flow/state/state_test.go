package state

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDirSaveLoad(t *testing.T) {
	t.Parallel()

	d := NewDir(t.TempDir(), "korylusflow")
	if _, err := d.Load(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("初回のLoadのエラー = %v、期待値 = fs.ErrNotExist", err)
	}

	now := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	in := &State{
		Task:    "2-2",
		Step:    StepReview,
		Role:    RoleReviewerCodex,
		Since:   now.Add(-time.Hour),
		Pending: &Event{Task: "2-1", Step: StepImplement, Role: RoleImplementer, Started: now.Add(-2 * time.Hour), Ended: now.Add(-time.Hour)},
		Ask:     &Ask{Reason: "要人間判断の指摘があります", Details: []string{"internal/flow/state/state.go"}},
		Tasks: []TaskRecord{{
			ID:      "2-2",
			Repo:    "/korylus-tools",
			Dir:     "/korylus-tools",
			Branch:  "korylusflow-2-2",
			Base:    "korylusflow-2-1",
			Reviews: []ReviewRecord{{Number: 1, Role: RoleReviewerCodex, Doc: "docs/private/reviews/a-001.md", Verdict: "Request Changes", At: now}},
			Verification: []VerificationItem{{
				Kind: "process", Title: "statusで進行状況を表示できる", Target: "korylusflow status x.md",
				Steps: []string{"実行する"}, Expected: "タスクの一覧が出る",
			}},
		}},
	}
	if err := d.Save(in, now); err != nil {
		t.Fatalf("Saveのエラー = %v", err)
	}

	got, err := d.Load()
	if err != nil {
		t.Fatalf("Loadのエラー = %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("Load() = %+v\n期待値 = %+v", got, in)
	}

	// 一時ファイルを置き換えたあとに、書きかけのファイルが残っていないこと。
	entries, err := os.ReadDir(d.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("一時ファイル %s が残っている", e.Name())
		}
	}
}

func TestDirLoadBroken(t *testing.T) {
	t.Parallel()

	d := NewDir(t.TempDir(), "korylusflow")
	if err := os.MkdirAll(d.Path(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Path(), stateFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Load(); err == nil {
		t.Error("壊れた状態ファイルでエラーを期待したが、nilだった")
	}
}

func TestDirLock(t *testing.T) {
	t.Parallel()

	d := NewDir(t.TempDir(), "korylusflow")
	unlock, err := d.Lock()
	if err != nil {
		t.Fatalf("Lockのエラー = %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		unlock2, err := d.Lock()
		if err != nil {
			t.Errorf("2つ目のLockのエラー = %v", err)
			close(acquired)
			return
		}
		close(acquired)
		_ = unlock2()
	}()

	select {
	case <-acquired:
		t.Fatal("ロックを持っている間に、2つ目のLockが取れた")
	case <-time.After(100 * time.Millisecond):
	}

	if err := unlock(); err != nil {
		t.Fatalf("unlockのエラー = %v", err)
	}
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("ロックを外したあとも、2つ目のLockが取れない")
	}
}

func TestDirLockContext(t *testing.T) {
	t.Parallel()

	d := NewDir(t.TempDir(), "korylusflow")
	unlock, err := d.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		unlock2, err := d.LockContext(ctx)
		if err == nil {
			_ = unlock2()
		}
		finished <- err
	}()
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("LockContextのエラー = %v、期待値 = context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("キャンセルしてもロック待ちが終わらない")
	}
}

func TestParseRole(t *testing.T) {
	t.Parallel()

	for _, r := range Roles {
		got, err := ParseRole(string(r))
		if err != nil || got != r {
			t.Errorf("ParseRole(%q) = %q, %v、期待値 = %q", r, got, err, r)
		}
	}
	if _, err := ParseRole("reviewer"); err == nil {
		t.Error("不明な役割でエラーを期待したが、nilだった")
	}
}

func TestStateTaskRecord(t *testing.T) {
	t.Parallel()

	s := &State{Tasks: []TaskRecord{{ID: "1-1"}, {ID: "1-2"}}}
	rec := s.TaskRecord("1-2")
	if rec == nil {
		t.Fatal("TaskRecord(\"1-2\") = nil")
	}
	// 返した記録への変更が状態に反映されること。
	rec.Branch = "x-1-2"
	if s.Tasks[1].Branch != "x-1-2" {
		t.Errorf("Tasks[1].Branch = %q、期待値 = %q", s.Tasks[1].Branch, "x-1-2")
	}
	if s.TaskRecord("2-1") != nil {
		t.Error("扱っていないタスクでnilを期待した")
	}
}

func TestDirEvents(t *testing.T) {
	t.Parallel()

	d := NewDir(t.TempDir(), "korylusflow")
	if events, err := d.ReadEvents(); err != nil || events != nil {
		t.Fatalf("記録が無いときのReadEvents() = %v, %v", events, err)
	}

	start := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	want := []Event{
		{Task: "1-1", Step: StepImplement, Role: RoleImplementer, Started: start, Ended: start.Add(20 * time.Minute)},
		{Task: "1-1", Step: StepAsk, Role: RoleReviewerCodex, Started: start.Add(30 * time.Minute), Ended: start.Add(45 * time.Minute)},
	}
	if err := d.AppendEvent(want[0]); err != nil {
		t.Fatal(err)
	}
	// 同じステップの再試行では、終了時刻が変わっても追記しない。
	retry := want[0]
	retry.Ended = retry.Ended.Add(time.Minute)
	if err := d.AppendEvent(retry); err != nil {
		t.Fatal(err)
	}
	// 書きかけで途切れた行は読み飛ばす。
	f, err := os.OpenFile(filepath.Join(d.Path(), timelineFile), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"task":"1-1","st`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.AppendEvent(want[1]); err != nil {
		t.Fatal(err)
	}

	got, err := d.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEventsのエラー = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadEvents() = %+v\n期待値 = %+v", got, want)
	}
	if d := got[1].Duration(); d != 15*time.Minute {
		t.Errorf("Duration() = %v、期待値 = 15m", d)
	}
}
