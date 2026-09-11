package cli

import (
	"flag"
	"io"
	"testing"
)

func TestRegisterCommonParsesBase(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := RegisterCommon(fs)

	if err := fs.Parse([]string{"-base=develop", "a.go", "b.go"}); err != nil {
		t.Fatalf("Parseに失敗した: %v", err)
	}
	if opts.Base != "develop" {
		t.Errorf("Base = %q、期待値 = %q", opts.Base, "develop")
	}
	if got := fs.Args(); len(got) != 2 {
		// -baseを取り除いた残りの位置引数が2つ残る。
		t.Errorf("位置引数 = %v、期待する個数 = 2", got)
	}
}

func TestRegisterCommonDefaultsToEmpty(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := RegisterCommon(fs)

	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parseに失敗した: %v", err)
	}
	if opts.Base != "" {
		// baseが空のときは全体を検査する。
		t.Errorf("既定のBase = %q、期待値 = 空文字", opts.Base)
	}
}
