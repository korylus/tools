// Package cli provides the flags and options shared across subcommands.
// Registering the common flags here lets both the comment and md subcommands
// use the same diff-scope mechanism (-base).
//
// [Ja] cli パッケージはサブコマンド間で共有するフラグ・オプションの土台を提供する。
// comment / md の両サブコマンドが同じ差分スコープ機構 (-base) を使えるよう、
// 共通フラグの登録をここに集約する。
package cli

import "flag"

// baseUsage is the description of the common -base flag.
// [Ja] baseUsage は共通フラグ -base の説明文。
const baseUsage = "git ref to diff against; limit checks to lines added since <ref> / 差分の基準 git ref。<ref> 以降に追加された行に検査を限定する"

// Options holds the options shared across subcommands. For now it only carries
// the base ref for the diff scope, but future shared options collect here too.
//
// [Ja] Options はサブコマンドが共通して受け取るオプション。現状は差分スコープの
// 基準 ref のみだが、今後追加する共通オプションもここへ集約する。
type Options struct {
	// Base is the git ref for the diff scope. An empty string checks the whole tree.
	// [Ja] Base は差分スコープの基準となる git ref。空文字なら全体を検査対象にする。
	Base string
}

// RegisterCommon registers the common flags on fs and returns an Options that
// points at the resolved values. The fields are populated after fs.Parse runs.
//
// [Ja] RegisterCommon は fs に共通フラグを登録し、解決済みの値を指す Options を返す。
// 返り値のフィールドは fs.Parse の呼び出し後に埋まる。
func RegisterCommon(fs *flag.FlagSet) *Options {
	opts := &Options{}
	fs.StringVar(&opts.Base, "base", "", baseUsage)
	return opts
}
