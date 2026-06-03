// Command koryluslint bundles the Korylus shared linters into a single binary
// and dispatches to each linter through a subcommand.
//
//	koryluslint comment [-base=<ref>] [paths...]  check the [Ja] marker in code comments
//	koryluslint md [-base=<ref>] [paths...]       check semantic line breaks in Markdown
//
// Each subcommand's implementation lives in a package under internal/; this
// file only dispatches to subcommands and owns the shared I/O and exit code.
//
// [Ja] koryluslint コマンドは Korylus 共通のリンタを単一バイナリにまとめ、
// サブコマンドで各リンタへディスパッチする。
//
// 各サブコマンドの実体は internal/ 配下のパッケージに置き、本ファイルは
// サブコマンドのディスパッチと共通の入出力・終了コードのみを担う。
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/korylus/korylus-tools/internal/comment"
	"github.com/korylus/korylus-tools/internal/md"
)

// usageText is shown for invalid arguments or -h.
// [Ja] usageText は引数不正・-h 時に表示する使い方。
const usageText = `koryluslint - the Korylus shared linter

Usage:
  koryluslint comment [-base=<ref>] [paths...]
  koryluslint md [-base=<ref>] [paths...]

Subcommands:
  comment   check misuse of the [Ja] marker in code comments
  md        check (and fix) semantic line breaks in Markdown documents

Common flags:
  -base=<ref>   limit checks to lines added since <ref> (diff scope)
`

// subcommand handles one subcommand. args is what remains after the subcommand
// name, and the return value is the process exit code.
// [Ja] subcommand は 1 サブコマンドの処理。args はサブコマンド名を除いた残りの
// 引数で、戻り値はプロセスの終了コード。
type subcommand func(args []string, stdout, stderr io.Writer) int

// subcommands maps a subcommand name to its handler.
// [Ja] subcommands はサブコマンド名から処理への対応表。
var subcommands = map[string]subcommand{
	"comment": comment.Run,
	"md":      md.Run,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to a subcommand. It is split out from main so that tests can
// call it directly.
// [Ja] run はサブコマンドをディスパッチする。テストから直接呼べるよう main から分離している。
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
		fmt.Fprintf(stderr, "koryluslint: unknown subcommand %q\n\n%s", name, usageText)
		return 2
	}
	return sub(rest, stdout, stderr)
}
