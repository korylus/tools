package hub

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korylus/tools/internal/pilot/runner"
)

// testGitEnvは、テストで使うgitの環境。利用者のgitの設定に左右されないようにし、
// コミットの作者を固定する。
func testGitEnv() []string {
	return append(gitEnv(os.Environ()),
		"GIT_AUTHOR_NAME=koryluspilot",
		"GIT_AUTHOR_EMAIL=koryluspilot@example.com",
		"GIT_COMMITTER_NAME=koryluspilot",
		"GIT_COMMITTER_EMAIL=koryluspilot@example.com",
	)
}

// gitRunはdirでgitを実行し、標準出力の前後の空白を除いて返す。
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //#nosec G204 -- テストが組み立てたgitの引数
	cmd.Dir = dir
	cmd.Env = testGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitはdirにファイルnameを書いてコミットし、コミットIDを返す。
func commit(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "--", name)
	gitRun(t, dir, "commit", "--quiet", "-m", name)
	return gitRun(t, dir, "rev-parse", "HEAD")
}

// fixtureはハブ・GitHubの代わりのリポジトリ・役割のクローンを用意したテストの環境。
type fixture struct {
	hub      Hub
	upstream string
	// baseはupstreamのdevelopの先端。
	base string
}

// newFixtureは、developにコミットを1つ持つupstreamと、それを取り込んだハブを作る。
// ハブは役割のコンテナの中から見たパスの代わりに、シンボリックリンクのパスで渡す。
func newFixture(t *testing.T) fixture {
	t.Helper()

	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	if err := os.Mkdir(upstream, 0o750); err != nil {
		t.Fatal(err)
	}
	gitRun(t, upstream, "init", "--quiet", "--initial-branch=develop")
	base := commit(t, upstream, "README.md", "mewst\n")

	h := Hub{Root: filepath.Join(dir, "pilot")}
	h.Path = filepath.Join(dir, "mounted-hub.git")
	ctx := context.Background()
	if err := h.Init(ctx, []string{"implementer", "reviewer_codex"}); err != nil {
		t.Fatalf("Initのエラー = %v", err)
	}
	if err := os.Symlink(h.GitDir(), h.Path); err != nil {
		t.Fatal(err)
	}
	if err := h.FetchUpstream(ctx, upstream, "develop"); err != nil {
		t.Fatalf("FetchUpstreamのエラー = %v", err)
	}
	return fixture{hub: h, upstream: upstream, base: base}
}

// roleRunnerは役割のランナーの代わり。作業ディレクトリを役割のクローンにし、
// 接頭辞で環境を足してからコマンドを起動する。
func (f fixture) roleRunner(role string) runner.Runner {
	return runner.Runner{
		Prefix: []string{"env", "KORYLUSPILOT_ROLE=" + role},
		Dir:    f.hub.CloneDir(role),
		Env:    testGitEnv(),
	}
}

func resolve(t *testing.T, h Hub, branch string) string {
	t.Helper()
	commit, ok, err := h.Resolve(context.Background(), branch)
	if err != nil {
		t.Fatalf("Resolveのエラー = %v", err)
	}
	if !ok {
		return ""
	}
	return commit
}

func wantRejected(t *testing.T, err error, want string) {
	t.Helper()
	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("エラー = %v、*RejectedErrorを期待", err)
	}
	if !strings.Contains(rejected.Reason, want) {
		t.Errorf("RejectedError.Reason = %q、含まれることを期待した文字列 = %q", rejected.Reason, want)
	}
}

func TestDefaultRoot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	got, err := DefaultRoot("mewst")
	if err != nil {
		t.Fatalf("DefaultRootのエラー = %v", err)
	}
	if got != "/data/koryluspilot/mewst" {
		t.Errorf("DefaultRoot() = %q、期待値 = %q", got, "/data/koryluspilot/mewst")
	}

	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/exedev")
	got, err = DefaultRoot("mewst")
	if err != nil {
		t.Fatalf("DefaultRootのエラー = %v", err)
	}
	if got != "/home/exedev/.local/share/koryluspilot/mewst" {
		t.Errorf("DefaultRoot() = %q、期待値 = %q", got, "/home/exedev/.local/share/koryluspilot/mewst")
	}
}

func TestHub_InitAndFetchUpstream(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := context.Background()
	if got := gitRun(t, f.hub.GitDir(), "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("ハブがベアリポジトリではない: %s", got)
	}
	for _, role := range []string{"implementer", "reviewer_codex"} {
		if info, err := os.Stat(f.hub.CloneDir(role)); err != nil || !info.IsDir() {
			t.Errorf("役割 %s のクローンのディレクトリが無い: %v", role, err)
		}
	}
	if got := resolve(t, f.hub, "develop"); got != f.base {
		t.Errorf("ハブのdevelop = %q、期待値 = %q", got, f.base)
	}
	// 2回目のInitはハブを作り直さない。
	if err := f.hub.Init(ctx, nil); err != nil {
		t.Fatalf("Initのエラー = %v", err)
	}
	if got := resolve(t, f.hub, "develop"); got != f.base {
		t.Errorf("2回目のInitのあとのハブのdevelop = %q、期待値 = %q", got, f.base)
	}

	// upstreamが進めば取り込む。
	next := commit(t, f.upstream, "go.mod", "module mewst\n")
	if err := f.hub.FetchUpstream(ctx, f.upstream, "develop"); err != nil {
		t.Fatalf("FetchUpstreamのエラー = %v", err)
	}
	if got := resolve(t, f.hub, "develop"); got != next {
		t.Errorf("ハブのdevelop = %q、期待値 = %q", got, next)
	}

	// upstreamの履歴が書き換わったら取り込まない。
	gitRun(t, f.upstream, "reset", "--quiet", "--hard", f.base)
	commit(t, f.upstream, "other.txt", "rewritten\n")
	if err := f.hub.FetchUpstream(ctx, f.upstream, "develop"); err == nil {
		t.Error("fast-forwardにならない取り込みでエラーを期待したが、nilだった")
	}
	if got := resolve(t, f.hub, "develop"); got != next {
		t.Errorf("取り込みを拒否したあとのハブのdevelop = %q、期待値 = %q", got, next)
	}

	if err := f.hub.FetchUpstream(ctx, f.upstream, "-develop"); err == nil || !strings.Contains(err.Error(), "ブランチの名前") {
		t.Errorf("不正なブランチの名前のエラー = %v", err)
	}
}

func TestHub_PrepareClone(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := context.Background()
	clone := f.hub.CloneDir("implementer")
	r := f.roleRunner("implementer")

	// ハブに無いタスクのブランチは、元のブランチから作る。
	if err := f.hub.PrepareClone(ctx, r, "plan-1-1", "develop"); err != nil {
		t.Fatalf("PrepareCloneのエラー = %v", err)
	}
	if got := gitRun(t, clone, "symbolic-ref", "--short", "HEAD"); got != "plan-1-1" {
		t.Errorf("クローンのブランチ = %q、期待値 = %q", got, "plan-1-1")
	}
	if got := gitRun(t, clone, "rev-parse", "HEAD"); got != f.base {
		t.Errorf("クローンの先端 = %q、期待値 = %q", got, f.base)
	}
	// クローンのリモートは、コンテナの中から見たハブのパスにする。
	if got := gitRun(t, clone, "config", "remote.hub.url"); got != f.hub.Path {
		t.Errorf("remote.hub.url = %q、期待値 = %q", got, f.hub.Path)
	}

	// ハブに取り込んでいないコミットは、次の準備でハブの先端に合わせ直す。
	commit(t, clone, "stale.txt", "not imported\n")
	gitRun(t, clone, "switch", "--quiet", "-c", "scratch")
	if err := f.hub.PrepareClone(ctx, r, "plan-1-1", "develop"); err != nil {
		t.Fatalf("PrepareCloneのエラー = %v", err)
	}
	if got := gitRun(t, clone, "rev-parse", "HEAD"); got != f.base {
		t.Errorf("準備し直したクローンの先端 = %q、期待値 = %q", got, f.base)
	}

	// ハブにあるタスクのブランチは、その先端に合わせる。
	other := f.hub.CloneDir("reviewer_codex")
	if err := f.hub.PrepareClone(ctx, f.roleRunner("reviewer_codex"), "plan-1-1", "develop"); err != nil {
		t.Fatalf("PrepareCloneのエラー = %v", err)
	}
	tip := commit(t, other, "review.txt", "fix\n")
	if _, err := f.hub.Import(ctx, f.roleRunner("reviewer_codex"), "plan-1-1", "develop"); err != nil {
		t.Fatalf("Importのエラー = %v", err)
	}
	if err := f.hub.PrepareClone(ctx, r, "plan-1-1", "develop"); err != nil {
		t.Fatalf("PrepareCloneのエラー = %v", err)
	}
	if got := gitRun(t, clone, "rev-parse", "HEAD"); got != tip {
		t.Errorf("ハブのブランチに合わせたクローンの先端 = %q、期待値 = %q", got, tip)
	}

	// 未コミットの変更は消さずに止める。
	if err := os.WriteFile(filepath.Join(clone, "wip.txt"), []byte("wip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := f.hub.PrepareClone(ctx, r, "plan-1-2", "plan-1-1")
	wantRejected(t, err, "wip.txt")
	if _, err := os.Stat(filepath.Join(clone, "wip.txt")); err != nil {
		t.Errorf("未コミットのファイルが消えた: %v", err)
	}

	if err := f.hub.PrepareClone(ctx, r, "plan-2-1", "missing"); err == nil || !strings.Contains(err.Error(), "missing がハブにありません") {
		t.Errorf("元のブランチが無いときのエラー = %v", err)
	}
}

func TestHub_Import(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	setup := func(t *testing.T) (fixture, string, runner.Runner) {
		t.Helper()
		f := newFixture(t)
		r := f.roleRunner("implementer")
		if err := f.hub.PrepareClone(ctx, r, "plan-1-1", "develop"); err != nil {
			t.Fatalf("PrepareCloneのエラー = %v", err)
		}
		return f, f.hub.CloneDir("implementer"), r
	}

	t.Run("タスクのブランチを作って取り込み、他のブランチは取り込まない", func(t *testing.T) {
		t.Parallel()

		f, clone, r := setup(t)
		tip := commit(t, clone, "a.go", "package a\n")
		gitRun(t, clone, "branch", "side")
		got, err := f.hub.Import(ctx, r, "plan-1-1", "develop")
		if err != nil {
			t.Fatalf("Importのエラー = %v", err)
		}
		if got != (Import{New: tip}) || !got.Updated() {
			t.Errorf("Import() = %+v、期待値 = %+v", got, Import{New: tip})
		}
		if b := resolve(t, f.hub, "plan-1-1"); b != tip {
			t.Errorf("ハブのplan-1-1 = %q、期待値 = %q", b, tip)
		}
		if b := resolve(t, f.hub, "side"); b != "" {
			t.Errorf("取り込まないブランチsideがハブにある: %s", b)
		}

		// 続けて足したコミットは、ハブにある履歴を送らずに取り込む。
		next := commit(t, clone, "b.go", "package b\n")
		got, err = f.hub.Import(ctx, r, "plan-1-1", "develop")
		if err != nil {
			t.Fatalf("Importのエラー = %v", err)
		}
		if got != (Import{Old: tip, New: next}) {
			t.Errorf("Import() = %+v、期待値 = %+v", got, Import{Old: tip, New: next})
		}

		// コミットが増えていなければ更新しない。
		got, err = f.hub.Import(ctx, r, "plan-1-1", "develop")
		if err != nil {
			t.Fatalf("Importのエラー = %v", err)
		}
		if got.Updated() {
			t.Errorf("Import() = %+v、更新しないことを期待", got)
		}
	})

	for _, tc := range []struct {
		name       string
		base       string
		advance    bool
		wantReject bool
	}{
		{name: "基準を満たす変更なしの取り込みは成功する", base: "develop"},
		{name: "変更なしでも進んだ基準を満たさなければ拒否する", base: "develop", advance: true, wantReject: true},
		{name: "変更なしでも別の基準を満たさなければ拒否する", base: "other-base", wantReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f, clone, r := setup(t)
			tip := commit(t, clone, "task.txt", "task\n")
			if _, err := f.hub.Import(ctx, r, "plan-1-1", "develop"); err != nil {
				t.Fatal(err)
			}
			if tc.advance {
				commit(t, f.upstream, "base.txt", "base\n")
				if err := f.hub.FetchUpstream(ctx, f.upstream, "develop"); err != nil {
					t.Fatal(err)
				}
			} else if tc.base != "develop" {
				other := f.roleRunner("reviewer_codex")
				if err := f.hub.PrepareClone(ctx, other, tc.base, "develop"); err != nil {
					t.Fatal(err)
				}
				commit(t, f.hub.CloneDir("reviewer_codex"), "other.txt", "other\n")
				if _, err := f.hub.Import(ctx, other, tc.base, "develop"); err != nil {
					t.Fatal(err)
				}
			}

			got, err := f.hub.Import(ctx, r, "plan-1-1", tc.base)
			if tc.wantReject {
				wantRejected(t, err, tc.base)
			} else if err != nil || got != (Import{Old: tip, New: tip}) {
				t.Errorf("Import() = %+v、エラー = %v、期待値 = %+v", got, err, Import{Old: tip, New: tip})
			}
			if got := resolve(t, f.hub, "plan-1-1"); got != tip {
				t.Errorf("ハブの先端 = %q、期待値 = %q", got, tip)
			}
		})
	}

	t.Run("クローンの設定に左右されずに取り込む", func(t *testing.T) {
		t.Parallel()

		f, clone, r := setup(t)
		tip := commit(t, clone, "a.go", "package a\n")
		// クローンから直接取り込むと、ハブ側のgitがこの設定を読んでブランチを見失う。
		evil := filepath.Join(t.TempDir(), "evil.config")
		if err := os.WriteFile(evil, []byte("[uploadpack]\n\thideRefs = refs/heads\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitRun(t, clone, "config", "include.path", evil)
		if _, err := f.hub.Import(ctx, r, "plan-1-1", "develop"); err != nil {
			t.Fatalf("Importのエラー = %v", err)
		}
		if b := resolve(t, f.hub, "plan-1-1"); b != tip {
			t.Errorf("ハブのplan-1-1 = %q、期待値 = %q", b, tip)
		}
	})

	t.Run("未コミットの変更があれば取り込まない", func(t *testing.T) {
		t.Parallel()

		f, clone, r := setup(t)
		commit(t, clone, "a.go", "package a\n")
		if err := os.WriteFile(filepath.Join(clone, "b.go"), []byte("package b\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := f.hub.Import(ctx, r, "plan-1-1", "develop")
		wantRejected(t, err, "未コミットの変更")
		if b := resolve(t, f.hub, "plan-1-1"); b != "" {
			t.Errorf("拒否したのにハブにplan-1-1がある: %s", b)
		}
	})

	t.Run("履歴を書き換えた更新は取り込まない", func(t *testing.T) {
		t.Parallel()

		f, clone, r := setup(t)
		tip := commit(t, clone, "a.go", "package a\n")
		if _, err := f.hub.Import(ctx, r, "plan-1-1", "develop"); err != nil {
			t.Fatalf("Importのエラー = %v", err)
		}
		gitRun(t, clone, "commit", "--quiet", "--amend", "-m", "amended")
		_, err := f.hub.Import(ctx, r, "plan-1-1", "develop")
		wantRejected(t, err, "fast-forwardではありません")

		// 巻き戻しも書き換えとして扱う。
		gitRun(t, clone, "reset", "--quiet", "--hard", f.base)
		_, err = f.hub.Import(ctx, r, "plan-1-1", "develop")
		wantRejected(t, err, "fast-forwardではありません")
		if b := resolve(t, f.hub, "plan-1-1"); b != tip {
			t.Errorf("拒否したあとのハブのplan-1-1 = %q、期待値 = %q", b, tip)
		}
	})

	t.Run("基準のブランチを祖先に持たなければ取り込まない", func(t *testing.T) {
		t.Parallel()

		f, clone, r := setup(t)
		gitRun(t, clone, "switch", "--quiet", "--orphan", "plan-1-1-orphan")
		commit(t, clone, "a.go", "package a\n")
		gitRun(t, clone, "branch", "--force", "plan-1-1")
		gitRun(t, clone, "switch", "--quiet", "plan-1-1")
		_, err := f.hub.Import(ctx, r, "plan-1-1", "develop")
		wantRejected(t, err, "develop")
		if b := resolve(t, f.hub, "plan-1-1"); b != "" {
			t.Errorf("拒否したのにハブにplan-1-1がある: %s", b)
		}
	})

	t.Run("クローンにブランチが無ければ取り込まない", func(t *testing.T) {
		t.Parallel()

		f, _, r := setup(t)
		_, err := f.hub.Import(ctx, r, "plan-9-9", "develop")
		wantRejected(t, err, "plan-9-9 がありません")
	})

	t.Run("基準のブランチがハブに無ければエラー", func(t *testing.T) {
		t.Parallel()

		f, _, r := setup(t)
		_, err := f.hub.Import(ctx, r, "plan-1-1", "missing")
		var rejected *RejectedError
		if err == nil || errors.As(err, &rejected) {
			t.Errorf("Importのエラー = %v、*RejectedErrorではないエラーを期待", err)
		}
	})
}

func TestHub_ExportTo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	// setupは2つのタスクのブランチをハブに取り込み、developをチェックアウトした
	// 人間のチェックアウトを作る。
	setup := func(t *testing.T) (fixture, string, []string, []string) {
		t.Helper()
		f := newFixture(t)
		r := f.roleRunner("implementer")
		clone := f.hub.CloneDir("implementer")
		branches := []string{"plan-1-1", "plan-1-2"}
		var tips []string
		start := "develop"
		for _, b := range branches {
			if err := f.hub.PrepareClone(ctx, r, b, start); err != nil {
				t.Fatalf("PrepareCloneのエラー = %v", err)
			}
			tips = append(tips, commit(t, clone, b+".txt", b+"\n"))
			if _, err := f.hub.Import(ctx, r, b, start); err != nil {
				t.Fatalf("Importのエラー = %v", err)
			}
			start = b
		}
		checkout := filepath.Join(t.TempDir(), "mewst")
		gitRun(t, filepath.Dir(checkout), "clone", "--quiet", "--branch", "develop", f.upstream, checkout)
		return f, checkout, branches, tips
	}

	t.Run("作業ツリーに変更が無ければ、取り込んで最後のブランチに切り替える", func(t *testing.T) {
		t.Parallel()

		f, checkout, branches, tips := setup(t)
		got, err := f.hub.ExportTo(ctx, checkout, branches)
		if err != nil {
			t.Fatalf("ExportToのエラー = %v", err)
		}
		if !got.Switched || len(got.Skipped) != 0 {
			t.Errorf("ExportTo() = %+v、切り替えを期待", got)
		}
		for i, b := range branches {
			if c := gitRun(t, checkout, "rev-parse", "refs/heads/"+b); c != tips[i] {
				t.Errorf("チェックアウトの%s = %q、期待値 = %q", b, c, tips[i])
			}
		}
		if c := gitRun(t, checkout, "symbolic-ref", "--short", "HEAD"); c != "plan-1-2" {
			t.Errorf("チェックアウトのブランチ = %q、期待値 = %q", c, "plan-1-2")
		}
	})

	t.Run("チェックアウトしているブランチはfast-forwardで進める", func(t *testing.T) {
		t.Parallel()

		f, checkout, branches, tips := setup(t)
		gitRun(t, checkout, "switch", "--quiet", "-c", "plan-1-2", "develop")
		got, err := f.hub.ExportTo(ctx, checkout, branches)
		if err != nil {
			t.Fatalf("ExportToのエラー = %v", err)
		}
		if !got.Switched {
			t.Errorf("ExportTo() = %+v、切り替えを期待", got)
		}
		if c := gitRun(t, checkout, "rev-parse", "HEAD"); c != tips[1] {
			t.Errorf("チェックアウトの先端 = %q、期待値 = %q", c, tips[1])
		}
	})

	for _, tc := range []struct {
		name     string
		diverged bool
	}{
		{name: "現在のブランチがハブより先なら拒否する"},
		{name: "現在のブランチがハブから分岐していたら拒否する", diverged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f, checkout, branches, _ := setup(t)
			if _, err := f.hub.ExportTo(ctx, checkout, branches); err != nil {
				t.Fatal(err)
			}
			if tc.diverged {
				gitRun(t, checkout, "reset", "--quiet", "--hard", f.base)
			}
			mine := commit(t, checkout, "mine.txt", "mine\n")
			got, err := f.hub.ExportTo(ctx, checkout, branches)
			if err == nil || !strings.Contains(err.Error(), "祖先") {
				t.Errorf("ExportToのエラー = %v、祖先の条件で拒否することを期待", err)
			}
			if got.Switched {
				t.Errorf("ExportTo() = %+v、切り替えないことを期待", got)
			}
			if got := gitRun(t, checkout, "rev-parse", "HEAD"); got != mine {
				t.Errorf("人間の先端 = %q、期待値 = %q", got, mine)
			}
			if got := gitRun(t, checkout, "status", "--porcelain"); got != "" {
				t.Errorf("作業ツリーの状態 = %q、変更が無いことを期待", got)
			}
		})
	}

	t.Run("作業ツリーに変更があれば、取り込みだけを行う", func(t *testing.T) {
		t.Parallel()

		f, checkout, branches, tips := setup(t)
		gitRun(t, checkout, "switch", "--quiet", "-c", "plan-1-1", "develop")
		if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("editing\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := f.hub.ExportTo(ctx, checkout, branches)
		if err != nil {
			t.Fatalf("ExportToのエラー = %v", err)
		}
		if got.Switched || len(got.Skipped) != 1 || got.Skipped[0] != "plan-1-1" {
			t.Errorf("ExportTo() = %+v、plan-1-1を飛ばして切り替えないことを期待", got)
		}
		if c := gitRun(t, checkout, "rev-parse", "refs/heads/plan-1-2"); c != tips[1] {
			t.Errorf("チェックアウトのplan-1-2 = %q、期待値 = %q", c, tips[1])
		}
		if c := gitRun(t, checkout, "rev-parse", "HEAD"); c != f.base {
			t.Errorf("チェックアウトの先端 = %q、変えないことを期待", c)
		}
		if b, err := os.ReadFile(filepath.Join(checkout, "README.md")); err != nil || string(b) != "editing\n" { //#nosec G304 -- テストが作ったチェックアウトのファイル
			t.Errorf("作業ツリーの変更が失われた: %q, %v", b, err)
		}
	})

	t.Run("人間のブランチを上書きしない", func(t *testing.T) {
		t.Parallel()

		f, checkout, branches, _ := setup(t)
		gitRun(t, checkout, "switch", "--quiet", "-c", "plan-1-2", "develop")
		mine := commit(t, checkout, "mine.txt", "mine\n")
		gitRun(t, checkout, "switch", "--quiet", "develop")
		if _, err := f.hub.ExportTo(ctx, checkout, branches); err == nil {
			t.Error("fast-forwardにならない取り込みでエラーを期待したが、nilだった")
		}
		if c := gitRun(t, checkout, "rev-parse", "refs/heads/plan-1-2"); c != mine {
			t.Errorf("チェックアウトのplan-1-2 = %q、人間のコミット %q のままを期待", c, mine)
		}
	})

	t.Run("ハブに無いブランチはエラー", func(t *testing.T) {
		t.Parallel()

		f, checkout, _, _ := setup(t)
		if _, err := f.hub.ExportTo(ctx, checkout, []string{"plan-9-9"}); err == nil || !strings.Contains(err.Error(), "plan-9-9 がハブにありません") {
			t.Errorf("ExportToのエラー = %v", err)
		}
	})
}

func TestHub_Pin(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := context.Background()
	got, err := f.hub.Pin(ctx, "plan-base", "develop")
	if err != nil {
		t.Fatalf("Pinのエラー = %v", err)
	}
	if got != f.base || resolve(t, f.hub, "plan-base") != f.base {
		t.Errorf("固定したコミット = %q、期待値 = %q", got, f.base)
	}

	// base_branchがあとから進んでも、固定したブランチは動かさない。
	next := commit(t, f.upstream, "CHANGELOG.md", "next\n")
	if err := f.hub.FetchUpstream(ctx, f.upstream, "develop"); err != nil {
		t.Fatalf("FetchUpstreamのエラー = %v", err)
	}
	if resolve(t, f.hub, "develop") != next {
		t.Fatal("ハブのdevelopが進んでいない")
	}
	got, err = f.hub.Pin(ctx, "plan-base", "develop")
	if err != nil {
		t.Fatalf("2回目のPinのエラー = %v", err)
	}
	if got != f.base || resolve(t, f.hub, "plan-base") != f.base {
		t.Errorf("2回目に固定したコミット = %q、期待値 = %q (最初に固定したまま)", got, f.base)
	}

	if _, err := f.hub.Pin(ctx, "plan2-base", "missing"); err == nil {
		t.Error("元のブランチが無いときのエラー = nil、エラーを期待")
	}
	if _, err := f.hub.Pin(ctx, "-bad", "develop"); err == nil {
		t.Error("不正なブランチの名前のエラー = nil、エラーを期待")
	}
}
