// Command koryluslintはKorylus共通のリンタを単一バイナリにまとめ、
// サブコマンドで各リンタへディスパッチする。
//
//	koryluslint comment [-base=<ref>] [paths...]  ソースコードの日本語スタイルを検査する
//	koryluslint md [-base=<ref>] [paths...]       Markdownの句点改行と日本語スタイルを検査する
//
// 各サブコマンドの実体はinternal/配下のパッケージに置き、本ファイルは
// サブコマンドのディスパッチと共通の入出力・終了コードのみを担う。
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/korylus/tools/internal/comment"
	"github.com/korylus/tools/internal/md"
)

// usageTextは引数不正・-h時に表示する使い方。
const usageText = `koryluslint - Korylus共通のリンタ

使い方:
  koryluslint comment [-base=<ref>] [paths...]
  koryluslint md [-base=<ref>] [paths...]

サブコマンド:
  comment   ソースコードの日本語テキストスタイル (korylus-lang.md §3) を検査する
            .go / .templはコメント、.sh / .tsはコメントと文字列の本文が対象
            .sql / .css / .tomlは日本語を含む行が対象
  md        Markdownドキュメントの句点改行 (semantic line break) と日本語テキストスタイル (korylus-lang.md §3) を検査する

共通フラグ:
  -base=<ref>   <ref> 以降に追加された行に検査を限定する (差分スコープ)
`

// subcommandは1サブコマンドの処理。argsはサブコマンド名を除いた残りの
// 引数で、戻り値はプロセスの終了コード。
type subcommand func(args []string, stdout, stderr io.Writer) int

// subcommandsはサブコマンド名から処理への対応表。
var subcommands = map[string]subcommand{
	"comment": comment.Run,
	"md":      md.Run,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// runはサブコマンドをディスパッチする。テストから直接呼べるようmainから分離している。
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}

	name, rest := args[0], args[1:]
	switch name {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usageText)
		return 0
	}

	sub, ok := subcommands[name]
	if !ok {
		fmt.Fprintf(stderr, "koryluslint: 不明なサブコマンド %q\n\n%s", name, usageText)
		return 2
	}
	return sub(rest, stdout, stderr)
}
