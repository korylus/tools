package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunner_Command(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tests := []struct {
		name   string
		runner Runner
		want   string
	}{
		{
			name:   "接頭辞が無ければ直接起動する",
			runner: Runner{Dir: dir, Env: []string{"KORYLUSPILOT_TEST=direct"}},
			want:   "direct " + dir + " a b\n",
		},
		{
			name: "接頭辞の後ろにコマンドを付けて起動する",
			// 接頭辞は環境を足してからコマンドを起動する (コンテナに入るランナーの代わり)。
			runner: Runner{Prefix: []string{"env", "KORYLUSPILOT_TEST=prefixed"}, Dir: dir, Env: []string{}},
			want:   "prefixed " + dir + " a b\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := tt.runner.Command(context.Background(), "sh", "-c", `echo "$KORYLUSPILOT_TEST" "$(pwd)" "$@"`, "sh", "a", "b")
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("Outputのエラー = %v", err)
			}
			if string(out) != tt.want {
				t.Errorf("出力 = %q、期待値 = %q", out, tt.want)
			}
		})
	}
}

// TestRunner_CommandCancelはコンテキストが終わったら、コマンドが起動した
// 子プロセスもまとめて止めることを確かめる。
func TestRunner_CommandCancel(t *testing.T) {
	t.Parallel()

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cmd := Runner{}.Command(ctx, "sh", "-c", `sleep 60 & echo $! > "$1"; wait`, "sh", pidFile)
	start := time.Now()
	err := cmd.Run()
	if err == nil {
		t.Fatal("エラーを期待したが、nilだった")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("コンテキストのエラー = %v、期限切れを期待", ctx.Err())
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("止まるまでの時間 = %s、子プロセスを待たずに止まることを期待", d)
	}

	b, err := os.ReadFile(pidFile) //#nosec G304 -- テストがt.TempDirの下に決めたパス
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(b))
	// 止めた子プロセスは親を失って引き取られるため、回収されずにゾンビとして
	// 残っていても止まったとみなす。
	deadline := time.Now().Add(5 * time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + pid + "/stat") //#nosec G304 G703 -- テストが起動した子プロセスの状態を読む
		if os.IsNotExist(err) || strings.Contains(string(stat), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("子プロセス %s が残っている", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunner_CommandKillは、SIGTERMで止まらないプロセスを、KillDelayのあとで
// SIGKILLで止めることを確かめる。
func TestRunner_CommandKill(t *testing.T) {
	t.Parallel()

	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := Runner{KillDelay: 200 * time.Millisecond}.Command(ctx, "sh", "-c", `trap '' TERM; touch "$1"; while :; do sleep 0.1; done`, "sh", ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// SIGTERMを無視する設定が済んでから止める。
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	err := cmd.Wait()
	if err == nil {
		t.Fatal("エラーを期待したが、nilだった")
	}
	// WaitDelay (KillDelay + 1秒) で起動したプロセスだけを止めるより先に、グループへの
	// SIGKILLで止まる。
	if d := time.Since(start); d >= time.Second {
		t.Errorf("止まるまでの時間 = %s、KillDelayのあとでSIGKILLで止まることを期待", d)
	}
}
