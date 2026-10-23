package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/usage"
)

// fakeFetcherはツールごとに決まった観測かエラーを返す。
type fakeFetcher map[agent.Tool][]usage.Sample

func (f fakeFetcher) Fetch(_ context.Context, tool agent.Tool, now time.Time) ([]usage.Sample, error) {
	samples, ok := f[tool]
	if !ok {
		return nil, errors.New("ログインしていません")
	}
	out := make([]usage.Sample, len(samples))
	for i, s := range samples {
		s.At, s.Tool, s.Source = now, tool, usage.SourceUsageCommand
		out[i] = s
	}
	return out, nil
}

func TestShowUsage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.Local)
	fiveHourReset := now.Add(3 * time.Hour)
	weeklyReset := now.Add(72 * time.Hour)

	root := t.TempDir()
	// 2時間前の観測があれば、そこからの差で消費速度を求める。
	if err := usage.NewLog(root).AppendSamples([]usage.Sample{
		{At: now.Add(-2 * time.Hour), Tool: agent.Claude, Window: usage.FiveHour, Percent: 40, ResetsAt: fiveHourReset, Source: usage.SourceStream},
		{At: now.Add(-2 * time.Hour), Tool: agent.Claude, Window: usage.Weekly, Percent: 20, ResetsAt: weeklyReset, Source: usage.SourceStream},
	}); err != nil {
		t.Fatal(err)
	}
	fetcher := fakeFetcher{agent.Claude: {
		{Window: usage.FiveHour, Percent: 60, ResetsAt: fiveHourReset},
		{Window: usage.Weekly, Percent: 21, ResetsAt: weeklyReset},
	}}

	var out, errBuf bytes.Buffer
	// 設定ファイルを置いていないディレクトリでも既定値で動く。
	code := showUsage(nil, root, fixedFetcher(fetcher), func() time.Time { return now }, &out, &errBuf)
	if code != exitError {
		t.Errorf("終了コード = %d、期待値 = %d (Codexの取得に失敗するため)", code, exitError)
	}

	for _, want := range []string{
		"Claude Code",
		"5時間枠: 60% (上限 90%)、リセット " + fiveHourReset.Format(timeLayout),
		// 2時間で20%消費 → 10%/時間。残り30%は3時間で、リセットと同時刻なので達する見込みとして出す。
		"直近の消費速度: 10.0%/時間 (" + now.Add(3*time.Hour).Format(timeLayout) + " ごろに上限に達する見込み)",
		"週枠: 21% (上限 80%)",
		"直近の消費速度: 0.5%/時間 (リセットまでに上限に達しない見込み)",
		"Codex\n  取得できませんでした",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("標準出力 = %q、含まれることを期待した文字列 = %q", out.String(), want)
		}
	}
	if !strings.Contains(errBuf.String(), "Codex の使用率: ログインしていません") {
		t.Errorf("標準エラー = %q、Codexの取得の失敗を期待", errBuf.String())
	}

	samples, _, err := usage.NewLog(root).Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 4 {
		t.Errorf("記録の件数 = %d、期待値 = 4 (取得した観測を記録に足す)", len(samples))
	}
}

func TestShowUsage_Projection(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.Local)
	tests := []struct {
		name    string
		percent float64
		want    string
	}{
		{name: "上限に達している", percent: 95, want: "上限に達しています"},
		{name: "記録が足りない", percent: 30, want: "直近の消費速度: 不明"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetcher := fakeFetcher{
				agent.Claude: {{Window: usage.FiveHour, Percent: tt.percent, ResetsAt: now.Add(time.Hour)}},
				agent.Codex:  {{Window: usage.Weekly, Percent: 1}},
			}
			var out, errBuf bytes.Buffer
			code := showUsage(nil, t.TempDir(), fixedFetcher(fetcher), func() time.Time { return now }, &out, &errBuf)
			if code != exitOK {
				t.Errorf("終了コード = %d、期待値 = %d (標準エラー: %s)", code, exitOK, errBuf.String())
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("標準出力 = %q、含まれることを期待した文字列 = %q", out.String(), tt.want)
			}
			if !strings.Contains(out.String(), "週枠: 1% (上限 80%)、リセット 不明") {
				t.Errorf("標準出力 = %q、リセット時刻が不明なCodexの週枠を期待", out.String())
			}
		})
	}
}

// fixedFetcherは、読んだ設定によらずfを返すshowUsageのnewFetcherを作る。
func fixedFetcher(f usage.Fetcher) func(*config.Config) usage.Fetcher {
	return func(*config.Config) usage.Fetcher { return f }
}

// pilotConfigは、usageの上限を50%にした設定ファイルの内容。
const pilotConfig = `
project = "tools"
base_branch = "develop"
reviewers = ["reviewer_codex", "reviewer_claude"]

[usage]
five_hour_cap = 50

[hub]
upstream = "https://github.com/korylus/tools.git"
path = "/koryluspilot/hub.git"

[roles.designer]
tool = "claude"
runner = ["docker", "compose", "run", "--rm", "-T", "pilot-designer"]

[roles.implementer]
tool = "claude"
runner = ["docker", "compose", "run", "--rm", "-T", "pilot-implementer"]

[roles.reviewer_codex]
tool = "codex"
runner = ["docker", "compose", "run", "--rm", "-T", "pilot-reviewer-codex"]

[roles.reviewer_claude]
tool = "claude"
runner = ["docker", "compose", "run", "--rm", "-T", "pilot-reviewer-claude"]
`

func TestCommandFetcher(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse(pilotConfig)
	if err != nil {
		t.Fatalf("config.Parseのエラー = %v", err)
	}
	rf, ok := commandFetcher(cfg).(roleFetcher)
	if !ok {
		t.Fatalf("commandFetcher() = %T、roleFetcherを期待", commandFetcher(cfg))
	}
	f, ok := rf.fetcher.(usage.CommandFetcher)
	if !ok {
		t.Fatalf("roleFetcher.fetcher = %T、usage.CommandFetcherを期待", rf.fetcher)
	}
	// 各ツールを使う役割のうち、名前の順で最初の役割のランナーで取得する。
	wantClaude := []string{"docker", "compose", "run", "--rm", "-T", "pilot-designer"}
	wantCodex := []string{"docker", "compose", "run", "--rm", "-T", "pilot-reviewer-codex"}
	if !reflect.DeepEqual(f.Claude.Prefix, wantClaude) || !reflect.DeepEqual(f.Codex.Prefix, wantCodex) {
		t.Errorf("ランナー = (%v, %v)、期待値 = (%v, %v)", f.Claude.Prefix, f.Codex.Prefix, wantClaude, wantCodex)
	}

	// 設定ファイルが無ければ現在の環境で取得する。
	if got := commandFetcher(nil); !reflect.DeepEqual(got, usage.CommandFetcher{}) {
		t.Errorf("commandFetcher(nil) = %+v、ゼロ値を期待", got)
	}
}

func TestRoleFetcher(t *testing.T) {
	t.Parallel()

	text := strings.ReplaceAll(pilotConfig, `tool = "codex"`, `tool = "claude"`)
	cfg, err := config.Parse(text)
	if err != nil {
		t.Fatalf("config.Parseのエラー = %v", err)
	}
	inner := &recordingFetcher{fetcher: fakeFetcher{
		agent.Claude: {{Window: usage.FiveHour, Percent: 10}},
		agent.Codex:  {{Window: usage.FiveHour, Percent: 20}},
	}}
	f := roleFetcher{cfg: cfg, fetcher: inner}

	if _, err := f.Fetch(context.Background(), agent.Codex, time.Now()); err == nil || !strings.Contains(err.Error(), "役割") {
		t.Errorf("Codexの取得のエラー = %v、役割が無いことによるエラーを期待", err)
	}
	samples, err := f.Fetch(context.Background(), agent.Claude, time.Now())
	if err != nil || len(samples) != 1 {
		t.Errorf("Claude Codeの取得 = %v、エラー = %v、1件を期待", samples, err)
	}
	// 役割の無いツールは、内側のFetcherを呼ばない。
	if !reflect.DeepEqual(inner.tools, []agent.Tool{agent.Claude}) {
		t.Errorf("取得したツール = %v、期待値 = %v", inner.tools, []agent.Tool{agent.Claude})
	}
}

func TestShowUsage_Config(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "pilot.toml")
	if err := os.WriteFile(cfgPath, []byte(pilotConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	fetcher := fakeFetcher{
		agent.Claude: {{Window: usage.FiveHour, Percent: 10}},
		agent.Codex:  {{Window: usage.Weekly, Percent: 1}},
	}

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{name: "指定した設定ファイルの上限を使う", args: []string{"-config=" + cfgPath}, wantCode: exitOK, wantOut: "5時間枠: 10% (上限 50%)"},
		{name: "指定した設定ファイルが無ければエラー", args: []string{"-config=" + filepath.Join(dir, "missing.toml")}, wantCode: exitError, wantErr: "missing.toml"},
		{name: "位置引数は受け付けない", args: []string{"plan.md"}, wantCode: exitError, wantErr: "使い方: koryluspilot usage"},
		{name: "-hは0で終わる", args: []string{"-h"}, wantCode: exitOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out, errBuf bytes.Buffer
			code := showUsage(tt.args, t.TempDir(), fixedFetcher(fetcher), time.Now, &out, &errBuf)
			if code != tt.wantCode {
				t.Errorf("終了コード = %d、期待値 = %d (標準エラー: %s)", code, tt.wantCode, errBuf.String())
			}
			if tt.wantOut != "" && !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("標準出力 = %q、含まれることを期待した文字列 = %q", out.String(), tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(errBuf.String(), tt.wantErr) {
				t.Errorf("標準エラー = %q、含まれることを期待した文字列 = %q", errBuf.String(), tt.wantErr)
			}
		})
	}
}

// recordingFetcherは取得を依頼されたツールを記録する。
type recordingFetcher struct {
	tools   []agent.Tool
	fetcher usage.Fetcher
}

func (f *recordingFetcher) Fetch(ctx context.Context, tool agent.Tool, now time.Time) ([]usage.Sample, error) {
	f.tools = append(f.tools, tool)
	return f.fetcher.Fetch(ctx, tool, now)
}

func TestShowUsage_SingleTool(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		tool        agent.Tool
		from        string
		to          string
		wantLabel   string
		unusedLabel string
	}{
		{name: "Claude Codeだけ使う", tool: agent.Claude, from: "codex", to: "claude", wantLabel: "Claude Code", unusedLabel: "Codex"},
		{name: "Codexだけ使う", tool: agent.Codex, from: "claude", to: "codex", wantLabel: "Codex", unusedLabel: "Claude Code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfgPath := filepath.Join(t.TempDir(), "pilot.toml")
			text := strings.ReplaceAll(pilotConfig, "tool = \""+tc.from+"\"", "tool = \""+tc.to+"\"")
			if err := os.WriteFile(cfgPath, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			f := &recordingFetcher{fetcher: fakeFetcher{
				tc.tool: {{Window: usage.FiveHour, Percent: 10}},
			}}
			var out, errBuf bytes.Buffer
			root := t.TempDir()
			code := showUsage([]string{"-config=" + cfgPath}, root, fixedFetcher(f), time.Now, &out, &errBuf)
			if code != exitOK || errBuf.Len() != 0 {
				t.Errorf("終了コード = %d、標準エラー = %q、正常終了を期待", code, errBuf.String())
			}
			if !reflect.DeepEqual(f.tools, []agent.Tool{tc.tool}) {
				t.Errorf("取得したツール = %v、期待値 = %v", f.tools, []agent.Tool{tc.tool})
			}
			if !strings.HasPrefix(out.String(), tc.wantLabel+"\n") || strings.Contains(out.String(), tc.unusedLabel) {
				t.Errorf("標準出力 = %q、使用するツールだけの表示を期待", out.String())
			}
			samples, _, err := usage.NewLog(root).Read()
			if err != nil || len(samples) != 1 {
				t.Fatalf("使用率の記録 = %v、エラー = %v、1件を期待", samples, err)
			}
			if samples[0].Tool != tc.tool {
				t.Errorf("記録したツール = %q、期待値 = %q", samples[0].Tool, tc.tool)
			}
		})
	}
}
