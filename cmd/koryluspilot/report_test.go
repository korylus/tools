package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/report"
	"github.com/korylus/tools/internal/pilot/state"
)

// setupReportは、計画書pの作業報告書の第1版を作り終えた状態と、設定ファイルの
// スナップショット (待ち受けるアドレスは空いているポート) を置き、状態のルートと
// プロジェクトのルートを返す。
func setupReport(t *testing.T) (stateRoot, root string) {
	t.Helper()

	root = t.TempDir()
	stateRoot = filepath.Join(root, "tmp", "koryluspilot")
	dir := state.NewDir(stateRoot, "p")
	if err := dir.Save(&state.State{ReportVersion: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := dir.SaveConfigSnapshot([]byte(pilotConfig + "\n[report]\naddr = \"127.0.0.1:0\"\n")); err != nil {
		t.Fatal(err)
	}
	if err := report.Write(report.Dir(root, "p"), &report.Report{Plan: "docs/private/plans/p.md", Name: "p", Version: 1}, nil); err != nil {
		t.Fatal(err)
	}
	return stateRoot, root
}

func TestServeReport(t *testing.T) {
	t.Parallel()

	stateRoot, root := setupReport(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	addrs := make(chan net.Addr, 1)
	done := make(chan int, 1)
	var out, errBuf bytes.Buffer
	go func() {
		done <- serveReport(ctx, []string{"docs/private/plans/p.md"}, stateRoot, root, func(a net.Addr) { addrs <- a }, &out, &errBuf)
	}()

	var addr net.Addr
	select {
	case addr = <-addrs:
	case code := <-done:
		t.Fatalf("待ち受けを始める前に終了した (終了コード = %d、標準エラー = %q)", code, errBuf.String())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr.String()+report.BasePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "作業報告書 第1版: p") {
		t.Errorf("ステータス = %d、本文 = %q", resp.StatusCode, body)
	}

	cancel()
	if code := <-done; code != exitOK {
		t.Errorf("終了コード = %d、期待値 = %d (標準エラー = %q)", code, exitOK, errBuf.String())
	}
	if want := "http://" + addr.String() + report.BasePath; !strings.Contains(out.String(), want) {
		t.Errorf("標準出力 = %q、含まれることを期待した文字列 = %q", out.String(), want)
	}
}

func TestServeReport_Errors(t *testing.T) {
	t.Parallel()

	stateRoot, root := setupReport(t)
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "計画書のパスを必須にする", args: nil, wantErr: "使い方: koryluspilot report"},
		{name: "未知のフラグを弾く", args: []string{"docs/private/plans/p.md", "-bogus"}, wantErr: "bogus"},
		{name: "ルートの外の計画書を弾く", args: []string{"../p.md"}, wantErr: "プロジェクトのルートからの相対パス"},
		{name: "まだrunしていない計画書", args: []string{"docs/private/plans/other.md"}, wantErr: "まだ koryluspilot run で実行していません"},
		{name: "-addrの形の誤り", args: []string{"docs/private/plans/p.md", "-addr=4190"}, wantErr: `-addr: "4190" は <ホスト>:<ポート> の形で指定してください`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out, errBuf bytes.Buffer
			code := serveReport(t.Context(), tt.args, stateRoot, root, nil, &out, &errBuf)
			if code != exitError {
				t.Errorf("終了コード = %d、期待値 = %d", code, exitError)
			}
			if !strings.Contains(errBuf.String(), tt.wantErr) {
				t.Errorf("標準エラー = %q、含まれることを期待した文字列 = %q", errBuf.String(), tt.wantErr)
			}
		})
	}
}
