<!-- last_synced: 2026-09-14 -->

# korylus-tools

> 日本語 | [English](./README.en.md)

Korylusプロジェクトで共通利用する開発ツールを集約したリポジトリです。

## koryluslint

`koryluslint` はKorylus共通のリンタをまとめた単一バイナリです。
サブコマンドで各リンタを呼び分けます。

| サブコマンド | 役割                                                                                                          |
| ------------ | ------------------------------------------------------------------------------------------------------------- |
| `comment`    | ソースコードの日本語テキストスタイル (korylus-lang.md §3) を検査する                                          |
| `md`         | Markdownドキュメントの句点改行 (semantic line break) と日本語テキストスタイル (korylus-lang.md §3) を検査する |

### 使い方

```sh
koryluslint comment [-base=<ref>] [paths...]
koryluslint md [-base=<ref>] [--all] [--write] [paths...]
```

`-base=<ref>` を渡すと、`<ref>` 以降に追加された行だけを検査します (差分スコープ)。

`comment` が見る範囲は形式ごとに違います。

| 形式                      | 検査対象                                                                          |
| ------------------------- | --------------------------------------------------------------------------------- |
| `.go`                     | コメント                                                                          |
| `.templ`                  | 行頭 `//` のコメント                                                              |
| `.sh`                     | Bashの構文解析で抽出したコメント・引数などの文字列・ヒアドキュメントの本文        |
| `.ts`                     | TypeScriptの構文解析で抽出したコメント・文字列の本文 (テンプレートリテラルを含む) |
| `.sql` / `.css` / `.toml` | 日本語を含む行                                                                    |

コメント以外の日本語も検査するのは、i18nの訳文やシェルが出力するメッセージも§3の対象だからです。
シェルのコマンド名・オプション・展開式や、TypeScriptの補間式の演算子などは本文に含めません。
引用符で囲まないシェルの引数どうしの区切りは、出力される文面のスペースとして検査します。
`.sh` / `.ts` では、コメント内のMarkdownインラインコードは除外しますが、文字列内のバッククォートは本文として検査します。
`.sql` / `.css` / `.toml` は行全体をMarkdownとして扱うため、バッククォートで囲まれた範囲は文字列の中でも除外されます。
シェル・TypeScriptの構文解析に失敗したファイルは、標準エラー出力に通知してスキップします。

日本語を含まない行は検査しません。
生成物の除外は`_templ.go`と、先頭20行以内に`// Code generated ... DO NOT EDIT.`の定型ヘッダーを持つファイルが対象です。
それ以外の生成物や、SQL・CSS・TOMLのコード中に日本語がある場合は、行単位の検査に含まれます。

`.go` の文字列リテラルは検査しません。
描画結果と突き合わせるテストのリテラルは、テンプレートの出力に合わせて §3に反する形を意図して持つためです。

`md` では、`--all` は差分行だけでなく全 `.md` ファイルを全行検査し、`--write` は句点改行をその場で修正します。
日本語テキストスタイルの違反は書き換え後の行番号で報告し、違反が残る場合は終了コード1を返します。

## 取得・実行

ビルドには、Goに加えてCコンパイラ (GCCまたはClang) と`CGO_ENABLED=1`が必要です。
シェル・TypeScriptの構文解析に[Tree-sitterのGoバインディング](https://github.com/tree-sitter/go-tree-sitter)を使っています。

各プロジェクトはバージョンを固定して `koryluslint` を取得します。

- Goプロジェクト: `go.mod` のtoolディレクティブで `github.com/korylus/tools/cmd/koryluslint` を固定し、`go tool koryluslint ...` で実行する
- 非Goプロジェクト: miseの `go:` バックエンドで固定取得する

## 開発

```sh
make build       # koryluslintをビルド (bin/koryluslint)
make test        # テストを実行
make vet         # go vetを実行
make lint        # golangci-lintを実行
make fmt         # gofmt + goimports + Oxfmtでフォーマット
make fmt-check   # フォーマットチェック
```
