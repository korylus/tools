package flow

import (
	"strings"

	"github.com/korylus/tools/internal/pilot/agent"
)

// skillPromptは、toolでスキルskillを引数argsで呼ぶプロンプトを返す。
// Claude Codeは `/スキル名`、Codexは `$スキル名` でスキルを呼ぶ。
func skillPrompt(tool agent.Tool, skill, args string) string {
	prefix := "/"
	if tool == agent.Codex {
		prefix = "$"
	}
	if args == "" {
		return prefix + skill
	}
	return prefix + skill + " " + args
}

// continuePromptは、使用量の上限や失敗で中断したセッションを再開するときのプロンプト。
func continuePrompt(schema bool) string {
	p := "koryluspilotからの指示: 前のターンは途中で中断しました。中断したところから作業を続けてください。"
	if schema {
		p += "\n終えたら、無人実行モードの構造化出力で結果を返してください。"
	}
	return p
}

// reviewPromptは、無人実行モードの /korylus-review でタスクの差分をレビューさせるプロンプト。
// スキルの引数のあとに、対象タスクと、宣言どおりに触れた保護領域と観点を添える。
func reviewPrompt(tool agent.Tool, base, planPath, task string, areas []reviewArea, note string) string {
	var b strings.Builder
	b.WriteString(skillPrompt(tool, "korylus-review", base+" "+planPath+" mode=unattended"))
	b.WriteString("\n\nkoryluspilotからの指示: 作業計画書のタスク ")
	b.WriteString(task)
	b.WriteString(" の差分をレビューしてください。\n")
	if len(areas) == 0 {
		b.WriteString("宣言どおりに触れた保護領域はありません。\n")
	} else {
		b.WriteString("宣言どおりに触れた保護領域と観点は次のとおりです。\n")
		for _, a := range areas {
			points := "観点の指定なし (合意済みの設計制約との照合)"
			if len(a.points) > 0 {
				points = strings.Join(a.points, " / ")
			}
			b.WriteString("- " + a.name + " (" + a.label + "): " + points + "\n")
		}
	}
	if note != "" {
		b.WriteString("\n補足: ")
		b.WriteString(note)
		b.WriteString("\n")
	}
	return b.String()
}

// fixPromptは、回答したレビュードキュメントの対応方針どおりに修正させるプロンプト。
func fixPrompt(doc, note string) string {
	var b strings.Builder
	b.WriteString("koryluspilotからの指示: レビュードキュメント ")
	b.WriteString(doc)
	b.WriteString(` の対応方針に回答しました。
チェックを入れた対応方針どおりに修正し、検証してください。
- 「後続タスクに積む」「現状のまま」などコードを変えない対応方針を選んだ指摘は、修正しないでください
- 直した変更はコミットしないでください。このあとkoryluspilotがコミットを依頼します
- ブランチの切り替え・pushは行わないでください
- レビュードキュメントの評価・総評は書き換えず、再レビューもしないでください
- 作業計画書の「合意済みの設計制約」節と「設計」節は書き換えないでください。設計と異なるアプローチが要るときや、直せないときはエスカレーションを返してください
- 結果は無人実行モードの構造化出力で、korylus-reviewスキルの「レビュー後の修正で返すもの」に従って返してください
`)
	if note != "" {
		b.WriteString("\n補足: ")
		b.WriteString(note)
		b.WriteString("\n")
	}
	return b.String()
}

// reworkPromptは、ゲートのコマンドの失敗を差し戻すプロンプト。
func reworkPrompt(failure, note string) string {
	var b strings.Builder
	b.WriteString(`koryluspilotからの指示: ハブへ取り込んだコミットのゲート検査で、次のコマンドが失敗しました。

`)
	b.WriteString(failure)
	b.WriteString(`

原因を直してください。
- 直した変更はコミットしないでください。このあとkoryluspilotがコミットを依頼します
- ブランチの切り替え・pushは行わないでください
- 作業計画書の「合意済みの設計制約」節と「設計」節は書き換えないでください。設計と異なるアプローチが要るときや、直せないときはエスカレーションを返してください
- 結果は無人実行モードの構造化出力で返してください。verification_itemsには、この修正で人間が確認する項目が増えたときだけ書いてください。触れた保護領域はprotected_changesに書いてください
`)
	if note != "" {
		b.WriteString("\n補足: ")
		b.WriteString(note)
		b.WriteString("\n")
	}
	return b.String()
}
