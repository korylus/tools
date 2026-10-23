// Package runnerは、役割のランナー (コマンドの接頭辞) でコマンドを起動する。
//
// 役割のコンテナで動かすコマンド (`claude` / `codex` / git) は、設定のrunner
// (`docker compose run --rm -T pilot-implementer` など) の後ろに付けて起動する。
// コンテキストが終わったら、起動したプロセスのプロセスグループごと止める。
package runner

import (
	"cmp"
	"context"
	"os/exec"
	"slices"
	"time"
)

// DefaultKillDelayはRunner.KillDelayを省略したときの、SIGTERMを送ってからSIGKILLを
// 送るまでの時間。
const DefaultKillDelay = 10 * time.Second

// Runnerはコマンドを起動する場所。ゼロ値は、現在の環境とカレントディレクトリで
// コマンドを直接起動する。
type Runner struct {
	// Prefixはコマンドの前に付ける接頭辞 (設定のrunner)。空ならコマンドを直接起動する。
	Prefix []string
	// Dirは起動するプロセスの作業ディレクトリ。空ならカレントディレクトリ。
	// 接頭辞があるときは接頭辞のプロセスの作業ディレクトリで、コンテナの中の
	// 作業ディレクトリはコンテナの設定で決まる。
	Dir string
	// Envは起動するプロセスの環境。nilなら現在の環境を使う。
	Env []string
	// KillDelayは、止めるときにSIGTERMを送ってからSIGKILLを送るまでの時間。
	// 0ならDefaultKillDelayを使う。
	KillDelay time.Duration
}

// Commandは、接頭辞の後ろにnameとargsを付けたコマンドを作る。ctxが終わると、
// 起動したプロセスのプロセスグループごと止める (エージェントやゲートのコマンドが
// 起動したテストなどの子プロセスを残さないため)。
func (r Runner) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	argv := slices.Concat(r.Prefix, []string{name}, args)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //#nosec G204 -- 起動するのは設定のランナーと、koryluspilotが決めたコマンド
	cmd.Dir = r.Dir
	cmd.Env = r.Env
	killDelay := cmp.Or(r.KillDelay, DefaultKillDelay)
	setProcessGroup(cmd, killDelay)
	// グループへのSIGKILLのあとで、出力のパイプが閉じるのを待たずに終える。
	cmd.WaitDelay = killDelay + time.Second
	return cmd
}
