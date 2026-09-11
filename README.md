<!-- last_synced: 2026-09-11 -->

# korylus-tools

> 日本語 | [English](./README.en.md)

Korylusプロジェクトで共通利用する開発ツールを集約したリポジトリです。

## koryluslint

`koryluslint` はKorylus共通のリンタをまとめた単一バイナリです。
サブコマンドで各リンタを呼び分けます。

| サブコマンド | 役割                                                                                                          |
| ------------ | ------------------------------------------------------------------------------------------------------------- |
| `comment`    | コードコメントの日本語テキストスタイル (korylus-lang.md §3) を検査する                                        |
| `md`         | Markdownドキュメントの句点改行 (semantic line break) と日本語テキストスタイル (korylus-lang.md §3) を検査する |

### 使い方

```sh
koryluslint comment [-base=<ref>] [paths...]
koryluslint md [-base=<ref>] [--all] [--write] [paths...]
```

`-base=<ref>` を渡すと、`<ref>` 以降に追加された行だけを検査します (差分スコープ)。

`md` では、`--all` は差分行だけでなく全 `.md` ファイルを全行検査し、`--write` は句点改行をその場で修正します。
日本語テキストスタイルの違反は書き換え後の行番号で報告し、違反が残る場合は終了コード1を返します。

## 取得・実行

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
