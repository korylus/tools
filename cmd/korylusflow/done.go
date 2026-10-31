package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/korylus/tools/internal/flow"
	"github.com/korylus/tools/internal/flow/state"
)

// runDoneはdoneサブコマンドのエントリポイント。
func runDone(args []string, stdout, stderr io.Writer) int {
	return done(context.Background(), args, state.Root, os.Stdin, stdout, stderr)
}

// doneは役割が進めているステップの完了を記録する。
// ブランチを作る実装タスクでは、stdinから動作確認の項目をJSONの配列で読む。
// 処理の流れのステップでは、stdinから処理の流れの場所か書かなかった理由をJSONで読む。
// [implementer] のタスクでは、計画書のチェックを確かめ、stdinは読まない。
func done(ctx context.Context, args []string, stateRoot string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("done", flag.ContinueOnError)
	flags.SetOutput(stderr)
	roleFlag := flags.String("role", "", "役割 (implementer / reviewer-codex / reviewer-claude)")
	reviewLimitFlag := flags.Int("review-limit", 0, "上限に達したタスクの新しいレビュー上限 (例: 3回からあと3回続ける場合は6)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "使い方: korylusflow done <計画書> -role=<役割> [-review-limit=<新しい上限>] [< 動作確認の項目か処理の流れのJSON]")
		flags.PrintDefaults()
	}
	planPath, err := parsePlanArgs(flags, args)
	if err != nil {
		return parseFailed(flags, err)
	}
	role, err := state.ParseRole(*roleFlag)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}

	f, err := flow.New(ctx, planPath, stateRoot)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	var msg string
	if *reviewLimitFlag == 0 {
		msg, err = f.Done(ctx, role, stdin)
	} else {
		msg, err = f.DoneWithReviewLimit(ctx, role, stdin, *reviewLimitFlag)
	}
	var rejected *flow.RejectedError
	switch {
	case errors.As(err, &rejected):
		fmt.Fprintln(stderr, rejected.Error())
		return exitRejected
	case err != nil:
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, msg)
	return exitOK
}
