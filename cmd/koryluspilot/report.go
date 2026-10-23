package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/report"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/plan"
)

// HTTPサーバーのタイムアウト。開発者がブラウザから使うだけのため、遅い接続を待ち続けない。
const (
	reportReadHeaderTimeout = 10 * time.Second
	reportReadTimeout       = 30 * time.Second
	reportShutdownTimeout   = 5 * time.Second
)

// runReportはreportサブコマンドのエントリポイント。
func runReport(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveReport(ctx, args, state.Root, ".", nil, stdout, stderr)
}

// serveReportは計画書の作業報告書のうち最新の版を、ctxが終わるまで配信する。状態は
// stateRootの下から、作業報告書はプロジェクトのルートrootの下から読む。readyには
// 待ち受けを始めたアドレスを渡す (テストで接続先を知るため)。nilなら呼ばない。
func serveReport(ctx context.Context, args []string, stateRoot, root string, ready func(net.Addr), stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("addr", "", "待ち受けるアドレス (省略時は設定ファイルの report.addr)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "使い方: koryluspilot report <計画書のパス> [-addr=<待ち受けるアドレス>]")
		flags.PrintDefaults()
	}
	positional, err := parseInterspersed(flags, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if len(positional) != 1 {
		flags.Usage()
		return exitError
	}
	planPath, err := projectPath(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}

	planName := plan.Name(planPath)
	dir := state.NewDir(stateRoot, planName)
	listenAddr, err := reportAddr(dir, *addr)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "エラー: 計画書 %s はまだ koryluspilot run で実行していません\n", planPath)
		} else {
			fmt.Fprintf(stderr, "エラー: %v\n", err)
		}
		return exitError
	}

	srv := &report.Server{
		Dir: report.Dir(root, planName),
		// 実行中のrunが新しい版を作ることがあるため、要求のたびに状態を読む。
		Latest: func() (int, error) {
			st, err := dir.Load()
			if err != nil {
				return 0, err
			}
			return st.ReportVersion, nil
		},
		Log: stderr,
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	httpServer := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: reportReadHeaderTimeout, ReadTimeout: reportReadTimeout}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(ln) }()

	fmt.Fprintf(stdout, "作業報告書を配信しています: http://%s%s\n", ln.Addr(), report.BasePath)
	fmt.Fprintf(stdout, "devサイトの %s を開いて動作確認してください (止めるにはCtrl+C)\n", report.BasePath)
	if ready != nil {
		ready(ln.Addr())
	}

	select {
	case err := <-served:
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	case <-ctx.Done():
	}
	// 保存の途中の要求を書き終えてから止める。ctxは終わっているため、別に期限を切る。
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	return exitOK
}

// reportAddrは待ち受けるアドレスを返す。flagAddrが空なら、runが取った設定ファイルの
// スナップショットの report.addr を使う。スナップショットはエージェントが書き換えられない
// 状態ディレクトリにあり、人間が合意した設定を表す。まだrunしていなければ
// fs.ErrNotExistを包んだエラーを返す。
func reportAddr(dir state.Dir, flagAddr string) (string, error) {
	if _, err := dir.Load(); err != nil {
		return "", err
	}
	if flagAddr != "" {
		if err := config.ValidateAddr(flagAddr); err != nil {
			return "", fmt.Errorf("-addr: %w", err)
		}
		return flagAddr, nil
	}
	src, err := dir.LoadConfigSnapshot()
	if err != nil {
		return "", err
	}
	cfg, err := config.Parse(string(src))
	if err != nil {
		return "", fmt.Errorf("設定ファイルのスナップショットを読み取れません:\n%w", err)
	}
	return cfg.Report.Addr, nil
}
