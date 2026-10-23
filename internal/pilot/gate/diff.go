package gate

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Statusはファイルの変更の種類 (git diff --name-statusの1文字目)。
type Status byte

const (
	Added       Status = 'A'
	Modified    Status = 'M'
	Deleted     Status = 'D'
	Renamed     Status = 'R'
	Copied      Status = 'C'
	TypeChanged Status = 'T'
)

// reHunkはハンクの見出しにマッチし、変更前と変更後の開始行を取り出す。
var reHunk = regexp.MustCompile(`^@@ -([0-9]+)(?:,[0-9]+)? \+([0-9]+)(?:,[0-9]+)? @@`)

// Diffは2つのコミットの間の差分。
type Diff struct {
	Files []File
	// GoSourcesReadはReadGoSourcesでGoのファイルの内容を読んだかどうか。
	// go_funcs_callingの検知は、読んでいない差分では行えない。
	GoSourcesRead bool
}

// Fileは差分の中の1ファイル。
type File struct {
	// Pathは変更後のパス。削除されたファイルは変更前のパス。
	Path string
	// OldPathは変更前のパス。追加されたファイルは空文字。
	OldPath string
	Status  Status
	// BinaryはGitがバイナリと判定したファイルかどうか。行は検査のために読むが、
	// 画像などの資産を実装の行数に数えないよう、行数の集計からは除く。
	Binary bool
	// Addedは追加された行。行番号は変更後のファイルでの番号。
	Added []Line
	// Removedは削除された行。行番号は変更前のファイルでの番号。
	Removed []Line
	// OldSourceとNewSourceは変更前と変更後のファイルの内容。ReadGoSourcesが
	// Goのファイルにだけ載せる。
	OldSource []byte
	NewSource []byte
}

// Lineは差分の1行。
type Line struct {
	No   int
	Text string
}

// Pathsは変更前と変更後のパスを重複なく返す。名前の変更では両方を返す。
func (f File) Paths() []string {
	if f.OldPath == "" || f.OldPath == f.Path {
		return []string{f.Path}
	}
	return []string{f.OldPath, f.Path}
}

// diffArgsは差分を決定的に読むためのgit diffの引数。リポジトリの属性で指定された
// 外部の差分ドライバーやtextconvを動かさず、利用者の設定に左右されないよう
// 名前の変更の検出を明示する。
var diffArgs = []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "-M"}

// ReadDiffは、gitDirのリポジトリ (ハブ) で、コミットbaseからheadまでの差分を読む。
func ReadDiff(ctx context.Context, gitDir, base, head string) (*Diff, error) {
	nameStatus, err := git(ctx, gitDir, slices.Concat(diffArgs, []string{"--name-status", "-z", "--end-of-options", base, head, "--"})...)
	if err != nil {
		return nil, err
	}
	// NULを含む実行可能ファイルも行検査から外さないよう、パッチはバイナリと判定される
	// 内容もテキストとして読む。バイナリかどうかは--textを付けないnumstatから取る。
	patch, err := git(ctx, gitDir, slices.Concat(diffArgs, []string{"--text", "--unified=0", "--src-prefix=a/", "--dst-prefix=b/", "--end-of-options", base, head, "--"})...)
	if err != nil {
		return nil, err
	}
	numstat, err := git(ctx, gitDir, slices.Concat(diffArgs, []string{"--numstat", "-z", "--end-of-options", base, head, "--"})...)
	if err != nil {
		return nil, err
	}

	files, err := parseNameStatus(nameStatus)
	if err != nil {
		return nil, err
	}
	binary, err := parseNumstatBinary(numstat)
	if err != nil {
		return nil, err
	}
	if len(binary) != len(files) {
		return nil, fmt.Errorf("差分のファイルの一覧とnumstatが対応しません (一覧: %d件、numstat: %d件)", len(files), len(binary))
	}
	for i := range files {
		files[i].Binary = binary[i]
	}
	sections := splitPatch(patch)
	// ファイルの一覧とパッチは同じ引数で作るため、同じ順に並ぶ。パスはパッチの
	// 見出しから読むと引用符の扱いが要るため、-zの一覧の側から取る。
	// 種類の変更 (ファイルからシンボリックリンクへなど) は、パッチでは削除と追加の
	// 2つに分かれる。
	next := 0
	for i := range files {
		n := 1
		if files[i].Status == TypeChanged {
			n = 2
		}
		if next+n > len(sections) {
			return nil, fmt.Errorf("差分のファイルの一覧とパッチが対応しません (%s)", files[i].Path)
		}
		for _, sec := range sections[next : next+n] {
			if err := parseSection(sec, &files[i]); err != nil {
				return nil, fmt.Errorf("%s の差分を読み取れません: %w", files[i].Path, err)
			}
		}
		next += n
	}
	if next != len(sections) {
		return nil, fmt.Errorf("差分のファイルの一覧とパッチが対応しません (一覧: %d件、パッチ: %d件)", len(files), len(sections))
	}
	return &Diff{Files: files}, nil
}

// gitはgitDirのリポジトリでgitを実行し、標準出力を返す。
func git(ctx context.Context, gitDir string, args ...string) (string, error) {
	args = append([]string{"--git-dir=" + gitDir, "-c", "core.quotePath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...) //#nosec G204 -- 引数はkoryluspilotが組み立てたもの
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s が失敗しました: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// parseNameStatusは `git diff --name-status -z` の出力を読む。
func parseNameStatus(out string) ([]File, error) {
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if out == "" {
		return nil, nil
	}
	var files []File
	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" {
			return nil, fmt.Errorf("差分のファイルの一覧を読み取れません: %q", out)
		}
		f := File{Status: Status(status[0])}
		switch f.Status {
		case Renamed, Copied:
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("差分のファイルの一覧が途中で切れています: %q", out)
			}
			f.OldPath, f.Path = fields[i+1], fields[i+2]
			i += 3
		default:
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("差分のファイルの一覧が途中で切れています: %q", out)
			}
			f.Path = fields[i+1]
			if f.Status != Added {
				f.OldPath = f.Path
			}
			i += 2
		}
		files = append(files, f)
	}
	return files, nil
}

// parseNumstatBinaryは `git diff --numstat -z` の出力を読み、ファイルごとに
// バイナリと判定されたか (追加・削除の行数が `-`) を一覧と同じ順に返す。
func parseNumstatBinary(out string) ([]bool, error) {
	if out == "" {
		return nil, nil
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var binary []bool
	for i := 0; i < len(fields); {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("差分のnumstatを読み取れません: %q", out)
		}
		binary = append(binary, parts[0] == "-" && parts[1] == "-")
		// 名前の変更はパスの欄が空で、続く2つの欄が変更前と変更後のパスになる。
		if parts[2] == "" {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("差分のnumstatが途中で切れています: %q", out)
			}
			i += 3
		} else {
			i++
		}
	}
	return binary, nil
}

// splitPatchはパッチをファイルごとの部分に分ける。
func splitPatch(patch string) [][]string {
	var sections [][]string
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			sections = append(sections, nil)
		}
		if len(sections) > 0 {
			sections[len(sections)-1] = append(sections[len(sections)-1], line)
		}
	}
	return sections
}

// parseSectionは1ファイル分のパッチから、追加・削除された行を読む。
func parseSection(lines []string, f *File) error {
	inHunk := false
	oldNo, newNo := 0, 0
	for _, line := range lines {
		if m := reHunk.FindStringSubmatch(line); m != nil {
			inHunk = true
			oldNo, _ = strconv.Atoi(m[1])
			newNo, _ = strconv.Atoi(m[2])
			continue
		}
		if !inHunk {
			continue
		}
		if line == "" {
			continue
		}
		switch line[0] {
		case '+':
			f.Added = append(f.Added, Line{No: newNo, Text: line[1:]})
			newNo++
		case '-':
			f.Removed = append(f.Removed, Line{No: oldNo, Text: line[1:]})
			oldNo++
		case ' ':
			oldNo++
			newNo++
		case '\\':
			// 「\ No newline at end of file」は行ではない。
		default:
			return fmt.Errorf("ハンクの中に読めない行があります: %q", line)
		}
	}
	return nil
}
