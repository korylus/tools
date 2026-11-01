package flow

import (
	"fmt"
	"strings"

	"github.com/korylus/tools/internal/flow/state"
)

// Actionは指示の種類。指示の先頭行 `action: <Action>` に書く。
type Action string

// 指示の種類。
const (
	ActionImplement Action = "implement"
	// ActionWorkは、実装者がブランチを作らずに行うタスク ([implementer]) の作業の指示。
	ActionWork   Action = "work"
	ActionReview Action = "review"
	ActionFix    Action = "fix"
	ActionAsk    Action = "ask"
	// ActionReportは、計画書の最後に作業報告書を書く指示。
	ActionReport Action = "report"
	ActionFinish Action = "finish"
	// ActionWaitは、waitの-timeoutを過ぎても番が来なかったときの指示。
	ActionWait Action = "wait"
)

// verificationExampleは、動作確認の項目のJSONの形の例。
const verificationExample = `[{"kind":"screen または process","title":"確認すること","target":"画面のパスまたはコマンド","steps":["手順"],"expected":"期待する結果"}]`

// Instructionはwaitが役割に返す指示。
type Instruction struct {
	Action  Action
	Role    state.Role
	PlanURL string
	// TaskとTitleは指示の対象のタスク。終了の指示では空。
	Task  string
	Title string
	// Recordは対象のタスクの記録。人が行うタスク・実装者がブランチを作らずに行うタスク・リポジトリを求められないときはnil。
	Record *state.TaskRecord
	// Askは人に尋ねる理由。ActionAskのときだけ持つ。
	Ask *state.Ask
	// ReviewNumberは、レビューの指示ではこれから行うレビューが何回目か、修正と
	// 人への確認の指示では最後に行ったレビューが何回目か。ReviewDocはその
	// レビュードキュメントで、レビューの指示では空。
	ReviewNumber int
	ReviewDoc    string
	// Reportは、終了の指示で、計画書の最後に書いた作業報告書のURL。
	Report string
	// Branchesは、作業報告書の指示で、リポジトリごとの最後のタスクのブランチ。
	Branches []ReportBranch
	// PreviousReportは、作業報告書の指示で、前の回までに書いた作業報告書のURL。無ければ空文字。
	PreviousReport string
	// ReportReviewは、作業報告書のレビュー・修正・人への確認の指示で、対象の作業報告書。
	// タスクの指示ではnil。
	ReportReview *ReportReview
}

// ReportReviewは、レビューする作業報告書。
type ReportReview struct {
	// URLは作業報告書のページのURL。
	URL string
	// Prefixは、レビュードキュメントの名前に使うレビュー接頭辞。
	Prefix string
}

// Stringは指示を、役割のセッションが読む文面にする。コンテキストが圧縮されたあとでも
// 直近の指示だけで続けられるよう、終わったあとの手順まで毎回すべて書く。
func (in Instruction) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "action: %s\n", in.Action)
	if in.Role != "" {
		fmt.Fprintf(&b, "role: %s\n", in.Role)
	}
	fmt.Fprintf(&b, "plan: %s\n", in.PlanURL)
	if in.Task != "" {
		fmt.Fprintf(&b, "task: %s %s\n", in.Task, in.Title)
	}
	if rec := in.Record; rec != nil {
		fmt.Fprintf(&b, "repo: %s\ndir: %s\nbranch: %s\nbase: %s\n", rec.Repo, rec.Dir, rec.Branch, rec.Base)
	}
	if rr := in.ReportReview; rr != nil {
		fmt.Fprintf(&b, "report: %s\n", rr.URL)
	}
	if in.ReviewNumber > 0 {
		fmt.Fprintf(&b, "review: %d回目\n", in.ReviewNumber)
	}
	if in.ReviewDoc != "" {
		fmt.Fprintf(&b, "review_doc: %s\n", in.ReviewDoc)
	}
	b.WriteString("\n")

	switch in.Action {
	case ActionImplement:
		in.writeImplement(&b)
	case ActionWork:
		in.writeWork(&b)
	case ActionReview:
		if in.ReportReview != nil {
			in.writeReportReview(&b)
		} else {
			in.writeReview(&b)
		}
	case ActionFix:
		if in.ReportReview != nil {
			in.writeReportFix(&b)
		} else {
			in.writeFix(&b)
		}
	case ActionAsk:
		in.writeAsk(&b)
	case ActionReport:
		in.writeReport(&b)
	case ActionFinish:
		if in.Report != "" {
			fmt.Fprintf(&b, "計画書の最後まで進んだ。作業報告書 %s を書いたことを人に伝え、ループを終える。\n", in.Report)
		} else {
			b.WriteString("計画書の最後まで進んだ。ループを終える。\n")
		}
	case ActionWait:
		fmt.Fprintf(&b, "まだ %s の番ではない。%s をもう一度実行して待つ。\n", in.Role, code(in.waitCommand()))
	}
	return b.String()
}

func (in Instruction) writeImplement(b *strings.Builder) {
	rec := in.Record
	steps := []string{
		fmt.Sprintf("%s を実行する。ブランチ %s はkorylusflowが作成済みのため、ブランチを作らない",
			code(fmt.Sprintf("/korylus-do-plan-task %s %s repo=%s base=%s", in.PlanURL, in.Task, rec.Repo, rec.Base)), rec.Branch),
		fmt.Sprintf("%s で、タスクの変更を %s にコミットする。docs/private 配下 (計画書・レビュードキュメントなど) はコミットしない", code("/korylus-commit repo="+rec.Repo), rec.Repo),
		"変更のうち、人が画面や処理で確かめられるものを、動作確認の項目のJSONの配列にまとめる。無ければ " + code("[]") + " にする\n   形: " + verificationExample,
		fmt.Sprintf("%s を、項目のJSONを標準入力に渡して実行する", code(in.doneCommand())),
		"done が受け付けられなかったら、理由を読んで直してから4をやり直す。直せなければ人に尋ねる",
		fmt.Sprintf("受け付けられたら %s で次の指示を待つ", code(in.waitCommand())),
	}
	b.WriteString("プロジェクトのルートで、次の順に作業する。\n\n")
	writeSteps(b, steps)
}

func (in Instruction) writeWork(b *strings.Builder) {
	steps := []string{
		fmt.Sprintf("計画書 %s のタスク %s を読み、書かれた作業を行う。ブランチの作成・切り替えとコミットはしない", in.PlanURL, in.Task),
		"取り返しのつかない操作 (gitで追跡していないファイルの削除など) は、行う前に対象の一覧を人に示し、その場で了承を得る",
		"作業の結果 (変えたファイル・作った下書きのURLなど) を人に示し、確かめて計画書のタスクにチェックを付けるよう頼む。チェックは自分で付けない。gitのリポジトリのファイルを変えたときは、人が差分を確かめてコミットするよう伝える",
		fmt.Sprintf("人がチェックを付けたと答えたら、%s を実行する", code(in.doneCommand())),
		"done が受け付けられなかったら、理由を人に伝えて3に戻る",
		fmt.Sprintf("受け付けられたら %s で次の指示を待つ", code(in.waitCommand())),
	}
	b.WriteString("プロジェクトのルートで、次の順に作業する。\n\n")
	writeSteps(b, steps)
}

func (in Instruction) writeReview(b *strings.Builder) {
	rec := in.Record
	command := fmt.Sprintf("/korylus-review %s %s", rec.Base, in.PlanURL)
	if rec.ReviewRepo != "" {
		command += " repo=" + rec.ReviewRepo
	}
	steps := []string{
		fmt.Sprintf("%s を実行し、ブランチ %s のタスク %s の差分をレビューする。ブランチは切り替えない", code(command), rec.Branch, in.Task),
		"レビュードキュメントの対応方針の選択肢にはチェックを入れない。回答はkorylusflowが区分と推奨をもとに行う",
		fmt.Sprintf("%s を実行する", code(in.doneCommand())),
		"done が受け付けられなかったら、理由を読んでレビュードキュメントを直してから3をやり直す。直せなければ人に尋ねる",
		fmt.Sprintf("受け付けられたら %s で次の指示を待つ", code(in.waitCommand())),
	}
	b.WriteString("プロジェクトのルートで、次の順に作業する。\n\n")
	writeSteps(b, steps)
}

func (in Instruction) writeFix(b *strings.Builder) {
	rec := in.Record
	steps := []string{
		fmt.Sprintf("レビュードキュメント %s で対応方針にチェックの入った指摘を、選ばれた方針どおりに %s で修正する。チェック・評価・総評は書き換えず、自分で再レビューしない", in.ReviewDoc, rec.Dir),
		fmt.Sprintf("%s に当てはまるフォーマット・リント・テストを実行し、通るまで直す", rec.Dir),
		fmt.Sprintf("%s で、修正を %s のブランチ %s にコミットする。docs/private 配下 (計画書・レビュードキュメントなど) はコミットしない", code("/korylus-commit repo="+rec.Repo), rec.Repo, rec.Branch),
		fmt.Sprintf("%s を実行する", code(in.doneCommand())),
		"done が受け付けられなかったら、理由を読んで直してから4をやり直す。直せなければ人に尋ねる",
		fmt.Sprintf("受け付けられたら %s で次の指示を待つ", code(in.waitCommand())),
	}
	b.WriteString("プロジェクトのルートで、次の順に作業する。\n\n")
	writeSteps(b, steps)
}

func (in Instruction) writeReportReview(b *strings.Builder) {
	rr := in.ReportReview
	command := fmt.Sprintf("/korylus-review-doc %s prefix=%s", rr.URL, rr.Prefix)
	steps := []string{
		fmt.Sprintf("%s を実行し、作業報告書 %s をレビューする。ブランチは切り替えない", code(command), rr.URL),
		"レビュードキュメントの対応方針の選択肢にはチェックを入れない。回答はkorylusflowが区分と推奨をもとに行う",
		fmt.Sprintf("%s を実行する", code(in.doneCommand())),
		"done が受け付けられなかったら、理由を読んでレビュードキュメントを直してから3をやり直す。直せなければ人に尋ねる",
		fmt.Sprintf("受け付けられたら %s で次の指示を待つ", code(in.waitCommand())),
	}
	b.WriteString("プロジェクトのルートで、次の順に作業する。\n\n")
	writeSteps(b, steps)
}

func (in Instruction) writeReportFix(b *strings.Builder) {
	rr := in.ReportReview
	steps := []string{
		fmt.Sprintf("レビュードキュメント %s で対応方針にチェックの入った指摘を、選ばれた方針どおりに、作業報告書 %s の下書きを書き換えて修正する。フィードバックの節には触れない。チェック・評価・総評は書き換えず、自分で再レビューしない", in.ReviewDoc, rr.URL),
		"作業報告書は下書きで、コミットしない。ブランチも切り替えない",
		fmt.Sprintf("%s を実行する", code(in.doneCommand())),
		"done が受け付けられなかったら、理由を読んで直してから3をやり直す。直せなければ人に尋ねる",
		fmt.Sprintf("受け付けられたら %s で次の指示を待つ", code(in.waitCommand())),
	}
	b.WriteString("プロジェクトのルートで、次の順に作業する。\n\n")
	writeSteps(b, steps)
}

func (in Instruction) writeAsk(b *strings.Builder) {
	fmt.Fprintf(b, "人に尋ねる理由: %s\n", in.Ask.Reason)
	for _, d := range in.Ask.Details {
		fmt.Fprintf(b, "- %s\n", d)
	}
	b.WriteString("\n")

	hint := ""
	switch in.Ask.Kind {
	case state.AskHumanTask:
		hint = "人がタスクを済ませ、計画書のタスクにチェックを付けたら続けられる"
	case state.AskBranch:
		hint = "人が作業ツリーの変更を片付けるか、計画書のタスクの接頭辞か担当の行を直したら続けられる"
	case state.AskReview:
		hint = "人がレビュードキュメントで、挙げた指摘の対応方針を選んだら続けられる"
	case state.AskReviewLimit:
		hint = "人がレビュードキュメントを確かめ、続けてよいと答えたら続けられる。さらにレビューを続ける指示があれば、doneに -review-limit=<新しい上限> を付ける。後続のタスクに積む指摘は、人が対応方針を選び直してよい"
	}
	steps := []string{
		"上の理由と詳細を人に伝え、対応を頼んで返事を待つ。" + hint,
		fmt.Sprintf("人が対応を済ませたと答えたら、%s を実行する", code(in.doneCommand())),
		"done が受け付けられなかったら、理由を人に伝えて1に戻る",
		fmt.Sprintf("受け付けられたら %s で次の指示を待つ", code(in.waitCommand())),
	}
	b.WriteString("プロジェクトのルートで、次の順に作業する。\n\n")
	writeSteps(b, steps)
}

func (in Instruction) writeReport(b *strings.Builder) {
	b.WriteString("リポジトリごとの最後のタスクのブランチ:\n")
	if len(in.Branches) == 0 {
		b.WriteString("- なし (ブランチを作ったタスクがありません)\n")
	}
	for _, br := range in.Branches {
		commit := br.Commit
		if commit == "" {
			commit = "ローカルに残っていません"
		}
		fmt.Fprintf(b, "- %s: %s (%s)\n", br.Repo, code(br.Branch), commit)
	}
	if in.PreviousReport != "" {
		fmt.Fprintf(b, "前の回の作業報告書: %s\n", in.PreviousReport)
	}
	b.WriteString("\n")

	write := fmt.Sprintf("%s を実行し、計画書の作業報告書を書く", code("/korylus-write-report "+in.PlanURL))
	if in.PreviousReport != "" {
		write += "。前の回の作業報告書を、新しく作らずに上のブランチのコードに合わせて更新する"
	}
	steps := []string{
		write + "。ブランチは切り替えない",
		fmt.Sprintf("%s を、作業報告書のURL (処理の流れを書かなかったときはその理由も) のJSONを標準入力に渡して実行する\n   形: %s", code(in.doneCommand()), reportExample),
		"done が受け付けられなかったら、理由を読んで直してから2をやり直す。直せなければ人に尋ねる",
		fmt.Sprintf("受け付けられたら %s で次の指示を待つ", code(in.waitCommand())),
	}
	b.WriteString("プロジェクトのルートで、次の順に作業する。\n\n")
	writeSteps(b, steps)
}

func (in Instruction) doneCommand() string {
	return fmt.Sprintf("korylusflow done %s -role=%s", in.PlanURL, in.Role)
}

func (in Instruction) waitCommand() string {
	return fmt.Sprintf("korylusflow wait %s -role=%s", in.PlanURL, in.Role)
}

// writeStepsは手順を番号付きで書く。
func writeSteps(b *strings.Builder, steps []string) {
	for i, s := range steps {
		fmt.Fprintf(b, "%d. %s\n", i+1, s)
	}
}

// codeはsをMarkdownのインラインコードにする。
func code(s string) string {
	return "`" + s + "`"
}
