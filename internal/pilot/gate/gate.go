// Package gateは、エージェントのステップのあとにハブへ取り込んだコミットに対して、
// エージェントとは独立した決定的な検査 (ゲート検査) を行う。
//
// 差分と計画書を読むだけの検査 (Inspect) は、koryluspilotがハブのコミットを
// 読んで行う。コードを実行する検査 (CommandRunner) は、ゲートのコンテナで、
// ハブから取り直したチェックアウトに対して行う。
package gate

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/plan"
)

// Kindは検査の種類。
type Kind string

const (
	// KindProtectedは宣言の無い保護領域への変更。
	KindProtected Kind = "protected"
	// KindSuppressは抑制パターンの追加。
	KindSuppress Kind = "suppress"
	// KindTestDeletionはテストファイル・テスト関数の削除。
	KindTestDeletion Kind = "test-deletion"
	// KindPlanは計画書の許可外の変更。
	KindPlan Kind = "plan"
	// KindSizeは想定行数の超過。
	KindSize Kind = "size"
)

// testPartは想定行数の内訳のうち、実装の行数に含めないテストの名前。
const testPart = "テスト"

// reGoTestFuncはGoのテスト関数 (go testが実行する関数) の宣言にマッチする。
var reGoTestFunc = regexp.MustCompile(`^func\s+((?:Test|Benchmark|Fuzz|Example)\w*)\s*\(`)

// Inputは差分と計画書を読む検査の入力。
type Input struct {
	Config *config.Config
	// Taskは検査するタスク。保護領域の宣言と想定行数は、人間が合意した基準である
	// 計画書のスナップショットから読んだものを渡す。
	Task plan.Task
	// Diffは差分。go_funcs_callingを使う保護領域があるときは、ReadGoSourcesで
	// Goのファイルの内容を読んでおく。
	Diff *Diff
	// GoDepsは差分の両端のパッケージの一覧。go_import_prefixを使う保護領域が
	// あるときは、CommandRunner.GoPackagesで両端の一覧を取って渡す。
	GoDeps *GoDeps
	// PlanSnapshotとPlanCurrentは計画書のスナップショットと現在の内容。
	// PlanSnapshotが空なら計画書の変更を検査しない。
	PlanSnapshot string
	PlanCurrent  string
}

// Reportは差分と計画書を読む検査の結果。
type Report struct {
	// Touchesは差分が触れた保護領域。宣言の有無にかかわらず、設定の順に並べる。
	Touches []Touch
	// Findingsは止める理由になる検知。空なら検査を通っている。
	Findings []Finding
	// ImplLinesは実装の行数 (テスト・生成物・バイナリと、削除されたファイルを除く追加・削除行の合計)。
	ImplLines int
}

// Passedは止める理由が無いかを返す。
func (r *Report) Passed() bool {
	return len(r.Findings) == 0
}

// Touchは差分が触れた保護領域1件。
type Touch struct {
	Name  string
	Label string
	// Declaredはタスクで宣言されていたかどうか。宣言されておらず、on_undeclaredが
	// reportの領域は、止めずに作業報告書に載せるためにここに残る。
	Declared bool
	// Hitsは保護領域に当たった箇所。
	Hits []Hit
}

// Hitは保護領域に当たった箇所1件。
type Hit struct {
	// Pathは当たったファイル。依存辺で当たったときは空文字。
	Path string
	// Lineは当たった行の行番号。パスで当たったときは0。
	Line int
	// Removedは削除された行で当たったかどうか (行番号は変更前のファイルでの番号)。
	Removed bool
	Text    string
	// Funcはgo_funcs_callingで当たった行を含む関数の名前。
	Func string
	// Edgeはgo_import_prefixで当たった、新しく増えた依存辺。
	Edge *Edge
}

// Findingは止める理由になる検知1件。
type Finding struct {
	Kind    Kind
	Message string
}

// Inspectは差分と計画書を読む検査を行う。
func Inspect(in Input) (*Report, error) {
	r := &Report{}
	if err := r.checkProtected(in); err != nil {
		return nil, err
	}
	if err := r.checkSuppress(in); err != nil {
		return nil, err
	}
	r.checkTestDeletion(in)
	if in.PlanSnapshot != "" {
		for _, v := range plan.CheckTaskChanges(in.PlanSnapshot, in.PlanCurrent, in.Task.ID) {
			msg := v.Message
			if v.LineNo > 0 {
				msg = fmt.Sprintf("%d行目: %s", v.LineNo, v.Message)
			}
			r.add(KindPlan, "計画書の許可外の変更: %s", msg)
		}
	}
	r.checkSize(in)
	return r, nil
}

func (r *Report) add(kind Kind, format string, args ...any) {
	r.Findings = append(r.Findings, Finding{Kind: kind, Message: fmt.Sprintf(format, args...)})
}

// checkProtectedは差分が触れた保護領域を、パス・追加・削除行のパターン・指定した
// 呼び出しを含むGoの関数・Goのパッケージ間の新しい依存辺で検知し、タスクの宣言と
// 照合する。
func (r *Report) checkProtected(in Input) error {
	declared := map[string]bool{}
	for _, d := range in.Task.Protected {
		declared[d.Name] = true
	}

	for _, p := range in.Config.Protected {
		patterns, err := compileAll(p.Patterns)
		if err != nil {
			return fmt.Errorf("保護領域 %s: %w", p.Name, err)
		}
		var hits []Hit
		for _, f := range in.Diff.Files {
			if slices.ContainsFunc(f.Paths(), func(name string) bool { return matchAny(p.Paths, name) }) {
				hits = append(hits, Hit{Path: f.Path})
			}
			for _, side := range []struct {
				lines   []Line
				removed bool
			}{{f.Added, false}, {f.Removed, true}} {
				for _, l := range side.lines {
					if matchesAny(patterns, l.Text) {
						hits = append(hits, Hit{Path: f.Path, Line: l.No, Removed: side.removed, Text: l.Text})
					}
				}
			}
		}
		if len(p.GoFuncsCalling) > 0 {
			if !in.Diff.GoSourcesRead {
				return fmt.Errorf("保護領域 %s: go_funcs_callingの検知には、差分のGoのファイルの内容が要ります", p.Name)
			}
			hits = append(hits, goFuncHits(in.Diff, p.GoFuncsCalling, in.Config.Gate.TestGlobs)...)
		}
		if p.GoImportPrefix != "" {
			if in.GoDeps == nil {
				return fmt.Errorf("保護領域 %s: go_import_prefixの検知には、差分の両端のパッケージの一覧が要ります", p.Name)
			}
			for _, e := range newEdges(in.GoDeps, p.GoImportPrefix) {
				hits = append(hits, Hit{Edge: &e})
			}
		}
		if len(hits) == 0 {
			continue
		}

		t := Touch{Name: p.Name, Label: p.Label, Declared: declared[p.Name], Hits: hits}
		r.Touches = append(r.Touches, t)
		if !t.Declared && p.OnUndeclared == config.OnUndeclaredStop {
			r.add(KindProtected, "タスク %s で宣言していない保護領域 %s (%s) に触れています: %s", in.Task.ID, p.Name, p.Label, hitSummary(hits))
		}
	}
	return nil
}

// checkSuppressは追加行に抑制パターンが含まれないかを確かめる。
func (r *Report) checkSuppress(in Input) error {
	patterns, err := compileAll(in.Config.Gate.SuppressPatterns)
	if err != nil {
		return fmt.Errorf("gate.suppress_patterns: %w", err)
	}
	for _, f := range in.Diff.Files {
		for _, l := range f.Added {
			for _, re := range patterns {
				if re.MatchString(l.Text) {
					r.add(KindSuppress, "%s:%d: 抑制のパターン %q を含む行が追加されています: %s", f.Path, l.No, re.String(), strings.TrimSpace(l.Text))
				}
			}
		}
	}
	return nil
}

// checkTestDeletionはテストファイルとGoのテスト関数の削除を検知する。テスト関数は、
// 同じディレクトリのテストファイルに同じ名前で追加されていれば、移動とみなして
// 削除に数えない。
func (r *Report) checkTestDeletion(in Input) {
	globs := in.Config.Gate.TestGlobs
	type funcKey struct{ dir, name string }
	removed := map[funcKey]string{} // テスト関数 → 削除されたファイル
	var order []funcKey
	added := map[funcKey]bool{}

	for _, f := range in.Diff.Files {
		oldIsTest := f.OldPath != "" && matchAny(globs, f.OldPath)
		switch {
		case oldIsTest && f.Status == Deleted:
			r.add(KindTestDeletion, "テストファイル %s が削除されています", f.OldPath)
			continue
		case oldIsTest && f.Status == Renamed && !matchAny(globs, f.Path):
			r.add(KindTestDeletion, "テストファイル %s がテストではないパス %s に移されています", f.OldPath, f.Path)
			continue
		case oldIsTest && f.Status == Renamed && strings.HasSuffix(f.OldPath, ".go") && path.Dir(f.OldPath) != path.Dir(f.Path):
			r.add(KindTestDeletion, "Goのテストファイル %s が別ディレクトリの %s に移されています", f.OldPath, f.Path)
			continue
		}

		if oldIsTest && strings.HasSuffix(f.OldPath, ".go") {
			for _, l := range f.Removed {
				if m := reGoTestFunc.FindStringSubmatch(l.Text); m != nil {
					k := funcKey{path.Dir(f.OldPath), m[1]}
					if _, ok := removed[k]; !ok {
						order = append(order, k)
					}
					removed[k] = f.OldPath
				}
			}
		}
		if f.Status != Deleted && matchAny(globs, f.Path) && strings.HasSuffix(f.Path, ".go") {
			for _, l := range f.Added {
				if m := reGoTestFunc.FindStringSubmatch(l.Text); m != nil {
					added[funcKey{path.Dir(f.Path), m[1]}] = true
				}
			}
		}
	}

	for _, k := range order {
		if !added[k] {
			r.add(KindTestDeletion, "テスト関数 %s が %s から削除されています", k.name, removed[k])
		}
	}
}

// checkSizeは実装の行数が、計画書の想定行数 (テストを除く) に設定の倍率を掛けた
// 値を超えないかを確かめる。想定行数の記載が無いタスクは検査しない。
func (r *Report) checkSize(in Input) {
	g := in.Config.Gate
	for _, f := range in.Diff.Files {
		if Classify(g, f) == FileImpl {
			r.ImplLines += len(f.Added) + len(f.Removed)
		}
	}

	if in.Task.Lines.Total <= 0 {
		return
	}
	expected := ExpectedImplLines(in.Task.Lines)
	limit := float64(expected) * g.SizeFactor
	if float64(r.ImplLines) > limit {
		r.add(KindSize, "実装の行数 %d行が、想定行数 (テストを除く) %d行の%g倍を超えています", r.ImplLines, expected, g.SizeFactor)
	}
}

// ExpectedImplLinesは想定行数sizeのうち、テストを除いた行数を返す。記載が無ければ0。
func ExpectedImplLines(size plan.Size) int {
	if size.Total <= 0 {
		return 0
	}
	return size.Total - size.Parts[testPart]
}

// FileKindは、実装の行数の集計でのファイルの扱い。
type FileKind string

const (
	FileImpl      FileKind = "impl"
	FileTest      FileKind = "test"
	FileGenerated FileKind = "generated"
	FileBinary    FileKind = "binary"
	FileDeleted   FileKind = "deleted"
)

// Classifyは差分のファイルfを、実装の行数に数えるか (FileImpl) と、数えない理由に分ける。
// 削除だけの変更はタスクの大きさに数えない。
func Classify(g config.Gate, f File) FileKind {
	switch {
	case f.Status == Deleted:
		return FileDeleted
	case f.Binary:
		return FileBinary
	case matchAny(g.TestGlobs, f.Path):
		return FileTest
	case matchAny(g.GeneratedGlobs, f.Path):
		return FileGenerated
	}
	return FileImpl
}

// compileAllはpatternsを正規表現にする。設定の読み込みで検証しているため、
// 失敗するのは検証を経ていない設定を渡したときに限る。
func compileAll(patterns []string) ([]*regexp.Regexp, error) {
	res := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		res = append(res, re)
	}
	return res, nil
}

func matchesAny(patterns []*regexp.Regexp, s string) bool {
	for _, re := range patterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// hitSummaryは停止理由に載せる、当たった箇所の短い一覧を返す。
func hitSummary(hits []Hit) string {
	const maxShown = 5
	var parts []string
	for i, h := range hits {
		if i == maxShown {
			parts = append(parts, fmt.Sprintf("ほか%d件", len(hits)-maxShown))
			break
		}
		switch {
		case h.Edge != nil:
			parts = append(parts, "依存 "+h.Edge.String())
		case h.Line == 0:
			parts = append(parts, h.Path)
		case h.Func != "":
			parts = append(parts, fmt.Sprintf("%s:%d (%s)", h.Path, h.Line, h.Func))
		default:
			parts = append(parts, fmt.Sprintf("%s:%d", h.Path, h.Line))
		}
	}
	return strings.Join(parts, ", ")
}
