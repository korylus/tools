// Package cliはサブコマンド間で共有するフラグ・オプションの土台を提供する。
// comment / mdの両サブコマンドが同じ差分スコープ機構 (-base) を使えるよう、
// 共通フラグの登録をここに集約する。
package cli

import "flag"

// baseUsageは共通フラグ -baseの説明文。
const baseUsage = "差分の基準となるgit ref。<ref> 以降に追加された行に検査を限定する"

// Optionsはサブコマンドが共通して受け取るオプション。
// 現状は差分スコープの基準refのみだが、今後追加する共通オプションもここへ集約する。
type Options struct {
	// Baseは差分スコープの基準となるgit ref。空文字なら全体を検査対象にする。
	Base string
}

// RegisterCommonはfsに共通フラグを登録し、解決済みの値を指すOptionsを返す。
// 返り値のフィールドはfs.Parseの呼び出し後に埋まる。
func RegisterCommon(fs *flag.FlagSet) *Options {
	opts := &Options{}
	fs.StringVar(&opts.Base, "base", "", baseUsage)
	return opts
}
