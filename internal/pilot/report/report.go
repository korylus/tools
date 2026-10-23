// Package reportは、計画書の最後まで進んだときに作る作業報告書を、状態ファイル・
// 計画書・レビューの記録・各ステップの構造化出力・ハブの差分から決定的に組み立て、
// HTMLに描画する。文章をLLMに新たに書かせず、記録にあるものだけを並べる。
// また、最新の版をHTTPで配信し、人間がフォームで送信した動作確認の結果を保存する (Server)。
//
// 作業報告書と動作確認の結果は、計画書と一緒に履歴として残すため、
// docs/private/plans/files/<計画書名>/ に版ごとに置く。
//
//	report-<版>.json        作業報告書の元データ (Report)
//	report-<版>.html        作業報告書
//	verification-<版>.json  動作確認の結果 (Verification)
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FilesDirは作業報告書を置く、プロジェクトのルートからの相対パス。
const FilesDir = "docs/private/plans/files"

// 確認項目の種類。screenとprocessは /korylus-do-plan-task の verification_items の値。
const (
	KindScreen    = "screen"
	KindProcess   = "process"
	KindProtected = "protected"
)

// 動作確認の結果の値。未確認は空文字。
const (
	StatusOK = "ok"
	StatusNG = "ng"
)

// Reportは作業報告書1版の元データ。
type Report struct {
	// Planは計画書の、プロジェクトのルートからの相対パス。
	Plan string `json:"plan"`
	// Nameは計画書名。
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	// Startedは計画書の最初の記録の時刻、VersionStartedはこの版の期間の始まり
	// (前の版を作った時刻。第1版はStartedと同じ)。
	Started        time.Time `json:"started"`
	VersionStarted time.Time `json:"version_started"`
	// Usageはこの版の期間の使用量の消費、Costsはツールごとのターン数とコスト。
	Usage []UsageRow `json:"usage"`
	Costs []CostRow  `json:"costs"`
	// Branchesはタスクを終えた順のブランチ。pushとPRの作成に使う。
	Branches []Branch `json:"branches"`
	// Preparationは動作確認の前に人間が行う準備。
	Preparation Preparation `json:"preparation"`
	Tasks       []Task      `json:"tasks"`
	Deferred    []Deferred  `json:"deferred"`
	Stops       []Stop      `json:"stops"`
	Timing      Timing      `json:"timing"`
	Protected   Protected   `json:"protected"`
	Items       []Item      `json:"items"`
	Previous    *PrevReport `json:"previous,omitempty"`
}

// PrevReportは前の版の概要。
type PrevReport struct {
	Version int    `json:"version"`
	Comment string `json:"comment,omitempty"`
}

// UsageRowは1つのツール・枠の使用率の消費。
type UsageRow struct {
	Tool   string `json:"tool"`
	Window string `json:"window"`
	// Percentは記録したステップの消費 (百分率のポイント) の合計、Stepsはその件数。
	Percent float64 `json:"percent"`
	Steps   int     `json:"steps"`
}

// CostRowは1つのツールのターン数とコスト。取れた実行の分だけを足す。
type CostRow struct {
	Tool    string  `json:"tool"`
	Runs    int     `json:"runs"`
	Turns   int     `json:"turns"`
	CostUSD float64 `json:"cost_usd"`
}

// Branchはタスク1件のブランチ。
type Branch struct {
	Task   string `json:"task"`
	Branch string `json:"branch"`
	Start  string `json:"start"`
	Commit string `json:"commit"`
}

// Preparationは動作確認の前の準備。
type Preparation struct {
	// Branchは動作確認に使う最後のブランチ。Switchedは、koryluspilotが人間のチェックアウトを
	// そのブランチに切り替えたかどうか (未コミットの変更があれば切り替えない)。
	Branch   string `json:"branch"`
	Switched bool   `json:"switched"`
	// Skippedは、チェックアウトしていたが未コミットの変更があったため進めなかったブランチ。
	Skipped []Branch `json:"skipped,omitempty"`
	// Setupは設定のreport.setupのコマンド (マイグレーションの適用など)。
	Setup []string `json:"setup,omitempty"`
}

// Taskはタスク1件の変更の内容。
type Task struct {
	ID       string `json:"id"`
	Platform string `json:"platform,omitempty"`
	Title    string `json:"title"`
	// NewInVersionは、この版で新しく終えたタスクかどうか。
	NewInVersion bool         `json:"new_in_version"`
	Branch       string       `json:"branch"`
	Start        string       `json:"start"`
	Commit       string       `json:"commit"`
	Files        []FileChange `json:"files"`
	// ImplLinesは実装の行数、ExpectedImplLinesは計画書の想定行数 (テストを除く。記載が無ければ0)。
	ImplLines         int      `json:"impl_lines"`
	ExpectedImplLines int      `json:"expected_impl_lines"`
	Summary           string   `json:"summary"`
	Decisions         []string `json:"decisions"`
	Reviews           []Review `json:"reviews"`
}

// FileChangeは変更したファイル1件。
type FileChange struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Status  string `json:"status"`
	// Kindは実装の行数の集計での扱い (gate.FileKind)。
	Kind    string `json:"kind"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// Reviewはレビュー1回。
type Review struct {
	Number  int    `json:"number"`
	Role    string `json:"role"`
	Verdict string `json:"verdict"`
	Doc     string `json:"doc"`
}

// Deferredは、推奨どおりに回答してコードを変えずに先送りした指摘1件。
type Deferred struct {
	Task   string `json:"task"`
	Doc    string `json:"doc"`
	Title  string `json:"title"`
	Choice string `json:"choice"`
}

// Stopは止まった履歴1件。
type Stop struct {
	Task    string    `json:"task,omitempty"`
	Step    string    `json:"step,omitempty"`
	Reason  string    `json:"reason"`
	Details []string  `json:"details,omitempty"`
	Role    string    `json:"role,omitempty"`
	At      time.Time `json:"at"`
	// ResumedAtは再開した時刻。まだ再開していなければゼロ値。
	ResumedAt time.Time `json:"resumed_at,omitzero"`
}

// Timingは所要時間の節。
type Timing struct {
	// Rowsは種類・役割ごとの合計。
	Rows []TimingRow `json:"rows"`
	// Tasksはタスクごとの内訳。
	Tasks []TaskTiming `json:"tasks"`
	// VersionTotalはこの版の期間の合計、Totalは版をまたいだ合計。
	VersionTotal time.Duration `json:"version_total"`
	Total        time.Duration `json:"total"`
}

// TimingRowは種類・役割ごとの所要時間の合計。
type TimingRow struct {
	// Kindは記録の種類 (state.EventKind)。
	Kind string `json:"kind"`
	Role string `json:"role,omitempty"`
	// Countはこの版の期間の件数、Versionはこの版の期間の合計、Totalは版をまたいだ合計。
	Count   int           `json:"count"`
	Version time.Duration `json:"version"`
	Total   time.Duration `json:"total"`
}

// TaskTimingはタスク1件の所要時間の内訳。
type TaskTiming struct {
	Task      string        `json:"task"`
	Agent     time.Duration `json:"agent"`
	Gate      time.Duration `json:"gate"`
	UsageWait time.Duration `json:"usage_wait"`
	HumanWait time.Duration `json:"human_wait"`
	// Reworksはゲートからの差し戻しの回数、Reviewsはレビューの回数。
	Reworks int `json:"reworks"`
	Reviews int `json:"reviews"`
}

// Protectedは保護領域の変更の節。
type Protected struct {
	// Constraintsは計画書の「合意済みの設計制約」節の本文。
	Constraints string `json:"constraints"`
	// Undeclaredは、タスクで宣言せずに触れ、on_undeclared = "report" で通した変更。
	Undeclared []UndeclaredChange `json:"undeclared"`
	Areas      []Area             `json:"areas"`
}

// UndeclaredChangeは宣言の無い保護領域への変更1件。
type UndeclaredChange struct {
	Task  string `json:"task"`
	Area  string `json:"area"`
	Label string `json:"label"`
	Hits  []Hit  `json:"hits"`
}

// Areaは保護領域1つの変更。
type Area struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Itemはこの領域の確認項目のID。
	Item         string        `json:"item"`
	Declarations []Declaration `json:"declarations"`
	// Finalはベースブランチから最後のブランチまでの差分のうち、この領域に当たった箇所。
	Final []Hit      `json:"final"`
	Tasks []AreaTask `json:"tasks"`
}

// Declarationはタスクでの保護領域の宣言1件。
type Declaration struct {
	Task string `json:"task"`
	Note string `json:"note,omitempty"`
}

// AreaTaskは保護領域1つのうち、タスク1件の内訳。
type AreaTask struct {
	Task     string `json:"task"`
	Declared bool   `json:"declared"`
	Hits     []Hit  `json:"hits"`
	// Changesは実装と修正のステップが返した変更の説明 (エージェントの自己申告)。
	Changes []Change `json:"changes"`
	// Checksはそのタスクの最後のレビューが返した照合の結果。
	Checks []Check `json:"checks"`
}

// Hitは保護領域に当たった箇所1件 (gate.Hitの表示用の形)。
type Hit struct {
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Removed bool   `json:"removed,omitempty"`
	Text    string `json:"text,omitempty"`
	Func    string `json:"func,omitempty"`
	Edge    string `json:"edge,omitempty"`
}

// Changeはステップが返した保護領域の変更の説明1件。
type Change struct {
	Step    string `json:"step"`
	Role    string `json:"role"`
	Summary string `json:"summary"`
	Reason  string `json:"reason"`
}

// Checkはレビュアーの照合の結果1件。
type Check struct {
	Role   string `json:"role"`
	Point  string `json:"point"`
	Result string `json:"result"`
	Detail string `json:"detail"`
}

// Itemは動作確認チェックリストの項目1件。
type Item struct {
	// IDは版をまたいで項目を対応付ける識別子。動作確認の結果はIDで項目を指す。
	ID string `json:"id"`
	// Numberは作業報告書に表示する項目の番号 (1始まり)。フィードバックのタスクは
	// 「第N版 #M」の形でこの番号を参照する。
	Number int `json:"number"`
	// Tasksは対象のタスク。保護領域の項目は、その領域に触れたタスクを並べる。
	Tasks    []string `json:"tasks"`
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	Target   string   `json:"target,omitempty"`
	Steps    []string `json:"steps"`
	Expected string   `json:"expected"`
	// Confirmedは、前の版までにOKになり、この版で確かめ直さなくてよい項目かどうか。
	Confirmed bool `json:"confirmed"`
	// Previousは前の版での同じ項目の結果。前の版に無ければnil。
	Previous *PrevItem `json:"previous,omitempty"`
	// Feedbackは前の版の項目に対応したフィードバックのタスク。
	Feedback []string `json:"feedback,omitempty"`
}

// PrevItemは前の版での項目の結果。
type PrevItem struct {
	Number  int    `json:"number"`
	Status  string `json:"status"`
	Comment string `json:"comment,omitempty"`
}

// Verificationは作業報告書1版の動作確認の結果。
type Verification struct {
	Version int `json:"version"`
	// Itemsは項目のIDごとの結果。
	Items map[string]ItemResult `json:"items"`
	// Commentは全体へのコメント。
	Comment string    `json:"comment"`
	SavedAt time.Time `json:"saved_at"`
}

// ItemResultは項目1件の結果。Statusは StatusOK / StatusNG か、未確認の空文字。
type ItemResult struct {
	Status  string `json:"status"`
	Comment string `json:"comment"`
}

// Classifyは、作業報告書rの確認項目を、動作確認の結果vでまだOK / NGを付けていない
// 項目 (pending) とNGを付けた項目 (ng) に分ける。前の版までに確認済みの項目は、
// 確かめ直さなくてよいため、未確認のままでもpendingに含めない。
func Classify(r *Report, v *Verification) (pending, ng []Item) {
	for _, it := range r.Items {
		switch res := v.Items[it.ID]; {
		case res.Status == StatusNG:
			ng = append(ng, it)
		case res.Status == "" && !it.Confirmed:
			pending = append(pending, it)
		}
	}
	return pending, ng
}

// Dirは計画書名planNameの作業報告書を置くディレクトリを返す。rootはプロジェクトのルート。
func Dir(root, planName string) string {
	return filepath.Join(root, FilesDir, planName)
}

// JSONPathは、dirの版vの作業報告書の元データのパスを返す。
func JSONPath(dir string, v int) string {
	return filepath.Join(dir, fmt.Sprintf("report-%d.json", v))
}

// HTMLPathは、dirの版vの作業報告書のパスを返す。
func HTMLPath(dir string, v int) string {
	return filepath.Join(dir, fmt.Sprintf("report-%d.html", v))
}

// VerificationPathは、dirの版vの動作確認の結果のパスを返す。
func VerificationPath(dir string, v int) string {
	return filepath.Join(dir, fmt.Sprintf("verification-%d.json", v))
}

// Loadはdirの版vの作業報告書の元データを読む。無ければfs.ErrNotExistを包んだエラーを返す。
func Load(dir string, v int) (*Report, error) {
	var r Report
	if err := readJSON(JSONPath(dir, v), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// LoadVerificationはdirの版vの動作確認の結果を読む。まだ保存されていなければ
// fs.ErrNotExistを包んだエラーを返す。
func LoadVerification(dir string, v int) (*Verification, error) {
	var res Verification
	if err := readJSON(VerificationPath(dir, v), &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Writeは作業報告書の元データとHTMLをdirに書く。verificationは埋め込む動作確認の結果で、
// まだ無ければnilを渡す。
func Write(dir string, r *Report, verification *Verification) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	var html bytes.Buffer
	if err := Render(&html, r, verification); err != nil {
		return err
	}
	if err := writeFile(JSONPath(dir, r.Version), append(b, '\n')); err != nil {
		return err
	}
	return writeFile(HTMLPath(dir, r.Version), html.Bytes())
}

// SaveVerificationは作業報告書rの動作確認の結果verificationをdirに書き、結果を
// 埋め込んだHTMLを書き直す。
func SaveVerification(dir string, r *Report, verification *Verification) error {
	b, err := json.MarshalIndent(verification, "", "  ")
	if err != nil {
		return err
	}
	var html bytes.Buffer
	if err := Render(&html, r, verification); err != nil {
		return err
	}
	if err := writeFile(VerificationPath(dir, r.Version), append(b, '\n')); err != nil {
		return err
	}
	return writeFile(HTMLPath(dir, r.Version), html.Bytes())
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path) //#nosec G304 -- 作業報告書のディレクトリの、版の番号から決めたパス
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s を読み取れません: %w", path, err)
	}
	return nil
}

// writeFileはpathへdataを書く。書きかけのファイルを配信しないよう、同じ
// ディレクトリの一時ファイルに書いてから置き換える。
func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
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
	return os.Rename(tmp.Name(), path)
}
