// Package sourceは、作業計画書の読み取り元 (Wikinoのページ) を扱う。
//
// 読み取り元の違いはこのパッケージに閉じ込め、呼び出し側は読んだ本文を
// plan.Parseに渡し、Nameで計画書名を決めるだけにする。
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/korylus/tools/internal/plan"
)

// wikinoHostは、計画書のURLとして受け付け、wikinoコマンドの接続先にするホスト。
// 受け付けたURLと別のホストのページを読まないよう、両方をこの値から作る。
const wikinoHost = "https://wikino.app"

// wikinoの終了コード。
const (
	wikinoExitAuth        = 3
	wikinoExitNotFound    = 5
	wikinoExitRateLimited = 8
)

// rePageURLはWikinoのページのURLにマッチし、スペースとページ番号を取り出す。
// スペースは `-` で始まる値をフラグとして読ませないよう、先頭にハイフンを許さない。
var rePageURL = regexp.MustCompile(`^` + regexp.QuoteMeta(wikinoHost) + `/s/([A-Za-z0-9_][A-Za-z0-9_-]*)/pages/([0-9]+)$`)

// Sourceは計画書の読み取り元。
type Source interface {
	// Readは計画書の本文を読む。
	Read(ctx context.Context) (string, error)
	// Nameは、読み取った計画書pの計画書名を返す。
	Name(p *plan.Plan) (string, error)
	// Stringは、コマンドで受け取ったときの計画書の指定 (URL) を返す。
	String() string
}

// Parseは、コマンドで受け取った計画書の指定argから読み取り元を返す。
// argはWikinoのページのURLである。ページ番号だけの指定はスペースを決められず、
// パスなどほかの指定は読み取り元が無いため、URLの形を示すエラーにする。
func Parse(arg string) (Source, error) {
	m := rePageURL.FindStringSubmatch(arg)
	if m == nil {
		return nil, fmt.Errorf("計画書の指定 %s を読み取れません。%s/s/<スペース>/pages/<ページ番号> の形のURLで指定してください", arg, wikinoHost)
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("計画書のURL %s のページ番号を読み取れません", arg)
	}
	return &Wikino{URL: arg, Space: m[1], Number: n, Run: execRunner("wikino")}, nil
}

// Runnerはwikinoコマンドを引数argsで実行し、標準出力・標準エラー・終了コードを返す。
// errは、コマンドを起動できなかったときと、終了コードで表せない失敗のときだけ返す。
type Runner func(ctx context.Context, args ...string) (stdout, stderr []byte, code int, err error)

// Wikinoは、WikinoのページのURLで指定した計画書。wikinoコマンドを子プロセスで呼んで読む。
type Wikino struct {
	URL    string
	Space  string
	Number int
	// Runはwikinoコマンドを実行する。テストでは差し替えて、実際のAPIを呼ばない。
	Run Runner
}

// Readは、下書きがあれば下書きを、無ければページを読み、frontmatterを除いた本文を返す。
// 実装者が付けたチェックや実装判断ログは、公開するまで下書きにしか無いためである。
func (w *Wikino) Read(ctx context.Context) (string, error) {
	for _, kind := range []string{"draft", "page"} {
		out, stderr, code, err := w.Run(ctx, kind, "get", strconv.Itoa(w.Number), "--host="+wikinoHost, "--space="+w.Space, "--md")
		if err != nil {
			return "", err
		}
		switch code {
		case 0:
			return stripFrontmatter(string(out))
		case wikinoExitNotFound:
			continue
		}
		return "", w.runError(kind, code, stderr)
	}
	return "", fmt.Errorf("計画書 %s が見つかりません。URLのスペースとページ番号を確かめてください", w.URL)
}

// runErrorは、wikinoコマンドが失敗したときのエラーを、標準エラーを添えて返す。
func (w *Wikino) runError(kind string, code int, stderr []byte) error {
	var reason string
	switch code {
	case wikinoExitAuth:
		reason = "ログインしていないか、資格情報が無効です"
	case wikinoExitRateLimited:
		reason = "公開APIの利用量の上限に当たりました"
	default:
		reason = fmt.Sprintf("wikino %s get が終了コード %d で失敗しました", kind, code)
	}
	msg := fmt.Sprintf("計画書 %s を読めません: %s。wikino auth status --host=%s --space=%s で状態を確かめてください", w.URL, reason, wikinoHost, w.Space)
	if s := strings.TrimSpace(string(stderr)); s != "" {
		msg += "\n" + s
	}
	return errors.New(msg)
}

// Nameは計画書名の行の値を返す。計画書名は状態の置き場とブランチ名に使うため、行を必須にする。
func (w *Wikino) Name(p *plan.Plan) (string, error) {
	if p.Name == "" {
		return "", fmt.Errorf("計画書 %s に計画書名の行がありません。最初の「## 」の見出しより前に「**計画書名**: <英小文字・数字・ハイフンの名前>」の行を書いてください", w.URL)
	}
	return p.Name, nil
}

func (w *Wikino) String() string {
	return w.URL
}

// stripFrontmatterは、wikinoの `--md` の出力からfrontmatterを除いた本文を返す。
func stripFrontmatter(s string) (string, error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return s, nil
	}
	_, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return "", errors.New("wikinoの出力のfrontmatterが閉じていません")
	}
	return strings.TrimPrefix(body, "\n"), nil
}

// execRunnerは、実行ファイルnameをwikinoコマンドとして子プロセスで呼ぶRunnerを返す。
// テストでは、出力と終了コードを決めた実行ファイルを渡す。
func execRunner(name string) Runner {
	return func(ctx context.Context, args ...string) ([]byte, []byte, int, error) {
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, name, args...) //#nosec G204 -- 検査したスペースとページ番号だけを、シェルを通さずに渡す
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			return stdout.Bytes(), stderr.Bytes(), 0, nil
		case errors.Is(err, exec.ErrNotFound):
			return nil, nil, 0, errors.New("wikinoコマンドが見つかりません。Wikino CLIをインストールしてログインしてください")
		case ctx.Err() != nil:
			return nil, nil, 0, ctx.Err()
		case errors.As(err, &exitErr):
			return stdout.Bytes(), stderr.Bytes(), exitErr.ExitCode(), nil
		}
		return nil, nil, 0, fmt.Errorf("wikinoコマンドを実行できません: %w", err)
	}
}
