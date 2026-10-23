package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/korylus/tools/internal/flow"
	"github.com/korylus/tools/internal/flow/state"
)

// runWaitはwaitサブコマンドのエントリポイント。
func runWait(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return wait(ctx, args, state.Root, stdout, stderr)
}

// waitは役割の番が来るまで待ち、次の指示を標準出力に書く。
func wait(ctx context.Context, args []string, stateRoot string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wait", flag.ContinueOnError)
	flags.SetOutput(stderr)
	roleFlag := flags.String("role", "", "役割 (implementer / reviewer-codex / reviewer-claude)")
	timeout := flags.Duration("timeout", 0, "この時間を過ぎても番が来なければ action: wait を返す (0なら番が来るまで待つ)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "使い方: korylusflow wait <計画書のパス> -role=<役割> [-timeout=<時間>]")
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

	in, err := flow.New(planPath, stateRoot).Wait(ctx, role, *timeout)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "中断しました")
		} else {
			fmt.Fprintf(stderr, "エラー: %v\n", err)
		}
		return exitError
	}
	fmt.Fprint(stdout, in.String())
	return exitOK
}
