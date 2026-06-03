.PHONY: help
help: ## ヘルプを表示
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## koryluslint をビルド (bin/koryluslint に出力)
	go build -o bin/koryluslint ./cmd/koryluslint

.PHONY: test
test: ## テストを実行
	go test ./...

.PHONY: vet
vet: ## go vet を実行
	go vet ./...

.PHONY: lint
lint: ## golangci-lint を実行
	golangci-lint run --config=.golangci.yml ./...

.PHONY: fmt
fmt: ## コード・ドキュメントをフォーマット (gofmt + goimports + Oxfmt)
	golangci-lint fmt --config=.golangci.yml
	pnpm fmt

.PHONY: fmt-check
fmt-check: ## フォーマットチェック (gofmt + Oxfmt)
	@diff=$$(gofmt -l .); if [ -n "$$diff" ]; then echo "gofmt 差分あり:"; echo "$$diff"; exit 1; fi
	pnpm fmt:check
