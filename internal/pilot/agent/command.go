package agent

import (
	"cmp"
	"fmt"
)

// claudePermissionModeは無人実行で使うClaude Codeの権限モード。
const claudePermissionMode = "auto"

// codexApprovalPolicyは無人実行で使うCodexの承認ポリシー。承認する人間が
// いないため、承認を求めずに進めさせる。
const codexApprovalPolicy = "never"

// schemaPathPlaceholderは、codexSchemaScriptがスキーマの一時ファイルのパスに
// 置き換える引数。
const schemaPathPlaceholder = "@koryluspilot-output-schema@"

// codexSchemaScriptは、ランナーの中でJSON Schema ($1) を一時ファイルに書き、
// 残りの引数のうちschemaPathPlaceholderをそのパスに置き換えてCodexを起動する
// シェルスクリプト。CodexはJSON Schemaをファイルのパスでだけ受け付け、
// koryluspilotが書いたファイルは役割のコンテナの中から見えないため、コンテナの中で書く。
const codexSchemaScript = `set -eu
f=$(mktemp)
trap 'rm -f "$f"' EXIT
printf '%s' "$1" > "$f"
shift
for a do
  shift
  if [ "$a" = "` + schemaPathPlaceholder + `" ]; then
    set -- "$@" "$f"
  else
    set -- "$@" "$a"
  fi
done
"$@"
`

// commandはランナーの後ろに付けて起動するコマンドと引数を組み立てる。
func (r Runner) command(req Request) (name string, args []string, err error) {
	switch req.Tool {
	case Claude:
		return cmp.Or(r.ClaudeCommand, string(Claude)), claudeArgs(req), nil
	case Codex:
		codex := cmp.Or(r.CodexCommand, string(Codex))
		if req.Schema == nil {
			return codex, codexArgs(req, ""), nil
		}
		args := append([]string{"-c", codexSchemaScript, "koryluspilot-codex", string(req.Schema), codex}, codexArgs(req, schemaPathPlaceholder)...)
		return "sh", args, nil
	}
	return "", nil, fmt.Errorf("不明なツール %q", req.Tool)
}

// claudeArgsは `claude -p` の引数を組み立てる。
func claudeArgs(req Request) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		// stream-jsonは--verboseを付けないと受け付けられない。
		"--verbose",
		"--permission-mode", claudePermissionMode,
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.Schema != nil {
		args = append(args, "--json-schema", string(req.Schema))
	}
	return append(args, req.Prompt)
}

// codexArgsは `codex exec` / `codex exec resume` の引数を組み立てる。
// 再開したセッションは起動時のモデル・サンドボックスを引き継がず、Codexの
// 設定ファイルの既定値に戻るため、再開でも毎回渡す。サンドボックスは
// resumeが-sを受け付けないため、どちらも-cで渡す。
func codexArgs(req Request, schemaPath string) []string {
	args := []string{"exec"}
	if req.SessionID != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json")
	if req.Model != "" {
		args = append(args, "-m", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", req.Effort))
	}
	if req.Sandbox != "" {
		args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", req.Sandbox))
	}
	args = append(args, "-c", fmt.Sprintf("approval_policy=%q", codexApprovalPolicy))
	if schemaPath != "" {
		args = append(args, "--output-schema", schemaPath)
	}
	if req.SessionID != "" {
		args = append(args, req.SessionID)
	}
	return append(args, req.Prompt)
}
