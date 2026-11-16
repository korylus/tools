.PHONY: help
help: ## ヘルプを表示
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## koryluslintをビルド (bin/koryluslintに出力)
	go build -o bin/koryluslint ./cmd/koryluslint

# BINDIRはinstall-korylusflowが実行ファイルを置くディレクトリ。
BINDIR ?= $(HOME)/.local/bin

.PHONY: install-korylusflow
install-korylusflow: ## korylusflowをビルドしてBINDIR (既定は~/.local/bin) に置く
	mkdir -p $(BINDIR)
	go build -o $(BINDIR)/korylusflow ./cmd/korylusflow

.PHONY: test
test: test-go test-rust ## テストを実行 (GoとRust)

.PHONY: test-go
test-go: ## Goのテストを実行
	go test ./...

.PHONY: test-rust
test-rust: ## Rustのテストを実行
	cargo test --workspace --locked

.PHONY: vet
vet: ## go vetを実行
	go vet ./...

.PHONY: lint
lint: lint-go lint-rust ## リントを実行 (GoとRust)

.PHONY: lint-go
lint-go: ## golangci-lintを実行
	golangci-lint run --config=.golangci.yml ./...

.PHONY: lint-rust
lint-rust: ## clippyを実行
	cargo clippy --workspace --all-targets --locked -- -D warnings

.PHONY: fmt
fmt: ## コード・ドキュメントをフォーマット (gofmt + goimports + rustfmt + Oxfmt)
	golangci-lint fmt --config=.golangci.yml
	cargo fmt --all
	pnpm fmt

.PHONY: fmt-check
fmt-check: fmt-check-go fmt-check-rust fmt-check-md ## フォーマットチェック (gofmt + rustfmt + Oxfmt)

.PHONY: fmt-check-go
fmt-check-go: ## Goのフォーマットチェック (gofmt)
	@diff=$$(gofmt -l .); if [ -n "$$diff" ]; then echo "gofmt差分あり:"; echo "$$diff"; exit 1; fi

.PHONY: fmt-check-rust
fmt-check-rust: ## Rustのフォーマットチェック (rustfmt)
	cargo fmt --all --check

.PHONY: fmt-check-md
fmt-check-md: ## ドキュメントのフォーマットチェック (Oxfmt)
	pnpm fmt:check
