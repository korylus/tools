package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string // stdoutに含まれることを期待する文字列 (空なら検査しない)
		wantErr  string // stderrに含まれることを期待する文字列 (空なら検査しない)
	}{
		{name: "引数なしはstderrに使い方を出す", args: nil, wantCode: 2, wantErr: "使い方:"},
		{name: "helpはstdoutに使い方を出す", args: []string{"--help"}, wantCode: 0, wantOut: "使い方:"},
		{name: "未知のサブコマンド", args: []string{"bogus"}, wantCode: 2, wantErr: "不明なサブコマンド"},
		{name: "commentは自前のflag setで未知のフラグを弾く", args: []string{"comment", "--bogus"}, wantCode: 2, wantErr: "bogus"},
		{name: "mdは自前のflag setで未知のフラグを弾く", args: []string{"md", "--bogus"}, wantCode: 2, wantErr: "bogus"},
		{name: "commentは共通フラグ -baseを受け取る", args: []string{"comment", "-base=main", "-h"}, wantCode: 0},
		{name: "mdは共通フラグ -baseを受け取る", args: []string{"md", "-base=main", "-h"}, wantCode: 0},
		{name: "comment -hは0で終わる", args: []string{"comment", "-h"}, wantCode: 0},
		{name: "md -hは0で終わる", args: []string{"md", "-h"}, wantCode: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out, errBuf bytes.Buffer
			code := run(tt.args, &out, &errBuf)
			if code != tt.wantCode {
				t.Errorf("run(%v) の終了コード = %d、期待値 = %d", tt.args, code, tt.wantCode)
			}
			if tt.wantOut != "" && !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("run(%v) の標準出力 = %q、含まれることを期待した文字列 = %q", tt.args, out.String(), tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(errBuf.String(), tt.wantErr) {
				t.Errorf("run(%v) の標準エラー = %q、含まれることを期待した文字列 = %q", tt.args, errBuf.String(), tt.wantErr)
			}
		})
	}
}
