// Package stateはkorylusflowの状態を、計画書ごとの状態ディレクトリに読み書きする。
//
// 状態ディレクトリ (<Root>/<計画書名>/) には次を置く。
//
//	flow.json       現在のタスク・ステップ・番の役割と、タスクごとの記録
//	flow.lock       ファイルロック用の空のファイル
//	timeline.jsonl  ステップごとの開始と終了の時刻
//
// 作業報告書はWikinoのページに実装者が書き、状態にはそのURLと処理の流れを
// 書かなかった理由だけを回ごとに記録する。
// 状態を変えるコマンドは、LockContextで取ったロックの中で読み取りから書き込みまでを行う。
// 書き込みは一時ファイルに書いてから置き換えるため、ロックを取らずに読んでも
// 書きかけの状態は見えない。
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Rootは状態ディレクトリを置く既定の場所 (プロジェクトのルートからの相対パス)。
// 計画書と一緒に履歴として残すため、計画書の置き場所の下に置く。
const Root = "docs/private/plans/files"

const (
	stateFile = "flow.json"
	lockFile  = "flow.lock"
)

// Roleはループに入るセッションの役割。
type Role string

// 役割。
const (
	RoleImplementer    Role = "implementer"
	RoleReviewerCodex  Role = "reviewer-codex"
	RoleReviewerClaude Role = "reviewer-claude"
)

// Rolesはすべての役割を、表示する順に並べたもの。
var Roles = []Role{RoleImplementer, RoleReviewerCodex, RoleReviewerClaude}

// roleLabelsは役割の表示名。
var roleLabels = map[Role]string{
	RoleImplementer:    "実装者",
	RoleReviewerCodex:  "レビュアー (Codex)",
	RoleReviewerClaude: "レビュアー (Claude Code)",
}

// ParseRoleは-roleの値を役割として読む。
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if _, ok := roleLabels[r]; !ok {
		return "", fmt.Errorf("不明な役割 %q (implementer / reviewer-codex / reviewer-claude のいずれか)", s)
	}
	return r, nil
}

// Labelは役割の表示名を返す。未知の役割は値をそのまま返す。
func (r Role) Label() string {
	if label, ok := roleLabels[r]; ok {
		return label
	}
	return string(r)
}

// Stepは1タスクの中のステップ。
type Step string

// ステップ。ブランチの準備と回答はkorylusflowがコマンドの中で済ませるため、
// 役割が担うステップだけを持つ。
const (
	StepImplement Step = "implement"
	StepReview    Step = "review"
	StepFix       Step = "fix"
	StepAsk       Step = "ask"
	// StepReportは、計画書の最後に実装者が作業報告書を書くステップ。
	// タスクに属さないため、状態と所要時間の記録のタスク番号は空にする。
	StepReport Step = "report"
)

// stepWalkthroughは、作業報告書のステップの以前の値。この値で記録した状態は、
// 読むときにStepReportに読み替える。所要時間の記録には残っていることがある。
const stepWalkthrough Step = "walkthrough"

// stepLabelsはステップの表示名。
var stepLabels = map[Step]string{ //#nosec G101 -- ステップの表示名で、資格情報ではない (stepWalkthroughの「pW」がパスワードの名前に見なされる)
	StepImplement:   "実装",
	StepReview:      "レビュー",
	StepFix:         "修正",
	StepAsk:         "人への確認",
	StepReport:      "作業報告書の作成",
	stepWalkthrough: "処理の流れの作成",
}

// Labelはステップの表示名を返す。未知のステップは値をそのまま返す。
func (s Step) Label() string {
	if label, ok := stepLabels[s]; ok {
		return label
	}
	return string(s)
}

// Stateは1つの計画書の状態。
type State struct {
	// Taskは進めているタスク番号。進めているタスクが無いときと、タスクに属さない
	// ステップ (作業報告書の作成) を進めているときは空文字。
	Task string `json:"task,omitempty"`
	// Stepは進めているステップで、無ければ空文字。doneで完了が記録されるまで変えず、
	// 次に尋ねられたときも同じ指示を返す。
	Step Step `json:"step,omitempty"`
	// Roleは番の役割。
	Role Role `json:"role,omitempty"`
	// Sinceは進めているステップを始めた時刻。所要時間の記録に使う。
	Since time.Time `json:"since,omitzero"`
	// Askは人を待っている理由。人を待っていなければnil。
	Ask *Ask `json:"ask,omitempty"`
	// ReviewBeforeは、レビューのステップを始めたときの最新のレビュードキュメント。
	// これより新しい連番のドキュメントを、そのステップのレビューとして受け付ける。
	ReviewBefore string `json:"review_before,omitempty"`
	// Tasksはkorylusflowが扱ったタスクを、扱い始めた順に並べたもの。
	Tasks []TaskRecord `json:"tasks,omitempty"`
	// Reportsは作業報告書のステップで受け取った記録を、回の順に並べたもの。
	Reports []ReportRound `json:"report_rounds,omitempty"`
	// Pendingは完了を確認済みのステップ。次のブランチの準備や履歴の記録中に
	// 落ちても、完了条件を確かめ直さずに続きを実行するために残す。
	// Stepが空なら次のステップは未選択で、入っていれば準備済み。
	Pending *Event `json:"pending,omitempty"`
	// UpdatedAtは状態を最後に書き込んだ時刻。
	UpdatedAt time.Time `json:"updated_at"`
}

// AskKindは人に尋ねている場面。doneで確かめる条件が場面ごとに異なる。
type AskKind string

// 人に尋ねる場面。
const (
	// AskHumanTaskは人が行うタスク。計画書のタスクにチェックが付いたら続ける。
	AskHumanTask AskKind = "human_task"
	// AskBranchはブランチを準備できないとき。準備ができたら続ける。
	AskBranch AskKind = "branch"
	// AskReviewは要人間判断の指摘か再検討提案があるとき。すべての指摘の
	// 対応方針に回答があれば続ける。
	AskReview AskKind = "review"
	// AskReviewLimitは、レビューが上限の回数に達してもRequest Changesだったとき。
	// 上限を延長せずに続ける場合は、回答と修正のあと再レビューせずに次のタスクへ進む。
	AskReviewLimit AskKind = "review_limit"
)

// Askは人を待っている理由。
type Ask struct {
	Kind AskKind `json:"kind"`
	// Reasonは人に尋ねている理由。
	Reason string `json:"reason"`
	// Detailsは理由の詳細 (人にしてほしいこと・未コミットのファイルなど)。
	Details []string `json:"details,omitempty"`
}

// TaskRecordはタスク1件の記録。
type TaskRecord struct {
	ID string `json:"id"`
	// Repoはタスクのリポジトリのルート、Dirは接頭辞が表す作業するディレクトリ。
	Repo string `json:"repo"`
	Dir  string `json:"dir"`
	// Branchはタスクのブランチ、Baseはそのブランチを作った元のブランチ。
	Branch string `json:"branch"`
	Base   string `json:"base"`
	// Preparedはブランチを準備できたかどうか。準備できなかったタスクのブランチは
	// 作られていないため、同じリポジトリの次のタスクのベースにしない。
	Prepared bool `json:"prepared,omitempty"`
	// ReviewPrefixは、/korylus-review がレビュードキュメントの名前に使うレビュー接頭辞。
	// ReviewRepoは /korylus-review の repo= に渡すリポジトリで、本体なら空。
	// どちらも最初のレビューを始めるときに決める。
	ReviewPrefix string `json:"review_prefix,omitempty"`
	ReviewRepo   string `json:"review_repo,omitempty"`
	// Reviewsは行ったレビューを、行った順に並べたもの。
	Reviews []ReviewRecord `json:"reviews,omitempty"`
	// ReviewLimitは人が延長したこのタスクのレビュー上限。0なら既定の3回を使う。
	ReviewLimit int `json:"review_limit,omitempty"`
	// Deferredは、コードを変えない選択肢 (後続タスクに積む・現状維持など) で回答した指摘。
	Deferred []DeferredFinding `json:"deferred,omitempty"`
	// Verificationは実装の完了で受け取った動作確認の項目。
	Verification []VerificationItem `json:"verification,omitempty"`
}

// ReviewRecordはレビュー1回の記録。
type ReviewRecord struct {
	// Numberはそのタスクで何回目のレビューか (1始まり)。
	Number int  `json:"number"`
	Role   Role `json:"role"`
	// Docはレビュードキュメントの、プロジェクトのルートからの相対パス。
	Doc     string    `json:"doc"`
	Verdict string    `json:"verdict"`
	At      time.Time `json:"at"`
}

// DeferredFindingは先送りした指摘1件。
type DeferredFinding struct {
	// Docは指摘があるレビュードキュメントの、プロジェクトのルートからの相対パス。
	Doc   string `json:"doc"`
	Title string `json:"title"`
	// Choiceは選ばれた選択肢の本文。
	Choice string `json:"choice"`
}

// VerificationItemは動作確認の項目1件。
type VerificationItem struct {
	// Kindは画面の確認なら "screen"、処理やコマンドの確認なら "process"。
	Kind  string `json:"kind"`
	Title string `json:"title"`
	// Targetは画面ならアプリのパス、処理ならコマンド。
	Target   string   `json:"target"`
	Steps    []string `json:"steps"`
	Expected string   `json:"expected"`
}

// ReportRoundは作業報告書1回分の記録。korylusflowは作業報告書の本文を読まず、
// 場所と処理の流れを書かなかった理由だけを持つ。
type ReportRound struct {
	// Numberは何回目の作業報告書か (1始まり)。
	Number int `json:"number"`
	// Eventsは、この回に含めた所要時間の記録の件数。これより後に記録が増えたら、
	// 計画書の最後で次の回の作業報告書を書く。
	Events int       `json:"events"`
	At     time.Time `json:"at"`
	// Locationは作業報告書のページのURL。Markdownの作業報告書を作っていた回
	// (以前の形式の状態から読み替えた回) では空。
	Location string `json:"location,omitempty"`
	// WalkthroughSkippedは、作業報告書に処理の流れを書かなかった理由。書いたときは空。
	WalkthroughSkipped string `json:"walkthrough_skipped,omitempty"`
}

// LatestReportは、最新の回の作業報告書のURLを返す。まだ書いていないか、最新の回が
// Markdownの作業報告書だったときは空文字。
func (s *State) LatestReport() string {
	if n := len(s.Reports); n > 0 {
		return s.Reports[n-1].Location
	}
	return ""
}

// PreviousReportは、これまでの回で書いた作業報告書のURLのうち最新のものを返す。
// 無ければ空文字。次の回で、新しく作らずに同じページを更新してもらうために使う。
func (s *State) PreviousReport() string {
	for i := len(s.Reports) - 1; i >= 0; i-- {
		if s.Reports[i].Location != "" {
			return s.Reports[i].Location
		}
	}
	return ""
}

// TaskRecordはタスク番号idの記録を返す。まだ扱っていなければnilを返す。
func (s *State) TaskRecord(id string) *TaskRecord {
	for i := range s.Tasks {
		if s.Tasks[i].ID == id {
			return &s.Tasks[i]
		}
	}
	return nil
}

// Dirは1つの計画書の状態ディレクトリ。
type Dir struct {
	path string
}

// NewDirはrootの下の計画書名planNameの状態ディレクトリを返す。
// ディレクトリは書き込むときかロックを取るときに作る。
func NewDir(root, planName string) Dir {
	return Dir{path: filepath.Join(root, planName)}
}

// Pathは状態ディレクトリのパスを返す。
func (d Dir) Path() string {
	return d.path
}

// Lockは、期限やキャンセルの要らない呼び出しのためのLockContext。
// ほかのプロセスがロックを持っていれば、外されるまで待つ。
func (d Dir) Lock() (unlock func() error, err error) {
	return d.LockContext(context.Background())
}

// LockContextは状態ディレクトリの排他ロックを取り、ロックを外す関数を返す。
// ほかのプロセスのロックを待っている間も、ctxの期限やキャンセルで中断する。
func (d Dir) LockContext(ctx context.Context) (unlock func() error, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(d.path, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(d.path, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //#nosec G115 -- ファイル記述子はintに収まる
		if err == nil {
			// ファイルを閉じるとロックも外れる。
			return f.Close, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("%s のロックを取れません: %w", f.Name(), err)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

// Loadは状態ファイルを読む。まだ状態が無ければfs.ErrNotExistを包んだエラーを返す。
func (d Dir) Load() (*State, error) {
	path := filepath.Join(d.path, stateFile)
	b, err := os.ReadFile(path) //#nosec G304 -- 計画書名から決めた状態ディレクトリのファイル
	if err != nil {
		return nil, err
	}
	var raw struct {
		State
		// LegacyReportsは、Markdownの作業報告書を作っていたときの版の記録。
		LegacyReports []legacyReport `json:"reports"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s を読み取れません: %w", path, err)
	}
	s := raw.State
	upgrade(&s, raw.LegacyReports)
	return &s, nil
}

// legacyReportは、Markdownの作業報告書を作っていたときの版1つの記録。
type legacyReport struct {
	Version int       `json:"version"`
	Events  int       `json:"events"`
	At      time.Time `json:"at"`
}

// upgradeは、以前の形式で書いた状態sを今の形式に読み替える。
// Markdownの作業報告書の版は、場所を持たない回として引き継ぐ。次の回でページを新しく
// 作ってもらうためである。版に移していなかった処理の流れの記録は、引き継がずに捨てる。
// 計画書の最後で作業報告書のステップから書き直してもらう。
func upgrade(s *State, legacy []legacyReport) {
	if len(s.Reports) == 0 {
		for _, r := range legacy {
			s.Reports = append(s.Reports, ReportRound{Number: r.Version, Events: r.Events, At: r.At})
		}
	}
	if s.Step == stepWalkthrough {
		s.Step = StepReport
	}
	if s.Pending != nil && s.Pending.Step == stepWalkthrough {
		s.Pending.Step = StepReport
	}
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
