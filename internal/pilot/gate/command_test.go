package gate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/plan"
)

func TestCommands(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse("project = \"mewst\"\nbase_branch = \"develop\"\nreviewers = [\"reviewer_codex\", \"reviewer_claude\"]\n" + roleConfig + "[gate]\nrunner = ['env']\n[gate.commands]\nGo = ['make -C go lint', 'make -C go test']\n")
	if err != nil {
		t.Fatalf("config.Parseのエラー = %v", err)
	}
	if got := Commands(cfg, plan.Task{Platform: "Go"}); !reflect.DeepEqual(got, []string{"make -C go lint", "make -C go test"}) {
		t.Errorf("Commands(Go) = %v", got)
	}
	if got := Commands(cfg, plan.Task{Platform: "korylus-guidelines"}); len(got) != 0 {
		t.Errorf("Commands(korylus-guidelines) = %v、空を期待", got)
	}
}

func TestCommandRunner_Run(t *testing.T) {
	t.Parallel()

	r := newTestRepo(t)
	r.write("check.sh", "test \"$(cat value.txt)\" = ok\n")
	r.write("value.txt", "ng\n")
	r.commit()
	r.write("value.txt", "ok\n")
	commit := r.commit()
	// 作業ツリーにだけある変更は検査に使わない。
	r.write("value.txt", "ng\n")

	tests := []struct {
		name     string
		commit   string
		commands []string
		// 実行した結果の終了コード。実行しなかったコマンドは含まない。
		wantExits []int
		wantTail  string
	}{
		{
			name:      "取り直したコミットで成功する",
			commit:    commit,
			commands:  []string{"sh check.sh", "test ! -e .koryluspilot"},
			wantExits: []int{0, 0},
		},
		{
			name:      "失敗したところで止め、出力の末尾を残す",
			commit:    commit,
			commands:  []string{"echo 失敗の理由 >&2; exit 3", "true"},
			wantExits: []int{3},
			wantTail:  "失敗の理由",
		},
		{
			name:      "前のコミットでは失敗する",
			commit:    commit + "~1",
			commands:  []string{"sh check.sh"},
			wantExits: []int{1},
		},
		{
			name:      "存在しないコミットは取り直しの失敗として結果に残す",
			commit:    strings.Repeat("0", 40),
			commands:  []string{"true"},
			wantExits: []int{128},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logDir := filepath.Join(t.TempDir(), "gate")
			runner := CommandRunner{Repo: r.dir}
			results, err := runner.Run(context.Background(), tt.commit, tt.commands, logDir)
			if err != nil {
				t.Fatalf("Runのエラー = %v", err)
			}
			var exits []int
			for _, res := range results {
				exits = append(exits, res.ExitCode)
			}
			if !reflect.DeepEqual(exits, tt.wantExits) {
				t.Fatalf("終了コード = %v、期待値 = %v (%+v)", exits, tt.wantExits, results)
			}
			last := results[len(results)-1]
			if !strings.Contains(last.OutputTail, tt.wantTail) {
				t.Errorf("OutputTail = %q、含まれることを期待した文字列 = %q", last.OutputTail, tt.wantTail)
			}
			if last.Passed() && last.OutputTail != "" {
				t.Errorf("成功したコマンドのOutputTail = %q、空を期待", last.OutputTail)
			}
			if _, err := os.Stat(last.LogPath); err != nil {
				t.Errorf("ログ %s が無い: %v", last.LogPath, err)
			}
		})
	}
}

func TestCommandRunner_RunnerPrefix(t *testing.T) {
	t.Parallel()

	r := newTestRepo(t)
	r.write("a.txt", "a\n")
	commit := r.commit()

	// ランナーの接頭辞の後ろに検査のコマンドが付くことを、envで環境変数を足して確かめる。
	runner := CommandRunner{Runner: []string{"env", "KORYLUSPILOT_GATE_TEST=1"}, Repo: r.dir}
	results, err := runner.Run(context.Background(), commit, []string{`test "$KORYLUSPILOT_GATE_TEST" = 1`}, t.TempDir())
	if err != nil {
		t.Fatalf("Runのエラー = %v", err)
	}
	if len(results) != 1 || !results[0].Passed() {
		t.Errorf("結果 = %+v、成功を期待", results)
	}
}

func TestCommandRunner_Timeout(t *testing.T) {
	t.Parallel()

	r := newTestRepo(t)
	r.write("a.txt", "a\n")
	commit := r.commit()

	runner := CommandRunner{Repo: r.dir, Timeout: 500 * time.Millisecond}
	start := time.Now()
	results, err := runner.Run(context.Background(), commit, []string{"sleep 30", "true"}, t.TempDir())
	if err != nil {
		t.Fatalf("Runのエラー = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("経過時間 = %s、タイムアウトで止まることを期待", elapsed)
	}
	if len(results) != 1 || !results[0].TimedOut || results[0].ExitCode != -1 || results[0].Passed() {
		t.Errorf("結果 = %+v、タイムアウトを期待", results)
	}
}

func TestCommandRunner_RunnerMissing(t *testing.T) {
	t.Parallel()

	runner := CommandRunner{Runner: []string{filepath.Join(t.TempDir(), "missing")}, Repo: "."}
	if _, err := runner.Run(context.Background(), "HEAD", []string{"true"}, t.TempDir()); err == nil {
		t.Error("起動できないランナーでエラーを期待したが、nilだった")
	}
}
