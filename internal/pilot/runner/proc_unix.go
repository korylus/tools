//go:build unix

package runner

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroupはcmdを新しいプロセスグループで起動し、コンテキストの終わりで
// グループ全体にSIGTERMを送り、killDelayのあとでSIGKILLを送るようにする。
// 先にSIGTERMを送るのは、ランナー (`docker compose run` など) にコンテナを
// 止める機会を与えるためである。
func setProcessGroup(cmd *exec.Cmd, killDelay time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pgid := -cmd.Process.Pid
		time.AfterFunc(killDelay, func() {
			// グループがもう無ければESRCHになるだけなので、結果は見ない。
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		})
		return syscall.Kill(pgid, syscall.SIGTERM)
	}
}
