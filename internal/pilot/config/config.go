// Package configはkoryluspilotの設定ファイル (koryluspilot.toml) を読み込み、
// 検証する。
//
// 設定ファイルはプロジェクトの公開リポジトリのルートに置き、保護領域・ゲートの
// コマンド・使用量の上限・役割ごとのモデルとeffortを定義する。
// 省略できる項目には既定値を入れ、値の誤りはまとめて報告する。
package config

import (
	"errors"
	"fmt"
	"go/token"
	"maps"
	"math"
	"net"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// FileNameは設定ファイルの既定のファイル名。
const FileName = "koryluspilot.toml"

// 省略時の既定値。
const (
	defaultMaxReviews   = 3
	defaultFiveHourCap  = 90
	defaultWeeklyCap    = 80
	defaultSafetyFactor = 1.5
	// 1ステップの消費量の見積もりの初期値 (百分率)。消費の履歴が無いうちに使う。
	defaultFiveHourEstimate = 10
	defaultWeeklyEstimate   = 2
	defaultSizeFactor       = 2.0
	// 作業報告書はベーシック認証を掛けたリバースプロキシの後ろからだけ届かせるため、
	// 既定ではVMの外から届かないループバックで待ち受ける。
	defaultReportAddr = "127.0.0.1:4190"
)

// claudeEffortsとcodexEffortsは各ツールが受け付けるeffortの値。
var (
	claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}
	codexEfforts  = []string{"low", "medium", "high", "xhigh", "max", "ultra"}
)

// codexSandboxesはCodexが受け付けるサンドボックスのモード。
var codexSandboxes = []string{"read-only", "workspace-write", "danger-full-access"}

// 役割が使うツール。
const (
	ToolClaude = "claude"
	ToolCodex  = "codex"
)

// koryluspilotが流れの中で名前で使う役割。レビュアーの役割はreviewersで指定する。
const (
	RoleDesigner    = "designer"
	RoleImplementer = "implementer"
)

// reRoleNameは役割の名前に使える文字。
var reRoleName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// on_undeclaredの値。宣言の無い保護領域への変更で止めるか、作業報告書に載せて進めるか。
const (
	OnUndeclaredStop   = "stop"
	OnUndeclaredReport = "report"
)

// reProtectedNameは保護領域の名前に使える文字。計画書の `**保護領域**:` の行で
// 「名前 (触れる内容)」の形に区切って読むため、空白・括弧・カンマを含めない。
var reProtectedName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Configは設定ファイル全体。
type Config struct {
	// Projectはプロジェクトの名前。ハブと役割のクローンを置くディレクトリの名前に使う。
	Project string `toml:"project"`
	// BaseBranchは最初のタスクのブランチを切る元のブランチ。
	BaseBranch string `toml:"base_branch"`
	// MaxReviewsは1タスクで行うレビューの回数の上限。
	MaxReviews int `toml:"max_reviews"`
	// Reviewersはレビューに使う役割の名前。1回目から順に使い、末尾の次は先頭に戻る。
	Reviewers []string        `toml:"reviewers"`
	Usage     Usage           `toml:"usage"`
	Roles     map[string]Role `toml:"roles"`
	Hub       Hub             `toml:"hub"`
	Report    Report          `toml:"report"`
	Gate      Gate            `toml:"gate"`
	Protected []Protected     `toml:"protected"`
}

// Usageは自動実行が使ってよい使用率の上限 (百分率) と、見積もりに掛ける安全係数。
type Usage struct {
	FiveHourCap  float64 `toml:"five_hour_cap"`
	WeeklyCap    float64 `toml:"weekly_cap"`
	SafetyFactor float64 `toml:"safety_factor"`
	// FiveHourEstimateとWeeklyEstimateは、消費の履歴が無いステップに使う
	// 1ステップの消費量の見積もり (百分率)。
	FiveHourEstimate float64 `toml:"five_hour_estimate"`
	WeeklyEstimate   float64 `toml:"weekly_estimate"`
}

// DefaultUsageは使用量の設定をすべて省略したときの値を返す。設定ファイルの
// 無い場所で使用率だけを確かめるときに使う。
func DefaultUsage() Usage {
	return Usage{
		FiveHourCap:      defaultFiveHourCap,
		WeeklyCap:        defaultWeeklyCap,
		SafetyFactor:     defaultSafetyFactor,
		FiveHourEstimate: defaultFiveHourEstimate,
		WeeklyEstimate:   defaultWeeklyEstimate,
	}
}

// Roleは1つの役割の設定。
type Role struct {
	// Toolは役割が使うツール (ToolClaude / ToolCodex)。
	Tool string `toml:"tool"`
	// ModelとEffortはツールに渡すモデルとeffort。空文字はツール側の既定値を使うことを表す。
	Model  string `toml:"model"`
	Effort string `toml:"effort"`
	// FixEffortは、レビューのあとの修正とゲートからの差し戻しに使うeffort。
	// 空文字はEffortを使うことを表す。
	FixEffort string `toml:"fix_effort"`
	// SandboxはCodexがコマンドを実行するサンドボックスのモード (Codexだけが持つ)。
	// 空文字はCodexの設定ファイルの既定値を使うことを表す。
	Sandbox string `toml:"sandbox"`
	// Runnerは役割のコンテナでコマンドを起動するための接頭辞。koryluspilotは
	// この後ろに `claude` / `codex` / gitのコマンドを付けて実行する。
	Runner []string `toml:"runner"`
}

// RoleUsingは、toolを使う役割のうち名前の順で最初のものを返す。使用率の取得の
// ように、そのツールの認証を持つどの役割のランナーでもよいときに使う。
func (c *Config) RoleUsing(tool string) (Role, bool) {
	for _, name := range slices.Sorted(maps.Keys(c.Roles)) {
		if c.Roles[name].Tool == tool {
			return c.Roles[name], true
		}
	}
	return Role{}, false
}

// Hubはハブ (役割のクローンの変更を受け渡すベアリポジトリ) の設定。
type Hub struct {
	// Upstreamはbase_branchを取り込むGitHubのリポジトリのURL。認証なしで読む。
	Upstream string `toml:"upstream"`
	// Pathは役割とゲートのコンテナの中から見たハブのパス。
	Path string `toml:"path"`
}

// Reportは作業報告書の配信の設定。
type Report struct {
	// Addrは作業報告書を配信するHTTPサーバーの待ち受けアドレス。
	Addr string `toml:"addr"`
	// Setupは、動作確認の前に人間のチェックアウトで実行するコマンド (マイグレーションの
	// 適用など)。作業報告書の「動作確認の前の準備」に手順として載せ、koryluspilotは実行しない。
	Setup []string `toml:"setup"`
}

// Gateはゲート検査の設定。
type Gate struct {
	// Runnerはゲートのコンテナでコマンドを起動するための接頭辞。koryluspilotは
	// この後ろに検査のコマンドを付けて実行する。
	Runner []string `toml:"runner"`
	// Commandsはタスクのプラットフォーム接頭辞 (Go / Railsなど) ごとに実行するコマンド。
	Commands map[string][]string `toml:"commands"`
	// SuppressPatternsは追加行に含まれていたら止める抑制の正規表現。
	SuppressPatterns []string `toml:"suppress_patterns"`
	// TestGlobsはテストファイルのglob。テストの削除の検知と行数の除外に使う。
	TestGlobs []string `toml:"test_globs"`
	// GeneratedGlobsは生成物のglob。行数の除外に使う。
	GeneratedGlobs []string `toml:"generated_globs"`
	// SizeFactorは実装の行数が想定行数の何倍を超えたら止めるか。
	SizeFactor float64 `toml:"size_factor"`
	// Watchはリポジトリの外の変更を検知するために、エージェントのステップの
	// 前後で状態を比べるパス。
	Watch []Watch `toml:"watch"`
}

// Watchはリポジトリの外の変更を監視するパス1件。役割のコンテナに書き込み可能で
// マウントした、クローン以外のパス (docs/private・~/.claudeなど) を指定する。
type Watch struct {
	// Nameは停止理由に表示する名前。役割のコンテナごとに同じパスを監視する
	// ことがあるため、パスとは別に持たせる。
	Name string `toml:"name"`
	// Pathはランナーの中から見た監視するパス (絶対パス)。ファイルでもディレクトリでもよい。
	Path string `toml:"path"`
	// Runnerは監視するパスを読むコンテナでコマンドを起動するための接頭辞。
	// 空ならkoryluspilotと同じ環境で直接読む。
	Runner []string `toml:"runner"`
	// Excludeは比べないパスのglob (Pathからの相対パス)。ツールがステップの
	// たびに書くセッションの記録などを指定する。
	Exclude []string `toml:"exclude"`
	// ProjectPathは、Pathがプロジェクトのチェックアウトのどのパスに当たるか
	// (例: docs/private)。koryluspilotがエージェントに書かせるパスを、
	// プロジェクトからの相対パスで比較から除くために使う。
	ProjectPath string `toml:"project_path"`
}

// Protectedは保護領域の定義。検知方法のどれか1つに当たれば、その保護領域に
// 触れたとみなす。
type Protected struct {
	// Nameは計画書の `**保護領域**:` の行で宣言に使う名前。
	Name string `toml:"name"`
	// Labelは作業報告書や停止理由に表示する名前。
	Label string `toml:"label"`
	// Pathsは差分が触れたら保護領域に触れたとみなすファイルのglob。
	Paths []string `toml:"paths"`
	// Patternsは追加・削除行に含まれていたら保護領域に触れたとみなす正規表現。
	Patterns []string `toml:"patterns"`
	// GoFuncsCallingは、変更された行を含むGoの関数がこれらを呼び出していたら
	// 保護領域に触れたとみなす呼び出しの名前。gate.test_globsに当たるファイルは対象にしない。
	GoFuncsCalling []string `toml:"go_funcs_calling"`
	// GoImportPrefixは、この配下のパッケージ間に新しい依存が増えたら
	// 保護領域に触れたとみなすimportパスのプレフィックス。
	GoImportPrefix string `toml:"go_import_prefix"`
	// OnUndeclaredは、タスクで宣言していないのにこの保護領域に触れたときの扱い
	// (OnUndeclaredStop / OnUndeclaredReport)。
	OnUndeclared string `toml:"on_undeclared"`
	// ReviewPointsは、宣言どおりに触れた変更をレビュアーが「合意済みの設計制約」と
	// 照合する観点。省略すると、レビュアーは合意済みの設計制約との照合だけを行う。
	ReviewPoints []string `toml:"review_points"`
}

// Loadはfilenameの設定ファイルを読み込み、既定値を補って検証する。
func Load(filename string) (*Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(filename, &cfg)
	if err != nil {
		return nil, fmt.Errorf("%s を読み込めません: %w", filename, err)
	}
	if err := decode(md, &cfg); err != nil {
		return nil, fmt.Errorf("%s の設定が不正です:\n%w", filename, err)
	}
	return &cfg, nil
}

// Parseはsrcを設定ファイルの内容として読み込み、既定値を補って検証する。
func Parse(src string) (*Config, error) {
	var cfg Config
	md, err := toml.Decode(src, &cfg)
	if err != nil {
		return nil, err
	}
	if err := decode(md, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// decodeは読み込んだ値に既定値を補い、未知のキーと値の誤りをまとめて返す。
func decode(md toml.MetaData, cfg *Config) error {
	var errs []error
	// 綴りを誤ったキーは黙って既定値になるため、未知のキーとして止める。
	for _, key := range md.Undecoded() {
		errs = append(errs, fmt.Errorf("未知のキー %q", key.String()))
	}
	cfg.applyDefaults(md)
	errs = append(errs, cfg.validate()...)
	return errors.Join(errs...)
}

// applyDefaultsは省略された項目に既定値を入れる。0を明示した値と省略を
// 区別するため、キーの有無で判定する。
func (c *Config) applyDefaults(md toml.MetaData) {
	if !md.IsDefined("max_reviews") {
		c.MaxReviews = defaultMaxReviews
	}
	if !md.IsDefined("usage", "five_hour_cap") {
		c.Usage.FiveHourCap = defaultFiveHourCap
	}
	if !md.IsDefined("usage", "weekly_cap") {
		c.Usage.WeeklyCap = defaultWeeklyCap
	}
	if !md.IsDefined("usage", "safety_factor") {
		c.Usage.SafetyFactor = defaultSafetyFactor
	}
	if !md.IsDefined("usage", "five_hour_estimate") {
		c.Usage.FiveHourEstimate = defaultFiveHourEstimate
	}
	if !md.IsDefined("usage", "weekly_estimate") {
		c.Usage.WeeklyEstimate = defaultWeeklyEstimate
	}
	if !md.IsDefined("gate", "size_factor") {
		c.Gate.SizeFactor = defaultSizeFactor
	}
	if !md.IsDefined("report", "addr") {
		c.Report.Addr = defaultReportAddr
	}
	for i := range c.Protected {
		if c.Protected[i].OnUndeclared == "" {
			c.Protected[i].OnUndeclared = OnUndeclaredStop
		}
	}
}

// validateは値の誤りを返す。
func (c *Config) validate() []error {
	var errs []error
	addf := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	switch {
	case c.Project == "":
		addf("project を指定してください")
	case !reProtectedName.MatchString(c.Project):
		// ハブを置くディレクトリの名前に使うため、パスの区切りや空白を含めない。
		addf("project %q には英小文字・数字・ハイフンだけを使ってください", c.Project)
	}
	if c.BaseBranch == "" {
		addf("base_branch を指定してください")
	}
	if c.MaxReviews < 1 {
		addf("max_reviews は1以上にしてください (値: %d)", c.MaxReviews)
	}
	for _, limit := range []struct {
		key string
		v   float64
	}{
		{"usage.five_hour_cap", c.Usage.FiveHourCap},
		{"usage.weekly_cap", c.Usage.WeeklyCap},
	} {
		if math.IsNaN(limit.v) || math.IsInf(limit.v, 0) {
			addf("%s は有限の値にしてください (値: %g)", limit.key, limit.v)
		} else if limit.v <= 0 || limit.v > 100 {
			addf("%s は0より大きく100以下にしてください (値: %g)", limit.key, limit.v)
		}
	}
	for _, estimate := range []struct {
		key string
		v   float64
	}{
		{"usage.five_hour_estimate", c.Usage.FiveHourEstimate},
		{"usage.weekly_estimate", c.Usage.WeeklyEstimate},
	} {
		if math.IsNaN(estimate.v) || math.IsInf(estimate.v, 0) {
			addf("%s は有限の値にしてください (値: %g)", estimate.key, estimate.v)
		} else if estimate.v < 0 || estimate.v > 100 {
			addf("%s は0以上100以下にしてください (値: %g)", estimate.key, estimate.v)
		}
	}
	if math.IsNaN(c.Usage.SafetyFactor) || math.IsInf(c.Usage.SafetyFactor, 0) {
		addf("usage.safety_factor は有限の値にしてください (値: %g)", c.Usage.SafetyFactor)
	} else if c.Usage.SafetyFactor < 1 {
		addf("usage.safety_factor は1以上にしてください (値: %g)", c.Usage.SafetyFactor)
	}

	errs = append(errs, c.validateRoles()...)
	errs = append(errs, validateHub(c.Hub)...)
	if err := ValidateAddr(c.Report.Addr); err != nil {
		addf("report.addr: %v", err)
	}

	// 報告の順序を安定させるため、プラットフォーム名の順に見る。
	for _, platform := range slices.Sorted(maps.Keys(c.Gate.Commands)) {
		for i, cmd := range c.Gate.Commands[platform] {
			if strings.TrimSpace(cmd) == "" {
				addf("gate.commands.%s[%d] が空です", platform, i)
			}
		}
	}
	// コマンドはエージェントが書き換えられるコード (Makefile・テストなど) を実行するため、
	// koryluspilotを動かすVM上で直接実行させず、ゲートのコンテナを必ず通す。
	if len(c.Gate.Commands) > 0 && len(c.Gate.Runner) == 0 {
		addf("gate.commands を使うときは gate.runner を指定してください")
	}
	for i, arg := range c.Gate.Runner {
		if strings.TrimSpace(arg) == "" {
			addf("gate.runner[%d] が空です", i)
		}
	}
	errs = append(errs, validateRegexps("gate.suppress_patterns", c.Gate.SuppressPatterns)...)
	errs = append(errs, validateGlobs("gate.test_globs", c.Gate.TestGlobs)...)
	errs = append(errs, validateGlobs("gate.generated_globs", c.Gate.GeneratedGlobs)...)
	if math.IsNaN(c.Gate.SizeFactor) || math.IsInf(c.Gate.SizeFactor, 0) {
		addf("gate.size_factor は有限の値にしてください (値: %g)", c.Gate.SizeFactor)
	} else if c.Gate.SizeFactor < 1 {
		addf("gate.size_factor は1以上にしてください (値: %g)", c.Gate.SizeFactor)
	}
	errs = append(errs, validateWatch(c.Gate.Watch)...)

	seen := map[string]bool{}
	for i, p := range c.Protected {
		key := fmt.Sprintf("protected[%d]", i)
		switch {
		case p.Name == "":
			addf("%s.name を指定してください", key)
		case !reProtectedName.MatchString(p.Name):
			addf("%s.name %q には英小文字・数字・ハイフンだけを使ってください", key, p.Name)
		case seen[p.Name]:
			addf("%s.name %q が重複しています", key, p.Name)
		}
		seen[p.Name] = true
		if p.Label == "" {
			addf("%s.label を指定してください", key)
		}
		if len(p.Paths) == 0 && len(p.Patterns) == 0 && len(p.GoFuncsCalling) == 0 && p.GoImportPrefix == "" {
			addf("%s (%s) に検知方法 (paths / patterns / go_funcs_calling / go_import_prefix) を1つ以上指定してください", key, p.Name)
		}
		if p.OnUndeclared != OnUndeclaredStop && p.OnUndeclared != OnUndeclaredReport {
			addf("%s.on_undeclared に %q は指定できません (指定できる値: [%s %s])", key, p.OnUndeclared, OnUndeclaredStop, OnUndeclaredReport)
		}
		errs = append(errs, validateGlobs(key+".paths", p.Paths)...)
		errs = append(errs, validateRegexps(key+".patterns", p.Patterns)...)
		for j, call := range p.GoFuncsCalling {
			switch {
			case strings.TrimSpace(call) == "":
				addf("%s.go_funcs_calling[%d] が空です", key, j)
			case !token.IsIdentifier(call):
				// 関数の検知は呼び出す名前 (`db.BeginTx` の `BeginTx`) で照合する。
				addf("%s.go_funcs_calling[%d] %q には呼び出す関数・メソッドの名前だけを書いてください", key, j, call)
			}
		}
		for j, point := range p.ReviewPoints {
			if strings.TrimSpace(point) == "" {
				addf("%s.review_points[%d] が空です", key, j)
			}
		}
	}
	return errs
}

// validateRolesはrolesとreviewersの値の誤りを返す。
func (c *Config) validateRoles() []error {
	var errs []error
	addf := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	for _, name := range []string{RoleDesigner, RoleImplementer} {
		if _, ok := c.Roles[name]; !ok {
			addf("roles.%s を指定してください", name)
		}
	}
	// 報告の順序を安定させるため、役割の名前の順に見る。
	for _, name := range slices.Sorted(maps.Keys(c.Roles)) {
		r := c.Roles[name]
		key := "roles." + name
		if !reRoleName.MatchString(name) {
			addf("%s の名前には英小文字・数字・アンダースコアだけを使ってください", key)
		}
		var efforts []string
		switch r.Tool {
		case ToolClaude:
			efforts = claudeEfforts
		case ToolCodex:
			efforts = codexEfforts
		case "":
			addf("%s.tool を指定してください", key)
		default:
			addf("%s.tool に %q は指定できません (指定できる値: [%s %s])", key, r.Tool, ToolClaude, ToolCodex)
		}
		for _, e := range []struct{ key, v string }{
			{"effort", r.Effort},
			{"fix_effort", r.FixEffort},
		} {
			if e.v != "" && efforts != nil && !slices.Contains(efforts, e.v) {
				addf("%s.%s に %q は指定できません (指定できる値: %v)", key, e.key, e.v, efforts)
			}
		}
		switch {
		case r.Sandbox == "":
		case r.Tool != ToolCodex:
			addf("%s.sandbox は指定できません (Codexだけの設定です)", key)
		case !slices.Contains(codexSandboxes, r.Sandbox):
			addf("%s.sandbox に %q は指定できません (指定できる値: %v)", key, r.Sandbox, codexSandboxes)
		}
		// エージェントは秘密情報を渡さないコンテナで動かす前提で、ブランチの作成や
		// コミットを自由に行わせる。設定の誤りでVM上で直接動かさないよう、ランナーを必須にする。
		if len(r.Runner) == 0 {
			addf("%s.runner を指定してください", key)
		}
		for i, arg := range r.Runner {
			if strings.TrimSpace(arg) == "" {
				addf("%s.runner[%d] が空です", key, i)
			}
		}
	}

	if len(c.Reviewers) == 0 {
		addf("reviewers を指定してください")
	}
	seen := map[string]bool{}
	for i, name := range c.Reviewers {
		_, ok := c.Roles[name]
		switch {
		case !ok:
			addf("reviewers[%d] の役割 %q が roles にありません", i, name)
		case name == RoleDesigner || name == RoleImplementer:
			// 実装者が自分の変更をレビューしないよう、レビュー専用の役割に限る。
			addf("reviewers[%d] に %q は指定できません (レビュー専用の役割を指定してください)", i, name)
		case seen[name]:
			addf("reviewers[%d] の役割 %q が重複しています", i, name)
		}
		seen[name] = true
	}
	// 修正した役割とは別の役割で再レビューするため、2回以上レビューするなら2つ以上要る。
	if c.MaxReviews > 1 && len(c.Reviewers) == 1 {
		addf("max_reviews が2以上のときは、reviewers に2つ以上の役割を指定してください")
	}
	return errs
}

// validateHubはhubの値の誤りを返す。
func validateHub(h Hub) []error {
	var errs []error
	// sshなど認証を使う経路で取り込まないよう、httpsに限る。
	if !strings.HasPrefix(h.Upstream, "https://") {
		errs = append(errs, fmt.Errorf("hub.upstream %q には認証なしで読めるhttpsのURLを指定してください", h.Upstream))
	}
	if !path.IsAbs(h.Path) || path.Clean(h.Path) != h.Path {
		errs = append(errs, fmt.Errorf("hub.path %q は正規化した絶対パスにしてください", h.Path))
	}
	return errs
}

// ValidateAddrは、作業報告書を配信するHTTPサーバーの待ち受けアドレスaddrの誤りを返す。
// ホストを省くとすべてのインターフェースで、ポートを0にすると空いているポートで待ち受ける。
func ValidateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q は <ホスト>:<ポート> の形で指定してください", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%q のポートは0から65535の数で指定してください", addr)
	}
	return nil
}

// validateWatchはgate.watchの値の誤りを返す。
func validateWatch(watches []Watch) []error {
	var errs []error
	addf := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	seen := map[string]bool{}
	for i, w := range watches {
		key := fmt.Sprintf("gate.watch[%d]", i)
		switch {
		case w.Name == "":
			addf("%s.name を指定してください", key)
		case !reProtectedName.MatchString(w.Name):
			addf("%s.name %q には英小文字・数字・ハイフンだけを使ってください", key, w.Name)
		case seen[w.Name]:
			addf("%s.name %q が重複しています", key, w.Name)
		}
		seen[w.Name] = true
		// 比べるときにPathからの相対パスを取り出すため、正規化した絶対パスに限る。
		if !path.IsAbs(w.Path) || path.Clean(w.Path) != w.Path {
			addf("%s.path %q は正規化した絶対パスにしてください", key, w.Path)
		}
		for j, arg := range w.Runner {
			if strings.TrimSpace(arg) == "" {
				addf("%s.runner[%d] が空です", key, j)
			}
		}
		errs = append(errs, validateGlobs(key+".exclude", w.Exclude)...)
		if p := w.ProjectPath; p != "" && (path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../")) {
			addf("%s.project_path %q はプロジェクトの中を指す、正規化した相対パスにしてください", key, p)
		}
	}
	return errs
}

// validateRegexpsはpatternsが正規表現として解釈できるかを確かめる。
func validateRegexps(key string, patterns []string) []error {
	var errs []error
	for i, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, fmt.Errorf("%s[%d] の正規表現が不正です: %w", key, i, err))
		}
	}
	return errs
}

// validateGlobsはglobsがglobとして解釈できるかを確かめる。`**` は
// path.Matchの構文の範囲で書けるため、構文の誤りだけをここで拾う
// (`**` の照合はgateパッケージが行う)。
func validateGlobs(key string, globs []string) []error {
	var errs []error
	for i, g := range globs {
		if g == "" {
			errs = append(errs, fmt.Errorf("%s[%d] が空です", key, i))
			continue
		}
		if _, err := path.Match(g, ""); err != nil {
			errs = append(errs, fmt.Errorf("%s[%d] のglob %q が不正です: %w", key, i, g, err))
		}
	}
	return errs
}
