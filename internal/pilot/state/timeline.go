package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const timelineFile = "timeline.jsonl"

// EventKindは所要時間の記録の種類。
type EventKind string

const (
	// EventAgentはエージェントの実行1回。
	EventAgent EventKind = "agent"
	// EventGateはゲート検査のコマンド1つの実行。
	EventGate EventKind = "gate"
	// EventUsageWaitは使用量の回復の待機。
	EventUsageWait EventKind = "usage_wait"
	// EventHumanWaitは人間の待ち (止まってから次のrunまで)。
	EventHumanWait EventKind = "human_wait"
)

// Eventの結果。
const (
	OutcomeOK          = "ok"
	OutcomeFailed      = "failed"
	OutcomeRateLimited = "rate_limited"
)

// Eventは所要時間の記録1件。koryluspilotがすべての遷移を行うため、1つのファイルで
// 計画書全体の時間を追える。
type Event struct {
	Kind EventKind `json:"kind"`
	Task string    `json:"task,omitempty"`
	Step Step      `json:"step,omitempty"`
	Role string    `json:"role,omitempty"`
	// Toolはエージェントの実行と使用量の待機で使ったツール。
	Tool    string    `json:"tool,omitempty"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	// Outcomeはエージェントの実行とゲートのコマンドの結果 (OutcomeOKなど)。
	Outcome string `json:"outcome,omitempty"`
	// TurnsとCostUSDは、エージェントの実行で取れた場合のターン数とコスト。
	Turns   int     `json:"turns,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
	// CommandとExitCodeはゲートのコマンドとその終了コード。
	Command  string `json:"command,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	// Windowは使用量の待機で待った枠。
	Window string `json:"window,omitempty"`
}

// Durationは記録の所要時間を返す。
func (e Event) Duration() time.Duration {
	return e.Ended.Sub(e.Started)
}

// AppendEventは所要時間の記録に1件を追記する。
func (d Dir) AppendEvent(e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.path, 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d.path, timelineFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// 書きかけで途切れた末尾と混ざらないよう、前に改行を置いて1回の書き込みで足す。
	if _, err := f.Write(append(append([]byte{'\n'}, b...), '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ReadEventsは所要時間の記録をすべて読む。まだ記録が無ければ空を返す。
// 書きかけで途切れた行は読み飛ばす。
func (d Dir) ReadEvents() ([]Event, error) {
	b, err := os.ReadFile(filepath.Join(d.path, timelineFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var events []Event
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		events = append(events, e)
	}
	return events, sc.Err()
}
