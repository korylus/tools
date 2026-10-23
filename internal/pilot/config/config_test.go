package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mewstConfigは作業計画書の設定例に沿った、Mewstの設定ファイルの例。
const mewstConfig = `
project = "mewst"
base_branch = "develop"
max_reviews = 3
reviewers = ["reviewer_codex", "reviewer_claude"]

[usage]
five_hour_cap = 90
weekly_cap = 80
safety_factor = 1.5
five_hour_estimate = 10
weekly_estimate = 2

[roles.designer]
tool = "claude"
model = "claude-opus-5-5"
effort = "medium"
runner = ["docker", "compose", "run", "--rm", "-T", "pilot-designer"]

[roles.implementer]
tool = "claude"
model = "claude-opus-5-5"
effort = "medium"
runner = ["docker", "compose", "run", "--rm", "-T", "pilot-implementer"]

[roles.reviewer_codex]
tool = "codex"
model = "gpt-6-astra"
effort = "xhigh"
fix_effort = "medium"
sandbox = "danger-full-access"
runner = ["docker", "compose", "run", "--rm", "-T", "pilot-reviewer-codex"]

[roles.reviewer_claude]
tool = "claude"
model = "claude-opus-5-5"
effort = "xhigh"
fix_effort = "medium"
runner = ["docker", "compose", "run", "--rm", "-T", "pilot-reviewer-claude"]

[hub]
upstream = "https://github.com/mewstcom/mewst.git"
path = "/koryluspilot/hub.git"

[report]
addr = ":4190"
setup = ["make -C go db-migrate"]

[gate.commands]
Go = ["make -C go lint", "make -C go test"]
Rails = ["make -C rails lint", "make -C rails test"]

[[protected]]
name = "schema"
label = "DBスキーマ・マイグレーション"
paths = ["go/db/migrations/**", "go/db/schema.sql"]
review_points = ["Rails側から参照している列・テーブルへの影響", "ロックの長さ", "ロールバックできるか"]

[[protected]]
name = "dependency"
label = "依存パッケージ"
paths = ["go/go.mod", "rails/Gemfile"]
on_undeclared = "report"

[[protected]]
name = "transaction"
label = "トランザクション境界"
go_funcs_calling = ["BeginTx"]
patterns = ['BeginTx\(']

[[protected]]
name = "module-dependency"
label = "モジュール間の依存関係"
go_import_prefix = "github.com/mewstcom/mewst/go/internal/"
paths = ["go/.golangci.yml"]

[gate]
runner = ["docker", "compose", "run", "--rm", "-T", "pilot-gate"]
suppress_patterns = ['//nolint', 't\.Skip\(', '# rubocop:disable']
test_globs = ["**/*_test.go", "rails/spec/**"]
generated_globs = ["**/*_templ.go", "**/*.sql.go"]
size_factor = 2.0

[[gate.watch]]
name = "docs-private"
path = "/home/exedev/mewst/docs/private"
project_path = "docs/private"

[[gate.watch]]
name = "implementer-claude"
path = "/home/developer/.claude"
runner = ["docker", "compose", "run", "--rm", "-T", "pilot-implementer"]
exclude = ["projects/**", "todos/**"]
`

func TestParse(t *testing.T) {
	t.Parallel()

	cfg, err := Parse(mewstConfig)
	if err != nil {
		t.Fatalf("Parseのエラー = %v", err)
	}

	if cfg.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q、期待値 = %q", cfg.BaseBranch, "develop")
	}
	if cfg.Project != "mewst" {
		t.Errorf("Project = %q、期待値 = %q", cfg.Project, "mewst")
	}
	if want := []string{"reviewer_codex", "reviewer_claude"}; !reflect.DeepEqual(cfg.Reviewers, want) {
		t.Errorf("Reviewers = %v、期待値 = %v", cfg.Reviewers, want)
	}
	if want := (Hub{Upstream: "https://github.com/mewstcom/mewst.git", Path: "/koryluspilot/hub.git"}); cfg.Hub != want {
		t.Errorf("Hub = %+v、期待値 = %+v", cfg.Hub, want)
	}
	if len(cfg.Roles) != 4 {
		t.Errorf("Rolesの数 = %d、期待値 = 4", len(cfg.Roles))
	}
	wantCommands := map[string][]string{
		"Go":    {"make -C go lint", "make -C go test"},
		"Rails": {"make -C rails lint", "make -C rails test"},
	}
	if !reflect.DeepEqual(cfg.Gate.Commands, wantCommands) {
		t.Errorf("Gate.Commands = %v、期待値 = %v", cfg.Gate.Commands, wantCommands)
	}
	if want := []string{"make -C go db-migrate"}; !reflect.DeepEqual(cfg.Report.Setup, want) {
		t.Errorf("Report.Setup = %v、期待値 = %v", cfg.Report.Setup, want)
	}
	if cfg.Gate.SizeFactor != 2.0 {
		t.Errorf("Gate.SizeFactor = %g、期待値 = 2", cfg.Gate.SizeFactor)
	}
	gotNames := make([]string, len(cfg.Protected))
	for i, p := range cfg.Protected {
		gotNames[i] = p.Name
	}
	wantNames := []string{"schema", "dependency", "transaction", "module-dependency"}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("保護領域の名前 = %v、期待値 = %v", gotNames, wantNames)
	}
	if want := []string{"Rails側から参照している列・テーブルへの影響", "ロックの長さ", "ロールバックできるか"}; !reflect.DeepEqual(cfg.Protected[0].ReviewPoints, want) {
		t.Errorf("Protected[0].ReviewPoints = %v、期待値 = %v", cfg.Protected[0].ReviewPoints, want)
	}
	if cfg.Protected[3].GoImportPrefix != "github.com/mewstcom/mewst/go/internal/" {
		t.Errorf("Protected[3].GoImportPrefix = %q", cfg.Protected[3].GoImportPrefix)
	}
	gotActions := make([]string, len(cfg.Protected))
	for i, p := range cfg.Protected {
		gotActions[i] = p.OnUndeclared
	}
	// 省略したon_undeclaredはstopになる。
	wantActions := []string{"stop", "report", "stop", "stop"}
	if !reflect.DeepEqual(gotActions, wantActions) {
		t.Errorf("保護領域のon_undeclared = %v、期待値 = %v", gotActions, wantActions)
	}
	wantRunner := []string{"docker", "compose", "run", "--rm", "-T", "pilot-gate"}
	if !reflect.DeepEqual(cfg.Gate.Runner, wantRunner) {
		t.Errorf("Gate.Runner = %v、期待値 = %v", cfg.Gate.Runner, wantRunner)
	}
	wantWatch := []Watch{
		{Name: "docs-private", Path: "/home/exedev/mewst/docs/private", ProjectPath: "docs/private"},
		{
			Name:    "implementer-claude",
			Path:    "/home/developer/.claude",
			Runner:  []string{"docker", "compose", "run", "--rm", "-T", "pilot-implementer"},
			Exclude: []string{"projects/**", "todos/**"},
		},
	}
	if !reflect.DeepEqual(cfg.Gate.Watch, wantWatch) {
		t.Errorf("Gate.Watch = %+v、期待値 = %+v", cfg.Gate.Watch, wantWatch)
	}
}

// minimalConfigは、省略できない項目だけを書いた設定ファイル。
const minimalConfig = `
project = "tools"
base_branch = "main"
reviewers = ["reviewer_a", "reviewer_b"]

[hub]
upstream = "https://github.com/korylus/tools.git"
path = "/hub.git"

[roles.designer]
tool = "claude"
runner = ["designer"]

[roles.implementer]
tool = "claude"
runner = ["implementer"]

[roles.reviewer_a]
tool = "codex"
runner = ["reviewer-a"]

[roles.reviewer_b]
tool = "claude"
runner = ["reviewer-b"]
`

func TestParse_Defaults(t *testing.T) {
	t.Parallel()

	cfg, err := Parse(minimalConfig)
	if err != nil {
		t.Fatalf("Parseのエラー = %v", err)
	}

	if cfg.MaxReviews != 3 {
		t.Errorf("MaxReviews = %d、期待値 = 3", cfg.MaxReviews)
	}
	want := Usage{FiveHourCap: 90, WeeklyCap: 80, SafetyFactor: 1.5, FiveHourEstimate: 10, WeeklyEstimate: 2}
	if cfg.Usage != want {
		t.Errorf("Usage = %+v、期待値 = %+v", cfg.Usage, want)
	}
	if got := DefaultUsage(); got != want {
		t.Errorf("DefaultUsage() = %+v、期待値 = %+v", got, want)
	}
	if cfg.Gate.SizeFactor != 2.0 {
		t.Errorf("Gate.SizeFactor = %g、期待値 = 2", cfg.Gate.SizeFactor)
	}
	if cfg.Report.Addr != "127.0.0.1:4190" {
		t.Errorf("Report.Addr = %q、期待値 = %q", cfg.Report.Addr, "127.0.0.1:4190")
	}
}

func TestParse_Invalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want []string // エラーメッセージに含まれることを期待する文字列
	}{
		{
			name: "構文の誤り",
			src:  "base_branch = \n",
			want: []string{"line 1"},
		},
		{
			name: "base_branchの省略",
			src:  "max_reviews = 3",
			want: []string{"base_branch を指定してください"},
		},
		{
			name: "未知のキーは綴りの誤りとして止める",
			src:  "base_branch = \"develop\"\n[roles.implementer]\nefort = \"medium\"\n",
			want: []string{`未知のキー "roles.implementer.efort"`},
		},
		{
			name: "0を明示した値は既定値で埋めない",
			src:  "base_branch = \"develop\"\nmax_reviews = 0\n[usage]\nweekly_cap = 0\n",
			want: []string{"max_reviews は1以上", "usage.weekly_cap は0より大きく100以下"},
		},
		{
			name: "使用率の上限が100を超える",
			src:  "base_branch = \"develop\"\n[usage]\nfive_hour_cap = 120\nsafety_factor = 0.5\n",
			want: []string{"usage.five_hour_cap", "usage.safety_factor は1以上"},
		},
		{
			name: "見積もりの初期値が範囲の外",
			src:  "base_branch = \"develop\"\n[usage]\nfive_hour_estimate = -1\nweekly_estimate = 101\n",
			want: []string{"usage.five_hour_estimate は0以上100以下", "usage.weekly_estimate は0以上100以下"},
		},
		{
			name: "ツールが受け付けないeffort",
			src: "base_branch = \"develop\"\n" +
				"[roles.implementer]\ntool = \"claude\"\neffort = \"ultra\"\nrunner = ['x']\n" +
				"[roles.reviewer]\ntool = \"codex\"\nfix_effort = \"huge\"\nrunner = ['x']\n",
			want: []string{`roles.implementer.effort に "ultra"`, `roles.reviewer.fix_effort に "huge"`},
		},
		{
			name: "Claude Codeのサンドボックスと、Codexが受け付けないサンドボックス",
			src: "base_branch = \"develop\"\n" +
				"[roles.implementer]\ntool = \"claude\"\nsandbox = \"read-only\"\nrunner = ['x']\n" +
				"[roles.reviewer]\ntool = \"codex\"\nsandbox = \"none\"\nrunner = ['x']\n",
			want: []string{"roles.implementer.sandbox は指定できません", `roles.reviewer.sandbox に "none"`},
		},
		{
			name: "役割の省略・ツールの誤り・ランナーの省略",
			src: "base_branch = \"develop\"\n" +
				"[roles.implementer]\n" +
				"[roles.Reviewer]\ntool = \"gemini\"\nrunner = ['docker', ' ']\n",
			want: []string{
				"roles.designer を指定してください",
				"roles.implementer.tool を指定してください",
				"roles.implementer.runner を指定してください",
				"roles.Reviewer の名前には英小文字",
				`roles.Reviewer.tool に "gemini" は指定できません`,
				"roles.Reviewer.runner[1] が空です",
			},
		},
		{
			name: "レビュアーの省略",
			src:  "base_branch = \"develop\"\n",
			want: []string{"reviewers を指定してください"},
		},
		{
			name: "レビュアーの誤り",
			src: "base_branch = \"develop\"\nreviewers = [\"reviewer\", \"implementer\", \"reviewer\", \"missing\"]\n" +
				"[roles.implementer]\ntool = \"claude\"\nrunner = ['x']\n" +
				"[roles.reviewer]\ntool = \"codex\"\nrunner = ['x']\n",
			want: []string{
				`reviewers[1] に "implementer" は指定できません`,
				`reviewers[2] の役割 "reviewer" が重複`,
				`reviewers[3] の役割 "missing" が roles にありません`,
			},
		},
		{
			name: "再レビューするのにレビュアーが1つ",
			src:  "base_branch = \"develop\"\nreviewers = [\"reviewer\"]\n[roles.reviewer]\ntool = \"codex\"\nrunner = ['x']\n",
			want: []string{"reviewers に2つ以上の役割を指定してください"},
		},
		{
			name: "プロジェクトの名前とハブの誤り",
			src:  "project = \"Mewst/go\"\nbase_branch = \"develop\"\n[hub]\nupstream = \"git@github.com:mewstcom/mewst.git\"\npath = \"hub.git\"\n",
			want: []string{
				`project "Mewst/go" には英小文字`,
				`hub.upstream "git@github.com:mewstcom/mewst.git" には認証なしで読めるhttpsのURL`,
				`hub.path "hub.git" は正規化した絶対パス`,
			},
		},
		{
			name: "プロジェクトとハブの省略",
			src:  "base_branch = \"develop\"\n",
			want: []string{"project を指定してください", `hub.upstream ""`, `hub.path ""`},
		},
		{
			name: "空のゲートのコマンドと不正な正規表現・glob",
			src:  "base_branch = \"develop\"\n[gate]\nrunner = ['x']\nsuppress_patterns = ['(']\ntest_globs = ['[']\n[gate.commands]\nGo = ['']\n",
			want: []string{"gate.commands.Go[0] が空です", "gate.suppress_patterns[0] の正規表現が不正", "gate.test_globs[0] のglob"},
		},
		{
			name: "保護領域の名前の誤りと重複",
			src: "base_branch = \"develop\"\n" +
				"[[protected]]\nname = \"DB Schema\"\nlabel = \"a\"\npaths = [\"x\"]\n" +
				"[[protected]]\nname = \"schema\"\nlabel = \"b\"\npaths = [\"x\"]\n" +
				"[[protected]]\nname = \"schema\"\nlabel = \"c\"\npaths = [\"x\"]\n",
			want: []string{`protected[0].name "DB Schema" には英小文字`, `protected[2].name "schema" が重複`},
		},
		{
			name: "保護領域のラベルと検知方法の省略",
			src:  "base_branch = \"develop\"\n[[protected]]\nname = \"schema\"\n",
			want: []string{"protected[0].label を指定してください", "検知方法"},
		},
		{
			name: "空白だけのゲートのコマンド",
			src:  "base_branch = \"develop\"\n[gate]\nrunner = ['x']\n[gate.commands]\nGo = ['make test', '  ']\n",
			want: []string{"gate.commands.Go[1] が空です"},
		},
		{
			name: "ゲートのコマンドがあるのにランナーが無い",
			src:  "base_branch = \"develop\"\n[gate.commands]\nGo = ['make test']\n",
			want: []string{"gate.commands を使うときは gate.runner を指定してください"},
		},
		{
			name: "ランナーの空の要素",
			src:  "base_branch = \"develop\"\n[gate]\nrunner = ['docker', ' ']\n",
			want: []string{"gate.runner[1] が空です"},
		},
		{
			name: "監視するパスの名前の誤りと重複",
			src: "base_branch = \"develop\"\n" +
				"[[gate.watch]]\npath = \"/a\"\n" +
				"[[gate.watch]]\nname = \"Docs\"\npath = \"/a\"\n" +
				"[[gate.watch]]\nname = \"docs\"\npath = \"/a\"\n" +
				"[[gate.watch]]\nname = \"docs\"\npath = \"/a\"\n",
			want: []string{"gate.watch[0].name を指定してください", `gate.watch[1].name "Docs" には英小文字`, `gate.watch[3].name "docs" が重複`},
		},
		{
			name: "監視するパスが相対パス・正規化されていない",
			src: "base_branch = \"develop\"\n" +
				"[[gate.watch]]\nname = \"a\"\npath = \"docs/private\"\n" +
				"[[gate.watch]]\nname = \"b\"\npath = \"/home/dev/.claude/\"\n" +
				"[[gate.watch]]\nname = \"c\"\n",
			want: []string{`gate.watch[0].path "docs/private" は正規化した絶対パス`, `gate.watch[1].path "/home/dev/.claude/"`, `gate.watch[2].path "" は`},
		},
		{
			name: "監視するパスのランナー・除外・プロジェクトのパスの誤り",
			src: "base_branch = \"develop\"\n" +
				"[[gate.watch]]\nname = \"a\"\npath = \"/a\"\nrunner = ['docker', '']\nexclude = ['[']\nproject_path = \"../docs\"\n" +
				"[[gate.watch]]\nname = \"b\"\npath = \"/b\"\nproject_path = \"/workspace/docs\"\n",
			want: []string{"gate.watch[0].runner[1] が空です", "gate.watch[0].exclude[0] のglob", `gate.watch[0].project_path "../docs"`, `gate.watch[1].project_path "/workspace/docs"`},
		},
		{
			name: "on_undeclaredの誤り",
			src:  "base_branch = \"develop\"\n[[protected]]\nname = \"dependency\"\nlabel = \"a\"\npaths = [\"go.mod\"]\non_undeclared = \"warn\"\n",
			want: []string{`protected[0].on_undeclared に "warn" は指定できません`},
		},
		{
			name: "保護領域の空白だけの呼び出し名",
			src:  "base_branch = \"develop\"\n[[protected]]\nname = \"transaction\"\nlabel = \"a\"\ngo_funcs_calling = ['BeginTx', ' ']\n",
			want: []string{"protected[0].go_funcs_calling[1] が空です"},
		},
		{
			name: "保護領域の呼び出し名に修飾を付ける",
			src:  "base_branch = \"develop\"\n[[protected]]\nname = \"transaction\"\nlabel = \"a\"\ngo_funcs_calling = ['db.BeginTx', 'BeginTx(']\n",
			want: []string{
				`protected[0].go_funcs_calling[0] "db.BeginTx" には呼び出す関数・メソッドの名前だけを書いてください`,
				`protected[0].go_funcs_calling[1] "BeginTx(" には呼び出す関数・メソッドの名前だけを書いてください`,
			},
		},
		{
			name: "保護領域の空白だけの観点",
			src:  "base_branch = \"develop\"\n[[protected]]\nname = \"schema\"\nlabel = \"a\"\npaths = ['db/**']\nreview_points = ['ロックの長さ', ' ']\n",
			want: []string{"protected[0].review_points[1] が空です"},
		},
		{
			name: "作業報告書の待ち受けアドレスにポートが無い",
			src:  "base_branch = \"develop\"\n[report]\naddr = \"localhost\"\n",
			want: []string{`report.addr: "localhost" は <ホスト>:<ポート> の形で指定してください`},
		},
		{
			name: "作業報告書の待ち受けアドレスのポートが範囲外",
			src:  "base_branch = \"develop\"\n[report]\naddr = \":65536\"\n",
			want: []string{`report.addr: ":65536" のポートは0から65535の数で指定してください`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tt.src)
			if err == nil {
				t.Fatal("エラーが返らなかった")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("エラー = %q、含まれることを期待した文字列 = %q", err.Error(), w)
				}
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(mewstConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Loadのエラー = %v", err)
	}
	want := Role{
		Tool:      ToolCodex,
		Model:     "gpt-6-astra",
		Effort:    "xhigh",
		FixEffort: "medium",
		Sandbox:   "danger-full-access",
		Runner:    []string{"docker", "compose", "run", "--rm", "-T", "pilot-reviewer-codex"},
	}
	if got := cfg.Roles["reviewer_codex"]; !reflect.DeepEqual(got, want) {
		t.Errorf("Roles[reviewer_codex] = %+v、期待値 = %+v", got, want)
	}

	_, err = Load(filepath.Join(dir, "missing.toml"))
	if err == nil || !strings.Contains(err.Error(), "missing.toml を読み込めません") {
		t.Errorf("存在しないファイルのエラー = %v", err)
	}

	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("max_reviews = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(bad)
	if err == nil || !strings.Contains(err.Error(), "bad.toml の設定が不正です") {
		t.Errorf("不正な設定のエラー = %v", err)
	}
}

func TestConfig_RoleUsing(t *testing.T) {
	t.Parallel()

	cfg, err := Parse(mewstConfig)
	if err != nil {
		t.Fatalf("Parseのエラー = %v", err)
	}
	tests := []struct {
		name       string
		tool       string
		wantRunner []string
		wantOK     bool
	}{
		// Claude Codeを使う役割のうち、名前の順で最初のdesignerを返す。
		{"Claude Code", ToolClaude, []string{"docker", "compose", "run", "--rm", "-T", "pilot-designer"}, true},
		{"Codex", ToolCodex, []string{"docker", "compose", "run", "--rm", "-T", "pilot-reviewer-codex"}, true},
		{"使う役割の無いツール", "gemini", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			role, ok := cfg.RoleUsing(tt.tool)
			if ok != tt.wantOK || !reflect.DeepEqual(role.Runner, tt.wantRunner) {
				t.Errorf("RoleUsing(%q) = (%v, %v)、期待値 = (%v, %v)", tt.tool, role.Runner, ok, tt.wantRunner, tt.wantOK)
			}
		})
	}
}

func TestParse_NonFinite(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"usage.five_hour_cap", "usage.weekly_cap", "usage.safety_factor", "usage.five_hour_estimate", "usage.weekly_estimate", "gate.size_factor"} {
		for _, value := range []string{"nan", "+inf", "-inf"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				t.Parallel()
				table, field, _ := strings.Cut(key, ".")
				src := minimalConfig + "\n[" + table + "]\n" + field + " = " + value + "\n"
				_, err := Parse(src)
				if err == nil {
					t.Fatalf("%s = %sでエラーを期待したが、nilだった", key, value)
				}
				if !strings.Contains(err.Error(), key) {
					t.Errorf("エラー = %q、含まれることを期待した設定名 = %q", err.Error(), key)
				}
			})
		}
	}
}
