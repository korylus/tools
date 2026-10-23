package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDirSaveLoad(t *testing.T) {
	t.Parallel()

	d := NewDir(t.TempDir(), "koryluspilot")
	if _, err := d.Load(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("実行前のLoadのエラー = %v、期待値 = fs.ErrNotExist", err)
	}

	now := time.Date(2026, 10, 18, 9, 30, 0, 0, time.FixedZone("JST", 9*60*60))
	in := &State{
		Task:     "2-1",
		Step:     StepReview,
		Reviews:  2,
		Branch:   "koryluspilot-2-1",
		Start:    "koryluspilot-1-5",
		Tasks:    []TaskRecord{{ID: "1-5", Branch: "koryluspilot-1-5", Start: "koryluspilot-base", Commit: "abc"}},
		Sessions: map[Step]string{StepImplement: "session-a", StepReview: "session-b"},
		Stop: &Stop{
			Reason:  "宣言の無い保護領域への変更",
			Details: []string{"schema: go/db/migrations/1.sql"},
			Role:    "implementer",
			At:      now,
		},
		ReportVersion: 1,
	}
	if err := d.Save(in, now); err != nil {
		t.Fatalf("Saveのエラー = %v", err)
	}
	if !in.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt = %v、期待値 = %v", in.UpdatedAt, now)
	}

	got, err := d.Load()
	if err != nil {
		t.Fatalf("Loadのエラー = %v", err)
	}
	if !got.UpdatedAt.Equal(now) || !got.Stop.At.Equal(now) {
		t.Errorf("時刻が往復で変わった: UpdatedAt = %v、Stop.At = %v", got.UpdatedAt, got.Stop.At)
	}
	// 時刻はタイムゾーンの表現が往復で変わりうるため、比較から外す。
	got.UpdatedAt, got.Stop.At = in.UpdatedAt, in.Stop.At
	if !reflect.DeepEqual(got, in) {
		t.Errorf("Load() = %+v\n期待値 = %+v", got, in)
	}

	entries, err := os.ReadDir(d.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Errorf("状態ディレクトリに一時ファイルが残った: %v", entries)
	}
}

func TestDirLoad_Broken(t *testing.T) {
	t.Parallel()

	d := NewDir(t.TempDir(), "p")
	if err := os.MkdirAll(d.Path(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Path(), "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := d.Load()
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("壊れた状態ファイルのエラー = %v", err)
	}
}

func TestDirSnapshot(t *testing.T) {
	t.Parallel()

	d := NewDir(t.TempDir(), "p")
	if _, err := d.LoadSnapshot(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("取得前のLoadSnapshotのエラー = %v、期待値 = fs.ErrNotExist", err)
	}

	for _, src := range []string{"# 計画書\n", "# 計画書 (承認後)\n"} {
		if err := d.SaveSnapshot([]byte(src)); err != nil {
			t.Fatalf("SaveSnapshotのエラー = %v", err)
		}
		got, err := d.LoadSnapshot()
		if err != nil {
			t.Fatalf("LoadSnapshotのエラー = %v", err)
		}
		if string(got) != src {
			t.Errorf("LoadSnapshot() = %q、期待値 = %q", got, src)
		}
	}
}

func TestDirPaths(t *testing.T) {
	t.Parallel()

	d := NewDir(Root, "koryluspilot")
	if got, want := d.Path(), filepath.Join("tmp", "koryluspilot", "koryluspilot"); got != want {
		t.Errorf("Path() = %q、期待値 = %q", got, want)
	}
	if got, want := d.LogsPath(), filepath.Join("tmp", "koryluspilot", "koryluspilot", "logs"); got != want {
		t.Errorf("LogsPath() = %q、期待値 = %q", got, want)
	}
}

func TestStepLabel(t *testing.T) {
	t.Parallel()

	if got := StepImplement.Label(); got != "実装" {
		t.Errorf("Label() = %q、期待値 = %q", got, "実装")
	}
	if got := Step("unknown").Label(); got != "unknown" {
		t.Errorf("未知のステップのLabel() = %q、期待値 = %q", got, "unknown")
	}
}

func TestDirConfigSnapshot(t *testing.T) {
	t.Parallel()

	d := NewDir(t.TempDir(), "p")
	if _, err := d.LoadConfigSnapshot(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("取得前のLoadConfigSnapshotのエラー = %v、期待値 = fs.ErrNotExist", err)
	}
	src := "project = \"mewst\"\n"
	if err := d.SaveConfigSnapshot([]byte(src)); err != nil {
		t.Fatalf("SaveConfigSnapshotのエラー = %v", err)
	}
	got, err := d.LoadConfigSnapshot()
	if err != nil {
		t.Fatalf("LoadConfigSnapshotのエラー = %v", err)
	}
	if string(got) != src {
		t.Errorf("LoadConfigSnapshot() = %q、期待値 = %q", got, src)
	}
}

func TestDirEvents(t *testing.T) {
	t.Parallel()

	d := NewDir(t.TempDir(), "p")
	if got, err := d.ReadEvents(); err != nil || got != nil {
		t.Fatalf("記録の前のReadEvents() = %v, %v、期待値 = 空", got, err)
	}

	started := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	in := []Event{
		{Kind: EventAgent, Task: "1-1", Step: StepImplement, Role: "implementer", Tool: "claude", Started: started, Ended: started.Add(time.Minute), Outcome: OutcomeOK, Turns: 3, CostUSD: 0.5},
		{Kind: EventGate, Task: "1-1", Step: StepGate, Started: started, Ended: started.Add(time.Second), Outcome: OutcomeFailed, Command: "make test", ExitCode: 2},
	}
	if err := d.AppendEvent(in[0]); err != nil {
		t.Fatalf("AppendEventのエラー = %v", err)
	}
	// 書きかけで途切れた行が残っていても、次の記録と混ざらない。
	f, err := os.OpenFile(filepath.Join(d.Path(), "timeline.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"ag`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.AppendEvent(in[1]); err != nil {
		t.Fatalf("AppendEventのエラー = %v", err)
	}

	got, err := d.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEventsのエラー = %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("ReadEvents() = %+v\n期待値 = %+v", got, in)
	}
	if got := in[0].Duration(); got != time.Minute {
		t.Errorf("Duration() = %v、期待値 = %v", got, time.Minute)
	}
}
