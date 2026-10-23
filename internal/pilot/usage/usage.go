// Package usageはClaude Code / Codexの使用率を取得・記録し、ステップごとの
// 消費量の見積もりと、エージェントを起動する前に待つかどうかの判定を行う。
//
// 使用率はどちらもサーバー側で集計したアカウント全体の値で、他プロジェクト・
// 他端末の消費を含む。取得した値は状態ディレクトリの usage.jsonl に記録し、
// 計画書をまたいで見積もりに使う。
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/state"
)

// FileNameは使用率の記録のファイル名。状態ディレクトリの直下に置く。
const FileName = "usage.jsonl"

// Windowは使用量の枠。
type Window string

const (
	FiveHour Window = "five_hour"
	Weekly   Window = "weekly"
)

// Windowsは表示と判定で枠を見る順序。
var Windows = []Window{FiveHour, Weekly}

// Labelは枠の表示名を返す。
func (w Window) Label() string {
	switch w {
	case FiveHour:
		return "5時間枠"
	case Weekly:
		return "週枠"
	}
	return string(w)
}

// Sourceは使用率の取得元。
type Source string

const (
	// SourceStreamはClaude Codeのstream-jsonの `rate_limit_event`。
	SourceStream Source = "stream"
	// SourceUsageCommandは `claude -p "/usage"` のテキスト。
	SourceUsageCommand Source = "usage_command"
	// SourceAppServerは `codex app-server` の `account/rateLimits/read`。
	SourceAppServer Source = "app_server"
	// SourceSessionLogはCodexのセッションログの `rate_limits` (非公式)。
	SourceSessionLog Source = "session_log"
)

// Sampleは1つの枠の使用率の観測。
type Sample struct {
	// Atは観測した時刻。セッションログから読んだ値はログに記録された時刻。
	At     time.Time  `json:"at"`
	Tool   agent.Tool `json:"tool"`
	Window Window     `json:"window"`
	// Percentは使用率 (百分率)。
	Percent float64 `json:"percent"`
	// ResetsAtは枠のリセット時刻。不明ならゼロ値。
	ResetsAt time.Time `json:"resets_at,omitzero"`
	Source   Source    `json:"source"`
}

// resetPassedはnowの時点で、観測した枠がリセットを過ぎたかどうかを返す。
func (s Sample) resetPassed(now time.Time) bool {
	return !s.ResetsAt.IsZero() && !now.Before(s.ResetsAt)
}

// PercentAtはnowの時点の使用率を返す。リセットを過ぎた観測は0%として扱う。
func (s Sample) PercentAt(now time.Time) float64 {
	if s.resetPassed(now) {
		return 0
	}
	return s.Percent
}

// StepUsageは1つのステップの前後の使用率。差を消費量の見積もりに使う。
type StepUsage struct {
	// Atはステップが終わった時刻。
	At     time.Time  `json:"at"`
	Tool   agent.Tool `json:"tool"`
	Step   state.Step `json:"step"`
	Window Window     `json:"window"`
	Before float64    `json:"before"`
	After  float64    `json:"after"`
	// BeforeResetsAtとAfterResetsAtは前後の観測のリセット時刻。
	BeforeResetsAt time.Time `json:"before_resets_at,omitzero"`
	AfterResetsAt  time.Time `json:"after_resets_at,omitzero"`
}

// resetToleranceはリセット時刻が同じとみなす誤差。取得元によって秒の丸めが異なるため幅を持たせる。
const resetTolerance = time.Minute

// Consumedはステップの消費量を返す。リセットをまたいだ差や、取得元の丸めで
// 減って見える差は見積もりに使えないため、okをfalseにする。
func (s StepUsage) Consumed() (v float64, ok bool) {
	d := s.AfterResetsAt.Sub(s.BeforeResetsAt)
	if d > resetTolerance || d < -resetTolerance {
		return 0, false
	}
	if s.After < s.Before {
		return 0, false
	}
	return s.After - s.Before, true
}

// lineは usage.jsonl の1行。観測とステップの消費のどちらか一方を持つ。
type line struct {
	Sample *Sample    `json:"sample,omitempty"`
	Step   *StepUsage `json:"step,omitempty"`
}

// Logは使用率の記録 (usage.jsonl)。
type Log struct {
	path string
}

// NewLogは状態ディレクトリrootの直下の記録を返す。ファイルは書き込むときに作る。
func NewLog(root string) Log {
	return Log{path: filepath.Join(root, FileName)}
}

// Pathは記録のファイルのパスを返す。
func (l Log) Path() string {
	return l.path
}

// Readは記録をすべて読む。まだ記録が無ければ空を返す。
// 書きかけで途切れた行やJSONとして読めない行は読み飛ばす。プロセスが書き込みの
// 途中で落ちても、それまでの記録を見積もりに使い続けるためである。
func (l Log) Read() (samples []Sample, steps []StepUsage, err error) {
	b, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ln line
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			continue
		}
		switch {
		case ln.Sample != nil:
			samples = append(samples, *ln.Sample)
		case ln.Step != nil:
			steps = append(steps, *ln.Step)
		}
	}
	return samples, steps, sc.Err()
}

// AppendSamplesは観測を記録に追記する。
func (l Log) AppendSamples(samples []Sample) error {
	lines := make([]line, 0, len(samples))
	for i := range samples {
		lines = append(lines, line{Sample: &samples[i]})
	}
	return l.append(lines)
}

// AppendStepsはステップの消費を記録に追記する。
func (l Log) AppendSteps(steps []StepUsage) error {
	lines := make([]line, 0, len(steps))
	for i := range steps {
		lines = append(lines, line{Step: &steps[i]})
	}
	return l.append(lines)
}

func (l Log) append(lines []line) error {
	if len(lines) == 0 {
		return nil
	}
	var buf bytes.Buffer
	// 書きかけの末尾と次の記録を分離する。改行も同じ書き込みに含め、
	// 同時追記時に末尾の確認や修復が競合しないようにする。
	buf.WriteByte('\n')
	enc := json.NewEncoder(&buf)
	for _, ln := range lines {
		if err := enc.Encode(ln); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o750); err != nil {
		return err
	}
	// 複数のkoryluspilotが同時に追記しても行が混ざらないよう、1回の書き込みで足す。
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Latestはtoolの枠ごとに、最も新しい観測を返す。
func Latest(samples []Sample, tool agent.Tool) map[Window]Sample {
	latest := map[Window]Sample{}
	for _, s := range samples {
		if s.Tool != tool {
			continue
		}
		if cur, ok := latest[s.Window]; !ok || !s.At.Before(cur.At) {
			latest[s.Window] = s
		}
	}
	return latest
}

// StepUsagesはステップの前後の観測から、枠ごとの消費の記録を作る。
// 前後のどちらかで観測できなかった枠は記録しない。
func StepUsages(tool agent.Tool, step state.Step, before, after map[Window]Sample, at time.Time) []StepUsage {
	var steps []StepUsage
	for _, w := range Windows {
		b, okB := before[w]
		a, okA := after[w]
		if !okB || !okA {
			continue
		}
		steps = append(steps, StepUsage{
			At:             at,
			Tool:           tool,
			Step:           step,
			Window:         w,
			Before:         b.Percent,
			After:          a.Percent,
			BeforeResetsAt: b.ResetsAt,
			AfterResetsAt:  a.ResetsAt,
		})
	}
	return steps
}
