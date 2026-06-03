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
		t.Fatalf("Parse: %v", err)
	}
	if opts.Base != "develop" {
		t.Errorf("Base = %q, want %q", opts.Base, "develop")
	}
	if got := fs.Args(); len(got) != 2 {
		// The two positional args remain after -base is consumed.
		// [Ja] -base を取り除いた残りの位置引数が 2 つ残る。
		t.Errorf("positional args = %v, want 2", got)
	}
}

func TestRegisterCommonDefaultsToEmpty(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := RegisterCommon(fs)

	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if opts.Base != "" {
		// An empty base means the whole tree is checked.
		// [Ja] base が空のときは全体を検査する。
		t.Errorf("default Base = %q, want empty", opts.Base)
	}
}
