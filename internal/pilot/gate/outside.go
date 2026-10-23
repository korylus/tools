package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/runner"
)

// KindOutsideはリポジトリの外の変更。
const KindOutside Kind = "outside"

// DefaultSnapshotTimeoutはOutsideWatcher.Timeoutを省略したときの、監視するパス
// 1件を読むタイムアウト。
const DefaultSnapshotTimeout = 10 * time.Minute

// outsideShownは停止理由に載せる、変わったパスの件数の上限。
const outsideShown = 20

// snapshotScriptは監視するパス ($1) の下のすべてのエントリーの種類・パーミッション・
// シンボリックリンクの参照先と、ファイルの内容のハッシュをNUL区切りで書き出す
// シェルスクリプト。シンボリックリンクはたどらず、ファイルの内容だけを読む。
// $2以降はfind -pathのパターンで、当たったエントリーとその下を読まずに飛ばす。
// 監視するパスはエージェントが書き換えられるため、そこにある設定で動きが変わる
// コマンド (gitなど) は使わない。find -printfとsha256sum -zはGNUの拡張を使う。
const snapshotScript = `set -eu
root=$1
shift
if [ ! -e "$root" ] && [ ! -L "$root" ]; then
  echo "監視するパス $root がありません" >&2
  exit 3
fi
for p do
  set -- "$@" -o -path "$p"
  shift
done
if [ $# -gt 0 ]; then
  shift
  find "$root" \( "$@" \) -prune -o -printf 'meta\0%p\0%y %m %l\0'
  printf 'hashes\0'
  find "$root" \( "$@" \) -prune -o -type f -exec sha256sum -z -- {} +
else
  find "$root" -printf 'meta\0%p\0%y %m %l\0'
  printf 'hashes\0'
  find "$root" -type f -exec sha256sum -z -- {} +
fi
printf 'end\0'
`

// OutsideSnapshotは監視するパス1件の、ある時点の状態。ステップの前に取った
// ものを状態ファイルに残せるよう、JSONにできる形にする。
type OutsideSnapshot struct {
	Watch string `json:"watch"`
	// Entriesは監視するパスからの相対パス (監視するパス自身は空文字) ごとの、
	// 種類・パーミッション・参照先・内容のハッシュ。比較から除くパスは含めない。
	Entries map[string]string `json:"entries"`
}

// OutsideWatcherは設定のgate.watchのパスを読み、ステップの前後で比べる。
type OutsideWatcher struct {
	Watches []config.Watch
	// Timeoutは0ならDefaultSnapshotTimeoutを使う。
	Timeout time.Duration
}

// Snapshotはすべての監視するパスの現在の状態を読む。読めないパスがあれば
// エラーにする (監視するパスが消えたときも含む)。
func (w OutsideWatcher) Snapshot(ctx context.Context) ([]OutsideSnapshot, error) {
	res := make([]OutsideSnapshot, 0, len(w.Watches))
	for _, watch := range w.Watches {
		out, err := w.read(ctx, watch)
		if err != nil {
			return nil, err
		}
		entries, err := parseSnapshot(out, watch)
		if err != nil {
			return nil, fmt.Errorf("監視するパス %s (%s) の状態を読み取れません: %w", watch.Name, watch.Path, err)
		}
		res = append(res, OutsideSnapshot{Watch: watch.Name, Entries: entries})
	}
	return res, nil
}

func (w OutsideWatcher) read(ctx context.Context, watch config.Watch) ([]byte, error) {
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = DefaultSnapshotTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append([]string{"-c", snapshotScript, "koryluspilot-watch", watch.Path}, prunePatterns(watch)...)
	cmd := runner.Runner{Prefix: watch.Runner}.Command(ctx, "sh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%s でタイムアウトしました", timeout)
		}
		return nil, fmt.Errorf("監視するパス %s (%s) を読めません: %w: %s", watch.Name, watch.Path, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// prunePatternsは、watchのExcludeのうち「メタ文字を含まない前半/**」の形のものを、
// 読み取りの段階で飛ばすfind -pathのパターンにする。容量の大きいディレクトリや
// 読めないファイルを含むディレクトリを除外したときに、その中を読まないためである。
// それ以外の形のglobは、parseSnapshotで結果から落とす。
func prunePatterns(watch config.Watch) []string {
	root := escapeFindPattern(strings.TrimSuffix(watch.Path, "/"))
	var patterns []string
	for _, g := range watch.Exclude {
		prefix, ok := strings.CutSuffix(g, "/**")
		if !ok || prefix == "" || strings.ContainsAny(prefix, findPatternMeta) {
			continue
		}
		patterns = append(patterns, root+"/"+prefix)
	}
	return patterns
}

// findPatternMetaはfind -pathのパターンで特別な意味を持つ文字。
const findPatternMeta = `*?[\`

// escapeFindPatternは、find -pathのパターンでsをそのままの文字列として照合させる。
func escapeFindPattern(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(findPatternMeta, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// parseSnapshotはsnapshotScriptの出力を、監視するパスからの相対パスごとの
// 状態にする。watchのExcludeに当たるパスは含めない。
func parseSnapshot(out []byte, watch config.Watch) (map[string]string, error) {
	fields := strings.Split(string(out), "\x00")
	if len(fields) == 0 || fields[len(fields)-1] != "" {
		return nil, errors.New("出力がNULで終わっていません")
	}
	fields = fields[:len(fields)-1]

	entries := map[string]string{}
	set := func(p, v string) error {
		rel, ok := relPath(watch.Path, p)
		if !ok {
			return fmt.Errorf("監視するパスの外のパス %q が含まれています", p)
		}
		if !matchAny(watch.Exclude, rel) {
			entries[rel] += v
		}
		return nil
	}

	i := 0
	for i < len(fields) && fields[i] == "meta" {
		if i+2 >= len(fields) {
			return nil, errors.New("エントリーの記録が途中で切れています")
		}
		if err := set(fields[i+1], fields[i+2]); err != nil {
			return nil, err
		}
		i += 3
	}
	if i >= len(fields) || fields[i] != "hashes" {
		return nil, errors.New("内容のハッシュの記録がありません")
	}
	for i++; i < len(fields) && fields[i] != "end"; i++ {
		// sha256sumの出力は「ハッシュ + 空白2つ + パス」の形。
		hash, p, ok := strings.Cut(fields[i], "  ")
		if !ok {
			return nil, fmt.Errorf("内容のハッシュの記録 %q を読めません", fields[i])
		}
		if err := set(p, " "+hash); err != nil {
			return nil, err
		}
	}
	if i != len(fields)-1 {
		return nil, errors.New("内容のハッシュの記録が終わっていません")
	}
	return entries, nil
}

// relPathはpがrootかその下のパスなら、rootからの相対パスを返す。
func relPath(root, p string) (string, bool) {
	if p == root {
		return "", true
	}
	rel, ok := strings.CutPrefix(p, strings.TrimSuffix(root, "/")+"/")
	return rel, ok
}

// Checkはステップの前後の状態を比べ、変わったパスを監視するパスごとに1件の検知にする。
// allowedは、koryluspilotがそのステップでエージェントに書かせるパスのglob
// (プロジェクトからの相対パス。例: docs/private/plans/1_doing/foo.md) で、
// project_pathを設定した監視するパスでだけ比較から除く。
// 許可されたパスは、通常のファイルの変更・追加と削除だけを除く。
// 許可されたファイルの作成に必要な親ディレクトリの新規追加も除く。
func (w OutsideWatcher) Check(before, after []OutsideSnapshot, allowed []string) ([]Finding, error) {
	var findings []Finding
	for _, watch := range w.Watches {
		b, ok1 := findSnapshot(before, watch.Name)
		a, ok2 := findSnapshot(after, watch.Name)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("監視するパス %s の、ステップの前後の状態がそろっていません", watch.Name)
		}

		// 許可された書き込みに必要な親だけを許容し、兄弟のファイルや
		// 既存ディレクトリの権限・種類の変更は引き続き検知する。
		allowedParents := map[string]bool{}
		if watch.ProjectPath != "" {
			for rel := range a.Entries {
				if !strings.HasPrefix(a.Entries[rel], "f ") || !matchAny(allowed, path.Join(watch.ProjectPath, rel)) {
					continue
				}
				for parent := path.Dir(rel); parent != "." && parent != "/"; parent = path.Dir(parent) {
					allowedParents[parent] = true
				}
			}
		}

		keys := map[string]string{}
		maps.Copy(keys, b.Entries)
		maps.Copy(keys, a.Entries)
		var changes []string
		for _, rel := range slices.Sorted(maps.Keys(keys)) {
			bv, inBefore := b.Entries[rel]
			av, inAfter := a.Entries[rel]
			if bv == av && inBefore == inAfter {
				continue
			}
			// 許可されたパスでも、通常のファイル以外 (シンボリックリンクなど) への
			// 置き換えは検知する。koryluspilotがVM上で読み書きするときに、リンク先の
			// VMのファイルを扱わないためである。
			if watch.ProjectPath != "" && (!inAfter || strings.HasPrefix(av, "f ")) && matchAny(allowed, path.Join(watch.ProjectPath, rel)) {
				continue
			}
			if !inBefore && strings.HasPrefix(av, "d ") && allowedParents[rel] {
				continue
			}
			kind := "変更"
			switch {
			case !inBefore:
				kind = "追加"
			case !inAfter:
				kind = "削除"
			}
			changes = append(changes, fmt.Sprintf("%s (%s)", path.Join(watch.Path, rel), kind))
		}
		if len(changes) == 0 {
			continue
		}
		if len(changes) > outsideShown {
			changes = append(changes[:outsideShown], fmt.Sprintf("ほか%d件", len(changes)-outsideShown))
		}
		findings = append(findings, Finding{
			Kind:    KindOutside,
			Message: fmt.Sprintf("リポジトリの外の変更: 監視するパス %s で次のパスが変わっています: %s", watch.Name, strings.Join(changes, ", ")),
		})
	}
	return findings, nil
}

func findSnapshot(snapshots []OutsideSnapshot, name string) (OutsideSnapshot, bool) {
	for _, s := range snapshots {
		if s.Watch == name {
			return s, true
		}
	}
	return OutsideSnapshot{}, false
}
