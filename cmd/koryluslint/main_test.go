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
		wantOut  string // substring expected on stdout (skipped when empty). [Ja] stdout に含まれることを期待する文字列 (空なら検査しない)
		wantErr  string // substring expected on stderr (skipped when empty). [Ja] stderr に含まれることを期待する文字列 (空なら検査しない)
	}{
		{name: "no args shows usage on stderr", args: nil, wantCode: 2, wantErr: "Usage:"},
		{name: "help shows usage on stdout", args: []string{"--help"}, wantCode: 0, wantOut: "Usage:"},
		{name: "unknown subcommand", args: []string{"bogus"}, wantCode: 2, wantErr: "unknown subcommand"},
		{name: "comment rejects an unknown flag via its own flag set", args: []string{"comment", "--bogus"}, wantCode: 2, wantErr: "bogus"},
		{name: "md rejects an unknown flag via its own flag set", args: []string{"md", "--bogus"}, wantCode: 2, wantErr: "bogus"},
		{name: "comment accepts the common -base flag", args: []string{"comment", "-base=main", "-h"}, wantCode: 0},
		{name: "md accepts the common -base flag", args: []string{"md", "-base=main", "-h"}, wantCode: 0},
		{name: "subcommand -h exits 0", args: []string{"comment", "-h"}, wantCode: 0},
		{name: "md -h exits 0", args: []string{"md", "-h"}, wantCode: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out, errBuf bytes.Buffer
			code := run(tt.args, &out, &errBuf)
			if code != tt.wantCode {
				t.Errorf("run(%v) code = %d, want %d", tt.args, code, tt.wantCode)
			}
			if tt.wantOut != "" && !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("run(%v) stdout = %q, want contains %q", tt.args, out.String(), tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(errBuf.String(), tt.wantErr) {
				t.Errorf("run(%v) stderr = %q, want contains %q", tt.args, errBuf.String(), tt.wantErr)
			}
		})
	}
}
