package usage

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/state"
)

func TestLog(t *testing.T) {
	t.Parallel()

	log := NewLog(t.TempDir())
	samples, steps, err := log.Read()
	if err != nil || samples != nil || steps != nil {
		t.Fatalf("記録が無いときのRead() = (%v, %v, %v)、空を期待", samples, steps, err)
	}

	wantSamples := []Sample{
		{At: baseTime, Tool: agent.Claude, Window: FiveHour, Percent: 3, ResetsAt: resetA, Source: SourceUsageCommand},
		{At: baseTime, Tool: agent.Codex, Window: Weekly, Percent: 5, Source: SourceAppServer},
	}
	wantSteps := []StepUsage{implementStep(3, 7, resetA, resetA)}
	if err := log.AppendSamples(wantSamples); err != nil {
		t.Fatal(err)
	}
	if err := log.AppendSteps(wantSteps); err != nil {
		t.Fatal(err)
	}
	// 書き込みの途中で落ちて途切れた行は読み飛ばす。
	f, err := os.OpenFile(log.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"sample":{"at":"2026-`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	samples, steps, err = log.Read()
	if err != nil {
		t.Fatalf("Readのエラー = %v", err)
	}
	if !reflect.DeepEqual(samples, wantSamples) {
		t.Errorf("観測 = %+v、期待値 = %+v", samples, wantSamples)
	}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Errorf("ステップの消費 = %+v、期待値 = %+v", steps, wantSteps)
	}
}

func TestLatest(t *testing.T) {
	t.Parallel()

	samples := []Sample{
		{At: baseTime.Add(time.Minute), Tool: agent.Claude, Window: FiveHour, Percent: 5},
		{At: baseTime, Tool: agent.Claude, Window: FiveHour, Percent: 3},
		{At: baseTime, Tool: agent.Claude, Window: Weekly, Percent: 20},
		{At: baseTime.Add(time.Hour), Tool: agent.Codex, Window: FiveHour, Percent: 50},
	}
	got := Latest(samples, agent.Claude)
	want := map[Window]Sample{FiveHour: samples[0], Weekly: samples[2]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Latest() = %+v、期待値 = %+v", got, want)
	}
}

func TestStepUsages(t *testing.T) {
	t.Parallel()

	before := map[Window]Sample{
		FiveHour: {At: baseTime, Percent: 10, ResetsAt: resetA},
		Weekly:   {At: baseTime, Percent: 30, ResetsAt: resetB},
	}
	after := map[Window]Sample{FiveHour: {At: baseTime.Add(time.Hour), Percent: 14, ResetsAt: resetA}}
	at := baseTime.Add(time.Hour)

	got := StepUsages(agent.Claude, state.StepImplement, before, after, at)
	want := []StepUsage{{
		At: at, Tool: agent.Claude, Step: state.StepImplement, Window: FiveHour,
		Before: 10, After: 14, BeforeResetsAt: resetA, AfterResetsAt: resetA,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StepUsages() = %+v、期待値 = %+v", got, want)
	}
}

// fakeFetcherは決まった観測を返し、呼ばれた回数を数える。
type fakeFetcher struct {
	samples []Sample
	err     error
	calls   int
}

func (f *fakeFetcher) Fetch(_ context.Context, tool agent.Tool, now time.Time) ([]Sample, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return stamp(append([]Sample(nil), f.samples...), now, tool, SourceUsageCommand), nil
}

func TestTracker_Current(t *testing.T) {
	t.Parallel()

	now := baseTime
	tests := []struct {
		name      string
		recorded  []Sample
		wantFetch bool
	}{
		{name: "記録が無ければ取得する", wantFetch: true},
		{
			name:     "新しい記録はそのまま使う",
			recorded: []Sample{{At: now.Add(-5 * time.Minute), Tool: agent.Claude, Window: FiveHour, Percent: 40, ResetsAt: resetA}},
		},
		{
			name:      "古い記録は取得し直す",
			recorded:  []Sample{{At: now.Add(-11 * time.Minute), Tool: agent.Claude, Window: FiveHour, Percent: 40, ResetsAt: resetA}},
			wantFetch: true,
		},
		{
			name:      "リセットを過ぎた枠があれば取得し直す",
			recorded:  []Sample{{At: now.Add(-time.Minute), Tool: agent.Claude, Window: FiveHour, Percent: 40, ResetsAt: now}},
			wantFetch: true,
		},
		{
			name:      "他のツールの記録は使わない",
			recorded:  []Sample{{At: now, Tool: agent.Codex, Window: Weekly, Percent: 40}},
			wantFetch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			log := NewLog(t.TempDir())
			if err := log.AppendSamples(tt.recorded); err != nil {
				t.Fatal(err)
			}
			fetcher := &fakeFetcher{samples: []Sample{{Window: FiveHour, Percent: 41, ResetsAt: resetA}}}
			tracker := Tracker{Log: log, Fetcher: fetcher, Now: func() time.Time { return now }}

			got, err := tracker.Current(context.Background(), agent.Claude)
			if err != nil {
				t.Fatalf("Currentのエラー = %v", err)
			}
			wantPercent := 40.0
			if tt.wantFetch {
				wantPercent = 41
			}
			if fetched := fetcher.calls > 0; fetched != tt.wantFetch {
				t.Errorf("取得したか = %v、期待値 = %v", fetched, tt.wantFetch)
			}
			if got[FiveHour].Percent != wantPercent {
				t.Errorf("5時間枠の使用率 = %g、期待値 = %g", got[FiveHour].Percent, wantPercent)
			}

			// 取得した値は記録に残り、次の判定に使われる。
			samples, _, err := log.Read()
			if err != nil {
				t.Fatal(err)
			}
			if want := len(tt.recorded) + fetcher.calls; len(samples) != want {
				t.Errorf("記録の件数 = %d、期待値 = %d", len(samples), want)
			}
		})
	}
}

func TestTracker_RefreshError(t *testing.T) {
	t.Parallel()

	log := NewLog(t.TempDir())
	want := errors.New("取得に失敗")
	tracker := Tracker{Log: log, Fetcher: &fakeFetcher{err: want}}
	if _, err := tracker.Refresh(context.Background(), agent.Codex); !errors.Is(err, want) {
		t.Errorf("Refreshのエラー = %v、期待値 = %v", err, want)
	}
	if _, err := os.Stat(log.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("取得に失敗したのに記録が書かれた: %v", err)
	}
}

func TestTracker_RecordClaudeStream(t *testing.T) {
	t.Parallel()

	log := NewLog(t.TempDir())
	tracker := Tracker{Log: log, Fetcher: &fakeFetcher{}, Now: func() time.Time { return baseTime }}
	got, err := tracker.RecordClaudeStream(strings.NewReader(claudeStream))
	if err != nil {
		t.Fatalf("RecordClaudeStreamのエラー = %v", err)
	}
	want := Sample{At: baseTime, Tool: agent.Claude, Window: FiveHour, Percent: 7, ResetsAt: time.Unix(1792411800, 0).UTC(), Source: SourceStream}
	if got[FiveHour] != want {
		t.Errorf("5時間枠 = %+v、期待値 = %+v", got[FiveHour], want)
	}
	samples, _, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Errorf("記録の件数 = %d、期待値 = 2", len(samples))
	}
}

func TestLog_AppendAfterPartialLine(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"観測", "ステップの消費"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			log := NewLog(t.TempDir())
			wantSamples := []Sample{{At: baseTime, Tool: agent.Claude, Window: FiveHour, Percent: 3}}
			wantSteps := []StepUsage{implementStep(3, 7, resetA, resetA)}
			if err := log.AppendSamples(wantSamples); err != nil {
				t.Fatal(err)
			}
			if err := log.AppendSteps(wantSteps); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(log.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := f.WriteString(`{"sample":{"at":"2026-`)
			closeErr := f.Close()
			if err := errors.Join(writeErr, closeErr); err != nil {
				t.Fatal(err)
			}

			if kind == "観測" {
				added := []Sample{
					{At: baseTime.Add(time.Minute), Tool: agent.Claude, Window: FiveHour, Percent: 7},
					{At: baseTime.Add(time.Minute), Tool: agent.Claude, Window: Weekly, Percent: 20},
				}
				if err := log.AppendSamples(added); err != nil {
					t.Fatal(err)
				}
				wantSamples = append(wantSamples, added...)
			} else {
				added := []StepUsage{implementStep(7, 9, resetA, resetA), implementStep(9, 12, resetA, resetA)}
				if err := log.AppendSteps(added); err != nil {
					t.Fatal(err)
				}
				wantSteps = append(wantSteps, added...)
			}

			samples, steps, err := log.Read()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(samples, wantSamples) {
				t.Errorf("観測 = %+v、期待値 = %+v", samples, wantSamples)
			}
			if !reflect.DeepEqual(steps, wantSteps) {
				t.Errorf("ステップの消費 = %+v、期待値 = %+v", steps, wantSteps)
			}
		})
	}
}
