package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/runner"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/pilot/usage"
)

// usageToolsは使用率を表示するツールと表示名。
var usageTools = []struct {
	tool  agent.Tool
	label string
}{
	{agent.Claude, "Claude Code"},
	{agent.Codex, "Codex"},
}

// runUsageはusageサブコマンドのエントリポイント。
func runUsage(args []string, stdout, stderr io.Writer) int {
	return showUsage(args, state.Root, commandFetcher, time.Now, stdout, stderr)
}

// commandFetcherは、設定ファイルがあれば各ツールを使う役割のランナーで、無ければ
// 現在の環境で使用率を取得するFetcherを返す。認証は役割のコンテナ用のボリュームにあるため、
// 役割のコンテナで取得する。設定ファイルがあるときは、役割の無いツールを起動しない。
func commandFetcher(cfg *config.Config) usage.Fetcher {
	var f usage.CommandFetcher
	if cfg == nil {
		return f
	}
	if role, ok := cfg.RoleUsing(config.ToolClaude); ok {
		f.Claude = runner.Runner{Prefix: role.Runner}
	}
	if role, ok := cfg.RoleUsing(config.ToolCodex); ok {
		f.Codex = runner.Runner{Prefix: role.Runner}
	}
	return roleFetcher{cfg: cfg, fetcher: f}
}

// roleFetcherは、設定でそのツールを使う役割があるときだけfetcherで取得する。
// 役割の無いツールのランナーはゼロ値 (VM上での直接起動) になるため、起動せずに
// エラーを返す。
type roleFetcher struct {
	cfg     *config.Config
	fetcher usage.Fetcher
}

func (f roleFetcher) Fetch(ctx context.Context, tool agent.Tool, now time.Time) ([]usage.Sample, error) {
	if _, ok := f.cfg.RoleUsing(string(tool)); !ok {
		return nil, fmt.Errorf("%s を使う役割が設定にありません", tool)
	}
	return f.fetcher.Fetch(ctx, tool, now)
}

// showUsageは各ツールの使用率を取得し直して記録し、直近の消費速度と上限に
// 達する見込みを表示する。記録はstateRootの直下に置く。newFetcherには読んだ設定
// (設定ファイルが無ければnil) を渡す。
func showUsage(args []string, stateRoot string, newFetcher func(*config.Config) usage.Fetcher, now func() time.Time, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("usage", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "設定ファイル (省略時は "+config.FileName+"。無ければ使用量の既定値を使う)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "使い方: koryluspilot usage [-config=<設定ファイル>]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return exitError
	}

	cfg, err := loadConfigIfExists(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}

	usageCfg := config.DefaultUsage()
	if cfg != nil {
		usageCfg = cfg.Usage
	}
	tracker := usage.Tracker{Log: usage.NewLog(stateRoot), Fetcher: newFetcher(cfg), Now: now}
	code := exitOK
	shown := false
	for _, t := range usageTools {
		if cfg != nil {
			if _, ok := cfg.RoleUsing(string(t.tool)); !ok {
				continue
			}
		}
		if shown {
			fmt.Fprintln(stdout)
		}
		shown = true
		fmt.Fprintln(stdout, t.label)
		// 表示のために明示的に取り直す。トークンは消費しない。
		if _, err := tracker.Refresh(context.Background(), t.tool); err != nil {
			fmt.Fprintln(stdout, "  取得できませんでした")
			fmt.Fprintf(stderr, "エラー: %s の使用率: %v\n", t.label, err)
			code = exitError
			continue
		}
		samples, _, err := tracker.Log.Read()
		if err != nil {
			fmt.Fprintf(stderr, "エラー: %v\n", err)
			return exitError
		}
		writeToolUsage(stdout, usageCfg, samples, t.tool, now())
	}
	return code
}

// loadConfigIfExistsは設定ファイルを読む。pathを省略して既定の設定ファイルも
// 無いときはnilを返す (設定ファイルの無いリポジトリでも使用率を確かめられるようにする)。
func loadConfigIfExists(path string) (*config.Config, error) {
	explicit := path != ""
	if !explicit {
		path = config.FileName
	}
	if _, err := os.Stat(path); !explicit && errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return config.Load(path)
}

// writeToolUsageはtoolの枠ごとの使用率と見込みを表示する。
func writeToolUsage(w io.Writer, cfg config.Usage, samples []usage.Sample, tool agent.Tool, now time.Time) {
	latest := usage.Latest(samples, tool)
	for _, win := range usage.Windows {
		s, ok := latest[win]
		if !ok {
			continue
		}
		limit := usage.Cap(cfg, win)
		percent := s.PercentAt(now)
		reset := "不明"
		if !s.ResetsAt.IsZero() {
			reset = s.ResetsAt.Local().Format(timeLayout)
		}
		fmt.Fprintf(w, "  %s: %g%% (上限 %g%%)、リセット %s\n", win.Label(), percent, limit, reset)
		fmt.Fprintf(w, "    %s\n", projection(samples, s, tool, limit, now))
	}
	if len(latest) == 0 {
		fmt.Fprintln(w, "  使用率の枠がありません")
	}
}

// projectionは直近の消費速度と、上限に達する見込みを1行で返す。
func projection(samples []usage.Sample, s usage.Sample, tool agent.Tool, limit float64, now time.Time) string {
	percent := s.PercentAt(now)
	if percent >= limit {
		return "上限に達しています (リセットまで自動実行は待ちます)"
	}
	rate, ok := usage.Rate(samples, tool, s.Window, now)
	if !ok {
		return "直近の消費速度: 不明 (同じリセットの期間の記録が足りません)"
	}
	if rate == 0 {
		return "直近の消費速度: 0%/時間 (上限に達する見込みはありません)"
	}
	reach := now.Add(time.Duration((limit - percent) / rate * float64(time.Hour)))
	if !s.ResetsAt.IsZero() && reach.After(s.ResetsAt) {
		return fmt.Sprintf("直近の消費速度: %.1f%%/時間 (リセットまでに上限に達しない見込み)", rate)
	}
	return fmt.Sprintf("直近の消費速度: %.1f%%/時間 (%s ごろに上限に達する見込み)", rate, reach.Local().Format(timeLayout))
}
