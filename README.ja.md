<!-- last_synced: 2026-06-03 -->

# korylus-tools

> [English](./README.md) | 日本語

Korylus プロジェクトで共通利用する開発ツールを集約したリポジトリです。

## koryluslint

`koryluslint` は Korylus 共通のリンタをまとめた単一バイナリです。
サブコマンドで各リンタを呼び分けます。

| サブコマンド | 役割                                                                   |
| ------------ | ---------------------------------------------------------------------- |
| `comment`    | コードコメントの英日併記 (`[Ja]` マーカー) の誤用を検査する            |
| `md`         | Markdown ドキュメントの句点改行 (semantic line break) を検査・修正する |

### 使い方

```sh
koryluslint comment [-base=<ref>] [paths...]
koryluslint md [-base=<ref>] [--all] [--write] [paths...]
```

`-base=<ref>` を渡すと、`<ref>` 以降に追加された行だけを検査します (差分スコープ)。

`md` では、`--all` は差分行だけでなく全 `.md` ファイルを全行検査し、`--write` は指摘ではなく該当行をその場で書き換えます。

## 取得・実行

各プロジェクトはバージョンを固定して `koryluslint` を取得します。

- Go プロジェクト: `go.mod` の tool ディレクティブで `github.com/korylus/korylus-tools/cmd/koryluslint` を固定し、`go tool koryluslint ...` で実行する
- 非 Go プロジェクト: mise の `go:` バックエンドで固定取得する

## 開発

```sh
make build       # koryluslint をビルド (bin/koryluslint)
make test        # テストを実行
make vet         # go vet を実行
make lint        # golangci-lint を実行
make fmt         # gofmt + goimports + Oxfmt でフォーマット
make fmt-check   # フォーマットチェック
```
