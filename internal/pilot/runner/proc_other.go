//go:build !unix

package runner

import (
	"os/exec"
	"time"
)

// setProcessGroupは、プロセスグループを扱えない環境では何もしない。
// キャンセル時はexec.CommandContextの既定どおり、起動したプロセスだけを止める。
func setProcessGroup(*exec.Cmd, time.Duration) {}
