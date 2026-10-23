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

// Eventはステップ1回の所要時間の記録。StepがStepAskの記録は、人を待った時間を表す。
type Event struct {
	Task    string    `json:"task,omitempty"`
	Step    Step      `json:"step"`
	Role    Role      `json:"role"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	// Reasonは、人への確認のステップで人に尋ねた理由。
	Reason string `json:"reason,omitempty"`
}

// Durationは記録の所要時間を返す。
func (e Event) Duration() time.Duration {
	return e.Ended.Sub(e.Started)
}

// AppendEventは所要時間の記録に1件を追記する。同じタスク・ステップ・役割・開始時刻の
// 記録が既にあれば何もしない。状態保存の失敗後に再試行しても重複しないようにする。
// 呼び出し側は、記録の確認から追記までを状態ディレクトリのロックの中で行う。
func (d Dir) AppendEvent(e Event) error {
	events, err := d.ReadEvents()
	if err != nil {
		return err
	}
	for _, prev := range events {
		if prev.Task == e.Task && prev.Step == e.Step && prev.Role == e.Role && prev.Started.Equal(e.Started) {
			return nil
		}
	}
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
