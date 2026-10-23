// Package stateはkorylusflowの状態を、計画書ごとの状態ディレクトリに読み書きする。
//
// 状態ディレクトリ (<Root>/<計画書名>/) には次を置く。
//
//	flow.json       現在のタスク・ステップ・番の役割と、タスクごとの記録
//	flow.lock       ファイルロック用の空のファイル
//	timeline.jsonl  ステップごとの開始と終了の時刻
//	report-<版>.md  作業報告書
//
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
)

// stepLabelsはステップの表示名。
var stepLabels = map[Step]string{
	StepImplement: "実装",
	StepReview:    "レビュー",
	StepFix:       "修正",
	StepAsk:       "人への確認",
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
	// Taskは進めているタスク番号。進めているタスクが無ければ空文字。
	Task string `json:"task,omitempty"`
	// Stepは進めているステップ。doneで完了が記録されるまで変えず、
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
	// Reportsは作った作業報告書の版を、作った順に並べたもの。
	Reports []ReportRecord `json:"reports,omitempty"`
	// Pendingは完了を確認済みのステップ。次のブランチの準備や履歴の記録中に
	// 落ちても、完了条件を確かめ直さずに続きを実行するために残す。
	// Taskが空なら次のステップは未選択で、入っていれば準備済み。
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
	// 人が続けてよいと答えたら、回答と修正のあと再レビューせずに次のタスクへ進む。
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

// ReportRecordは作業報告書1版の記録。
type ReportRecord struct {
	Version int `json:"version"`
	// Eventsは、この版に含めた所要時間の記録の件数。これより後に記録が増えたら、
	// 計画書の最後で次の版を作る。
	Events int       `json:"events"`
	At     time.Time `json:"at"`
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
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s を読み取れません: %w", path, err)
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

// ReportPathは版vの作業報告書のパスを返す。
func (d Dir) ReportPath(v int) string {
	return filepath.Join(d.path, reportName(v))
}

// WriteReportは版vの作業報告書を書く。
func (d Dir) WriteReport(v int, data []byte) error {
	return d.write(reportName(v), data)
}

func reportName(v int) string {
	return fmt.Sprintf("report-%d.md", v)
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
