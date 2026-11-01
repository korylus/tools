package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "引数無しは使い方を表示してエラー", args: nil, wantCode: exitError, wantStderr: "使い方:"},
		{name: "-hは使い方を表示して成功", args: []string{"-h"}, wantCode: exitOK, wantStdout: "使い方:"},
		{name: "不明なサブコマンドはエラー", args: []string{"start"}, wantCode: exitError, wantStderr: `不明なサブコマンド "start"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != tt.wantCode {
				t.Errorf("終了コード = %d、期待値 = %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("標準出力 = %q、%q を含むことを期待", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("標準エラー = %q、%q を含むことを期待", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestParsePlanArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantURL  string
		wantRole string
		wantErr  error
	}{
		{name: "フラグを計画書の指定の後ろに書ける", args: []string{"https://wikino.app/s/shimbaco/pages/1", "-role=implementer"}, wantURL: "https://wikino.app/s/shimbaco/pages/1", wantRole: "implementer"},
		{name: "フラグを計画書の指定の前に書ける", args: []string{"-role=implementer", "https://wikino.app/s/shimbaco/pages/1"}, wantURL: "https://wikino.app/s/shimbaco/pages/1", wantRole: "implementer"},
		{name: "フラグは省略できる", args: []string{"https://wikino.app/s/shimbaco/pages/1"}, wantURL: "https://wikino.app/s/shimbaco/pages/1"},
		{name: "計画書の指定が無いとエラー", args: []string{"-role=implementer"}, wantErr: errUsage},
		{name: "計画書の指定が2つあるとエラー", args: []string{"https://wikino.app/s/shimbaco/pages/1", "https://wikino.app/s/shimbaco/pages/2"}, wantErr: errUsage},
		{name: "-hはflag.ErrHelp", args: []string{"https://wikino.app/s/shimbaco/pages/1", "-h"}, wantErr: flag.ErrHelp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			flags.SetOutput(io.Discard)
			role := flags.String("role", "", "")

			planURL, err := parsePlanArgs(flags, tt.args)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("parsePlanArgsのエラー = %v、期待値 = %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePlanArgsのエラー = %v", err)
			}
			if planURL != tt.wantURL || *role != tt.wantRole {
				t.Errorf("計画書の指定 = %q、role = %q、期待値 = %q, %q", planURL, *role, tt.wantURL, tt.wantRole)
			}
		})
	}
}
