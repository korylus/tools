// Package stateはkoryluspilotの実行状態を計画書ごとの状態ディレクトリに
// 読み書きする。
//
// 状態ディレクトリ (<Root>/<計画書名>/) には次を置く。
//
//	state.json      現在のタスク・ステップ・レビュー回数・セッションID・止まった理由・版
//	plan.md         計画書のスナップショット (人間が合意した基準)
//	config.toml     設定ファイルのスナップショット (人間が合意した基準)
//	logs/           ステップごとのエージェントの出力
//	timeline.jsonl  ステップごとの所要時間の記録
//
// プロセスが途中で落ちても続きから進められるよう、書き込みは一時ファイルへ
// 書いてから置き換える。
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
)

// Rootは状態ディレクトリを置く既定の場所 (プロジェクトのルートからの相対パス)。
const Root = "tmp/koryluspilot"

const (
	stateFile          = "state.json"
	snapshotFile       = "plan.md"
	configSnapshotFile = "config.toml"
	logsDir            = "logs"
)

// Stepは1タスクの中のステップ。
type Step string

// 1タスクの流れと、フィードバックのループのステップ。
const (
	StepBranch    Step = "branch"
	StepImplement Step = "implement"
	StepGate      Step = "gate"
	StepRework    Step = "rework"
	StepCommit    Step = "commit"
	StepReview    Step = "review"
	StepAnswer    Step = "answer"
	StepFix       Step = "fix"
	StepFeedback  Step = "feedback"
	StepApprove   Step = "approve"
	StepReport    Step = "report"
)

// stepLabelsはステップの表示名。
var stepLabels = map[Step]string{
	StepBranch:    "ブランチ作成",
	StepImplement: "実装",
	StepGate:      "ゲート検査",
	StepRework:    "ゲートからの差し戻し",
	StepCommit:    "コミット",
	StepReview:    "レビュー",
	StepAnswer:    "回答",
	StepFix:       "修正",
	StepFeedback:  "フィードバックの起票",
	StepApprove:   "フィードバック対応の承認",
	StepReport:    "作業報告書の作成",
}

// Labelはステップの表示名を返す。未知のステップは値をそのまま返す。
func (s Step) Label() string {
	if label, ok := stepLabels[s]; ok {
		return label
	}
	return string(s)
}

// Stateは1つの計画書の実行状態。
type State struct {
	// Taskは実行中のタスク番号。
	Task string `json:"task"`
	// Stepは実行中のステップ。止まったときは止まったステップのままにし、
	// 再開するとこのステップからやり直す。
	Step Step `json:"step"`
	// Branchは実行中のタスクのブランチ、Startはそのブランチを切った元のハブのブランチ。
	Branch string `json:"branch,omitempty"`
	Start  string `json:"start,omitempty"`
	// Roleは実行中のタスクの変更を持つ役割。ハブへはこの役割のクローンから取り込む。
	Role string `json:"role,omitempty"`
	// Reviewsは実行中のタスクで行ったレビューの回数。
	Reviews int `json:"reviews"`
	// Reworksは実行中のタスクで、ゲートのコマンドの失敗を差し戻した回数。
	Reworks int `json:"reworks,omitempty"`
	// GateFailureは差し戻すゲートのコマンドの失敗の内容。差し戻しのプロンプトに使う。
	GateFailure string `json:"gate_failure,omitempty"`
	// GateCommitは最後にゲート検査を通ったコミット。再開時に変更が無ければ検査を省略する。
	GateCommit string `json:"gate_commit,omitempty"`
	// ResumeStepは、停止中の変更を取り込み、ゲート検査を通したあとに戻るステップ。
	// 差し戻しや中断を挟んでも、回答やレビューを飛ばさないために残す。
	ResumeStep Step `json:"resume_step,omitempty"`
	// Touchedは、実行中のタスクの最後のゲート検査で、宣言どおりに触れた保護領域の名前。
	// レビュアーへのプロンプトに渡す。
	Touched []string `json:"touched,omitempty"`
	// ReviewDocは実行中のタスクの最後のレビュードキュメント (プロジェクトのルートからの相対パス)、
	// Verdictはその総合評価。
	ReviewDoc string `json:"review_doc,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
	// SessionsはステップごとのエージェントのセッションID。止まったステップを
	// 同じセッションで再開するために使う。
	Sessions map[Step]string `json:"sessions,omitempty"`
	// OutsideBeforeは、実行中のエージェントのステップの前に読んだ、リポジトリの外の
	// 監視するパスの状態。ステップの途中で落ちても、再開したときに前の状態と比べる。
	OutsideBefore json.RawMessage `json:"outside_before,omitempty"`
	// Stopは止まった理由。止まっていなければnil。
	Stop *Stop `json:"stop,omitempty"`
	// Tasksはkoryluspilotが終えたタスクを、終えた順に並べたもの。
	Tasks []TaskRecord `json:"tasks,omitempty"`
	// Outputsは各ステップの構造化出力。作業報告書の元データになる。
	Outputs []Output `json:"outputs,omitempty"`
	// ReviewLogは行ったレビューを、行った順に並べたもの。
	ReviewLog []ReviewRecord `json:"review_log,omitempty"`
	// Deferredは、推奨どおりに回答してコードを変えずに先送りした指摘。
	// 後続タスクにするかは、作業報告書を見て人間が決める。
	Deferred []DeferredFinding `json:"deferred,omitempty"`
	// StopLogは止まった履歴を、止まった順に並べたもの。作業報告書に載せる。
	StopLog []StopRecord `json:"stop_log,omitempty"`
	// ReportVersionは最後に作った作業報告書の版。まだ作っていなければ0。
	ReportVersion int `json:"report_version"`
	// Reportsは作った作業報告書を、版の順に並べたもの。
	Reports []ReportRecord `json:"reports,omitempty"`
	// Verifiedは、動作確認ですべての項目がOKになり、計画書を確認済みとして終えた
	// 作業報告書の版。まだ終えていなければ0。
	Verified int `json:"verified,omitempty"`
	// UpdatedAtは状態を最後に書き込んだ時刻。
	UpdatedAt time.Time `json:"updated_at"`
}

// Stopは停止条件に当たって止まったときの記録。
type Stop struct {
	// Reasonは止まった理由。
	Reason string `json:"reason"`
	// Detailsは理由の詳細 (検知した変更・エスカレーションの内容など)。
	Details []string `json:"details,omitempty"`
	// Roleは止まった原因の解消に使う役割。
	Role string `json:"role,omitempty"`
	// Atは止まった時刻。
	At time.Time `json:"at"`
}

// StopRecordは止まった履歴1件。
type StopRecord struct {
	Task string `json:"task,omitempty"`
	Step Step   `json:"step,omitempty"`
	Stop
	// ResumedAtは止まったあとにrunで再開した時刻。まだ再開していなければゼロ値。
	ResumedAt time.Time `json:"resumed_at,omitzero"`
}

// ReportRecordは作った作業報告書1版。
type ReportRecord struct {
	Version int `json:"version"`
	// Tasksは、この版を作った時点で終えていたタスクの数 (State.Tasksの先頭からの件数)。
	// 次の版では、これより後ろのタスクを新しく終えたタスクとして扱う。
	Tasks int       `json:"tasks"`
	At    time.Time `json:"at"`
}

// TaskRecordはkoryluspilotが終えたタスク1件。
type TaskRecord struct {
	ID string `json:"id"`
	// Branchはタスクのブランチ、Startはそのブランチを切った元のハブのブランチ。
	Branch string `json:"branch"`
	Start  string `json:"start"`
	// Commitはタスクを終えたときのブランチの先端。
	Commit string    `json:"commit"`
	At     time.Time `json:"at"`
}

// ReviewRecordはレビュー1回の記録。
type ReviewRecord struct {
	Task string `json:"task"`
	// Numberはそのタスクで何回目のレビューか (1始まり)。
	Number int    `json:"number"`
	Role   string `json:"role"`
	// Docはレビュードキュメントの、プロジェクトのルートからの相対パス。
	Doc     string    `json:"doc"`
	Verdict string    `json:"verdict"`
	At      time.Time `json:"at"`
}

// DeferredFindingは、コードを変えずに先送りした指摘1件。
type DeferredFinding struct {
	Task string `json:"task"`
	// Docは指摘があるレビュードキュメントのパス、Titleは指摘の見出し。
	Doc   string `json:"doc"`
	Title string `json:"title"`
	// Choiceは選ばれた対応方針の選択肢。
	Choice string `json:"choice"`
}

// Outputはエージェントのステップ1回の構造化出力。同じステップの再試行や再開の
// 出力も1件ずつ残すため、作業報告書はOutcomeとStoppedで採る出力を選ぶ。
type Output struct {
	Task string    `json:"task"`
	Step Step      `json:"step"`
	Role string    `json:"role"`
	At   time.Time `json:"at"`
	// Outcomeはこの出力を返した試行の結果 (OutcomeOKなど)。
	Outcome string `json:"outcome"`
	// Stoppedは、この出力を返した試行のあとに、リポジトリの外の監視で止めたかどうか。
	// エスカレーションで止めたことはResult.Statusで分かる。
	Stopped bool                   `json:"stopped,omitempty"`
	Result  agent.UnattendedResult `json:"result"`
}

// Dirは1つの計画書の状態ディレクトリ。
type Dir struct {
	path string
}

// NewDirはrootの下の計画書名planNameの状態ディレクトリを返す。
// ディレクトリは書き込むときに作る。
func NewDir(root, planName string) Dir {
	return Dir{path: filepath.Join(root, planName)}
}

// Pathは状態ディレクトリのパスを返す。
func (d Dir) Path() string {
	return d.path
}

// LogsPathはエージェントの出力を置くディレクトリのパスを返す。
func (d Dir) LogsPath() string {
	return filepath.Join(d.path, logsDir)
}

// Loadは状態ファイルを読む。まだ実行していなければfs.ErrNotExistを包んだ
// エラーを返す。
func (d Dir) Load() (*State, error) {
	b, err := os.ReadFile(filepath.Join(d.path, stateFile))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s を読み取れません: %w", filepath.Join(d.path, stateFile), err)
	}
	return &s, nil
}

// Saveは状態ファイルを書く。UpdatedAtにはnowを入れる。
func (d Dir) Save(s *State, now time.Time) error {
	s.UpdatedAt = now
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return d.write(stateFile, append(b, '\n'))
}

// LoadSnapshotは計画書のスナップショットを読む。まだ取っていなければ
// fs.ErrNotExistを包んだエラーを返す。
func (d Dir) LoadSnapshot() ([]byte, error) {
	return os.ReadFile(filepath.Join(d.path, snapshotFile))
}

// SaveSnapshotは計画書の内容srcをスナップショットとして書く。
func (d Dir) SaveSnapshot(src []byte) error {
	return d.write(snapshotFile, src)
}

// LoadConfigSnapshotは設定ファイルのスナップショットを読む。まだ取っていなければ
// fs.ErrNotExistを包んだエラーを返す。
func (d Dir) LoadConfigSnapshot() ([]byte, error) {
	return os.ReadFile(filepath.Join(d.path, configSnapshotFile))
}

// SaveConfigSnapshotは設定ファイルの内容srcをスナップショットとして書く。
func (d Dir) SaveConfigSnapshot(src []byte) error {
	return d.write(configSnapshotFile, src)
}

// writeはnameへdataを書く。書きかけのファイルが残らないよう、同じ
// ディレクトリの一時ファイルに書いてから置き換える。
func (d Dir) write(name string, data []byte) error {
	if err := os.MkdirAll(d.path, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d.path, name+".*.tmp")
	if err != nil {
		return err
	}
	// 置き換えに成功したあとは消す対象が無く、失敗は無視してよい。
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		// 書き込みの失敗のほうを返す。
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(d.path, name))
}
