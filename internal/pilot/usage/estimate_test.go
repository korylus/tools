package usage

import (
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/state"
)

var (
	baseTime = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	resetA   = baseTime.Add(3 * time.Hour)
	resetB   = baseTime.Add(8 * time.Hour)
)

// implementStepは実装のステップの5時間枠の消費の記録を作る。
func implementStep(before, after float64, beforeReset, afterReset time.Time) StepUsage {
	return StepUsage{
		Tool: agent.Claude, Step: state.StepImplement, Window: FiveHour,
		Before: before, After: after, BeforeResetsAt: beforeReset, AfterResetsAt: afterReset,
	}
}

func TestEstimate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		steps []StepUsage
		want  float64
	}{
		{name: "記録が無ければ初期値", steps: nil, want: 10},
		{
			name: "奇数件は中央の値",
			steps: []StepUsage{
				implementStep(10, 14, resetA, resetA),
				implementStep(14, 16, resetA, resetA),
				implementStep(16, 25, resetA, resetA),
			},
			want: 4,
		},
		{
			name: "偶数件は中央の2件の平均",
			steps: []StepUsage{
				implementStep(10, 12, resetA, resetA),
				implementStep(12, 16, resetA, resetA),
			},
			want: 3,
		},
		{
			name: "直近の5件だけを使う",
			steps: []StepUsage{
				implementStep(0, 50, resetA, resetA),
				implementStep(0, 50, resetA, resetA),
				implementStep(0, 50, resetA, resetA),
				implementStep(0, 1, resetA, resetA),
				implementStep(0, 1, resetA, resetA),
				implementStep(0, 1, resetA, resetA),
				implementStep(0, 1, resetA, resetA),
				implementStep(0, 1, resetA, resetA),
			},
			want: 1,
		},
		{
			name: "リセットをまたいだ差と減った差は使わない",
			steps: []StepUsage{
				implementStep(80, 2, resetA, resetB),
				implementStep(20, 19, resetA, resetA),
				implementStep(20, 26, resetA, resetA.Add(30*time.Second)),
			},
			want: 6,
		},
		{
			name: "他のツール・ステップ・枠の記録は使わない",
			steps: []StepUsage{
				{Tool: agent.Codex, Step: state.StepImplement, Window: FiveHour, Before: 0, After: 30, BeforeResetsAt: resetA, AfterResetsAt: resetA},
				{Tool: agent.Claude, Step: state.StepCommit, Window: FiveHour, Before: 0, After: 30, BeforeResetsAt: resetA, AfterResetsAt: resetA},
				{Tool: agent.Claude, Step: state.StepImplement, Window: Weekly, Before: 0, After: 30, BeforeResetsAt: resetA, AfterResetsAt: resetA},
			},
			want: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Estimate(tt.steps, agent.Claude, state.StepImplement, FiveHour, 10)
			if got != tt.want {
				t.Errorf("Estimate() = %g、期待値 = %g", got, tt.want)
			}
		})
	}
}

func TestEstimates(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultUsage()
	steps := []StepUsage{implementStep(10, 13, resetA, resetA)}
	got := Estimates(cfg, steps, agent.Claude, state.StepImplement)
	if got[FiveHour] != 3 {
		t.Errorf("5時間枠の見積もり = %g、期待値 = 3", got[FiveHour])
	}
	if got[Weekly] != cfg.WeeklyEstimate {
		t.Errorf("週枠の見積もり = %g、期待値 = %g (初期値)", got[Weekly], cfg.WeeklyEstimate)
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()

	// 上限は5時間枠90%・週枠80%、安全係数1.5。
	cfg := config.DefaultUsage()
	now := baseTime

	tests := []struct {
		name      string
		latest    map[Window]Sample
		estimates map[Window]float64
		want      Decision
	}{
		{
			name:      "上限に収まる",
			latest:    map[Window]Sample{FiveHour: {Percent: 75, ResetsAt: resetA}, Weekly: {Percent: 50, ResetsAt: resetB}},
			estimates: map[Window]float64{FiveHour: 10, Weekly: 2},
			want:      Decision{},
		},
		{
			name:      "ちょうど上限は起動してよい",
			latest:    map[Window]Sample{FiveHour: {Percent: 75, ResetsAt: resetA}},
			estimates: map[Window]float64{FiveHour: 10},
			want:      Decision{},
		},
		{
			name:      "超えそうな枠のリセットまで待つ",
			latest:    map[Window]Sample{FiveHour: {Percent: 76, ResetsAt: resetA}, Weekly: {Percent: 50, ResetsAt: resetB}},
			estimates: map[Window]float64{FiveHour: 10, Weekly: 2},
			want:      Decision{Wait: true, Until: resetA, Window: FiveHour, Percent: 76, Needed: 15, Cap: 90},
		},
		{
			name:      "複数の枠が超えそうならリセットが遅い枠まで待つ",
			latest:    map[Window]Sample{FiveHour: {Percent: 89, ResetsAt: resetA}, Weekly: {Percent: 79, ResetsAt: resetB}},
			estimates: map[Window]float64{FiveHour: 2, Weekly: 2},
			want:      Decision{Wait: true, Until: resetB, Window: Weekly, Percent: 79, Needed: 3, Cap: 80},
		},
		{
			name:      "リセットを過ぎた観測は0%として扱う",
			latest:    map[Window]Sample{FiveHour: {Percent: 95, ResetsAt: now.Add(-time.Minute)}},
			estimates: map[Window]float64{FiveHour: 10},
			want:      Decision{},
		},
		{
			name:      "リセット時刻が分からなければ一定時間待つ",
			latest:    map[Window]Sample{FiveHour: {Percent: 95}},
			estimates: map[Window]float64{FiveHour: 10},
			want:      Decision{Wait: true, Until: now.Add(unknownResetWait), Window: FiveHour, Percent: 95, Needed: 15, Cap: 90},
		},
		{
			name:      "0%でも収まらない見積もりでは待たない",
			latest:    map[Window]Sample{Weekly: {Percent: 0, ResetsAt: resetB}},
			estimates: map[Window]float64{Weekly: 60},
			want:      Decision{},
		},
		{
			name:      "観測の無い枠は判定しない",
			latest:    map[Window]Sample{},
			estimates: map[Window]float64{FiveHour: 100},
			want:      Decision{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Check(cfg, tt.latest, tt.estimates, now)
			if got != tt.want {
				t.Errorf("Check() = %+v、期待値 = %+v", got, tt.want)
			}
		})
	}
}

func TestRate(t *testing.T) {
	t.Parallel()

	sample := func(minutes int, percent float64, reset time.Time) Sample {
		return Sample{At: baseTime.Add(time.Duration(minutes) * time.Minute), Tool: agent.Claude, Window: FiveHour, Percent: percent, ResetsAt: reset}
	}
	now := baseTime.Add(2 * time.Hour)

	tests := []struct {
		name    string
		samples []Sample
		want    float64
		wantOK  bool
	}{
		{
			name:    "同じリセットの期間の最古と最新の差",
			samples: []Sample{sample(0, 10, resetA), sample(30, 12, resetA), sample(120, 16, resetA)},
			want:    3,
			wantOK:  true,
		},
		{
			name:    "前のリセットの期間と3時間より前の観測は使わない",
			samples: []Sample{sample(-300, 0, resetA), sample(0, 80, baseTime), sample(60, 10, resetA), sample(120, 12, resetA)},
			want:    2,
			wantOK:  true,
		},
		{
			name:    "間隔が短すぎると求めない",
			samples: []Sample{sample(115, 10, resetA), sample(120, 12, resetA)},
		},
		{
			name:    "減って見える差は0にする",
			samples: []Sample{sample(0, 12, resetA), sample(120, 11, resetA)},
			want:    0,
			wantOK:  true,
		},
		{name: "観測が無い"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := Rate(tt.samples, agent.Claude, FiveHour, now)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("Rate() = (%g, %v)、期待値 = (%g, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
