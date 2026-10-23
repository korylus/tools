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

// doneは役割が進めているステップの完了を記録する。実装のステップでは、stdinから
// 動作確認の項目をJSONの配列で読む。
func done(ctx context.Context, args []string, stateRoot string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("done", flag.ContinueOnError)
	flags.SetOutput(stderr)
	roleFlag := flags.String("role", "", "役割 (implementer / reviewer-codex / reviewer-claude)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "使い方: korylusflow done <計画書のパス> -role=<役割> [< 動作確認の項目のJSON]")
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

	msg, err := flow.New(planPath, stateRoot).Done(ctx, role, stdin)
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
