package usage

import (
	"slices"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/state"
)

// estimateSamplesは見積もりに使う、直近の消費の記録の件数。
const estimateSamples = 5

// unknownResetWaitは、上限を超えそうな枠のリセット時刻が分からないときに待つ時間。
// 待ったあとに使用率を取り直して判定し直す。
const unknownResetWait = 30 * time.Minute

// Estimateはtoolのstepが枠windowで消費する量を、直近の記録の中央値で見積もる。
// 使える記録が無ければinitialを返す。
func Estimate(steps []StepUsage, tool agent.Tool, step state.Step, window Window, initial float64) float64 {
	var recent []float64
	// 新しい記録から見るため、後ろから辿る。
	for i := len(steps) - 1; i >= 0 && len(recent) < estimateSamples; i-- {
		s := steps[i]
		if s.Tool != tool || s.Step != step || s.Window != window {
			continue
		}
		if v, ok := s.Consumed(); ok {
			recent = append(recent, v)
		}
	}
	if len(recent) == 0 {
		return initial
	}
	slices.Sort(recent)
	n := len(recent)
	if n%2 == 1 {
		return recent[n/2]
	}
	return (recent[n/2-1] + recent[n/2]) / 2
}

// Estimatesはtoolのstepについて、枠ごとの消費量の見積もりを返す。
func Estimates(cfg config.Usage, steps []StepUsage, tool agent.Tool, step state.Step) map[Window]float64 {
	return map[Window]float64{
		FiveHour: Estimate(steps, tool, step, FiveHour, cfg.FiveHourEstimate),
		Weekly:   Estimate(steps, tool, step, Weekly, cfg.WeeklyEstimate),
	}
}

// Capは枠windowについて、自動実行が使ってよい使用率の上限を返す。
func Cap(cfg config.Usage, window Window) float64 {
	if window == FiveHour {
		return cfg.FiveHourCap
	}
	return cfg.WeeklyCap
}

// Decisionはエージェントを起動する前の判定の結果。
type Decision struct {
	// Waitがtrueなら、Untilまで待ってから使用率を取り直して判定し直す。
	Wait  bool
	Until time.Time
	// Windowは上限を超えそうな枠。Untilはこの枠のリセット時刻になる。
	Window Window
	// Percentは判定に使った使用率、Neededは見積もりに安全係数を掛けた値、Capは上限。
	Percent float64
	Needed  float64
	Cap     float64
}

// Checkは枠ごとの最新の観測latestと見積もりestimatesから、すべての枠で
// 「現在の使用率 + 見積もり × 安全係数 ≤ 上限」を満たすかを判定する。
// 満たさない枠が複数あれば、リセットが最も遅い枠まで待つ。
//
// 使用率が0%でも満たさないときは、待ってもリセットで減らないため待たない
// (見積もりの初期値を上限より大きく設定したときに、待ち続けないようにする)。
func Check(cfg config.Usage, latest map[Window]Sample, estimates map[Window]float64, now time.Time) Decision {
	var d Decision
	for _, w := range Windows {
		s, ok := latest[w]
		if !ok {
			continue
		}
		percent := s.PercentAt(now)
		needed := estimates[w] * cfg.SafetyFactor
		limit := Cap(cfg, w)
		if percent+needed <= limit || percent == 0 {
			continue
		}
		until := s.ResetsAt
		if until.IsZero() {
			until = now.Add(unknownResetWait)
		}
		if !d.Wait || until.After(d.Until) {
			d = Decision{Wait: true, Until: until, Window: w, Percent: percent, Needed: needed, Cap: limit}
		}
	}
	return d
}

// rateSpanは消費速度を求めるのに使う、直近の観測の期間。
const rateSpan = 3 * time.Hour

// rateMinSpanは消費速度を求めるのに要る、最も古い観測と最も新しい観測の間隔。
// 短すぎると、取得元の1%刻みの丸めで速度が大きく振れる。
const rateMinSpan = 10 * time.Minute

// Rateはtoolの枠windowの、直近の消費速度 (1時間あたりの百分率) を返す。
// 最新の観測と同じリセットの期間の観測だけを使い、求められなければokをfalseにする。
func Rate(samples []Sample, tool agent.Tool, window Window, now time.Time) (perHour float64, ok bool) {
	latest, found := Latest(samples, tool)[window]
	if !found || latest.resetPassed(now) {
		return 0, false
	}
	oldest := latest
	for _, s := range samples {
		if s.Tool != tool || s.Window != window || s.At.Before(latest.At.Add(-rateSpan)) || s.At.After(latest.At) {
			continue
		}
		if d := s.ResetsAt.Sub(latest.ResetsAt); d > resetTolerance || d < -resetTolerance {
			continue
		}
		if s.At.Before(oldest.At) {
			oldest = s
		}
	}
	span := latest.At.Sub(oldest.At)
	if span < rateMinSpan {
		return 0, false
	}
	return max(latest.Percent-oldest.Percent, 0) / span.Hours(), true
}
