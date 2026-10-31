package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/korylus/tools/internal/flow/source"
	"github.com/korylus/tools/internal/flow/state"
)

// runReportはreportサブコマンドのエントリポイント。
func runReport(args []string, stdout, stderr io.Writer) int {
	return report(context.Background(), args, state.Root, source.Parse, stdout, stderr)
}

// reportは作業報告書の最新の版を作り直す。
func report(ctx context.Context, args []string, stateRoot string, parse parseSource, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "使い方: korylusflow report <計画書>")
	}
	planURL, err := parsePlanArgs(flags, args)
	if err != nil {
		return parseFailed(flags, err)
	}

	f, err := openFlow(ctx, parse, planURL, stateRoot)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	path, err := f.Report(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "作業報告書を書きました: %s\n", path)
	return exitOK
}
