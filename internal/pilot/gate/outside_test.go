package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/korylus/tools/internal/pilot/config"
)

// writeFileはroot/relにcontentを書く。親のディレクトリが無ければ作る。
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOutsideWatcher_Check(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		change  func(t *testing.T, root string)
		allowed []string
		// 変わったパスとして停止理由に載ることを期待する「相対パス (種類)」。空なら検知しない。
		want []string
	}{
		{
			name:   "変わっていない",
			change: func(*testing.T, string) {},
		},
		{
			name:   "内容の変更",
			change: func(t *testing.T, root string) { writeFile(t, root, "settings.json", `{"hooks":{}}`) },
			want:   []string{"settings.json (変更)"},
		},
		{
			name: "パーミッションの変更",
			change: func(t *testing.T, root string) {
				if err := os.Chmod(filepath.Join(root, "hooks/pre.sh"), 0o700); err != nil { //#nosec G302 -- フックを実行可能にする変更を再現する
					t.Fatal(err)
				}
			},
			want: []string{"hooks/pre.sh (変更)"},
		},
		{
			name: "シンボリックリンクの参照先の変更",
			change: func(t *testing.T, root string) {
				link := filepath.Join(root, "CLAUDE.md")
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("hooks/pre.sh", link); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"CLAUDE.md (変更)"},
		},
		{
			name:   "ディレクトリとファイルの追加",
			change: func(t *testing.T, root string) { writeFile(t, root, "skills/new/SKILL.md", "x") },
			want:   []string{"skills (追加)", "skills/new (追加)", "skills/new/SKILL.md (追加)"},
		},
		{
			name: "削除",
			change: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "hooks/pre.sh")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"hooks/pre.sh (削除)"},
		},
		{
			name: "除外したパスの変更は比べない",
			change: func(t *testing.T, root string) {
				writeFile(t, root, "projects/a/session.jsonl", "2")
				writeFile(t, root, "projects/b/session.jsonl", "1")
			},
		},
		{
			name: "書かせるパスはプロジェクトからの相対パスで除く",
			change: func(t *testing.T, root string) {
				writeFile(t, root, "plans/1_doing/foo.md", "チェックを入れた")
				writeFile(t, root, "reviews/foo-1.md", "レビュー")
				writeFile(t, root, "plans/1_doing/bar.md", "別の計画書")
			},
			allowed: []string{"docs/private/plans/1_doing/foo.md", "docs/private/reviews/**"},
			want:    []string{"plans/1_doing/bar.md (変更)"},
		},
		{
			name: "書かせるパスの削除は除く",
			change: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "plans/1_doing/foo.md")); err != nil {
					t.Fatal(err)
				}
			},
			allowed: []string{"docs/private/plans/1_doing/foo.md"},
		},
		{
			name: "書かせるパスをシンボリックリンクに置き換えると検知する",
			change: func(t *testing.T, root string) {
				link := filepath.Join(root, "plans/1_doing/foo.md")
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/etc/passwd", link); err != nil {
					t.Fatal(err)
				}
			},
			allowed: []string{"docs/private/plans/1_doing/foo.md"},
			want:    []string{"plans/1_doing/foo.md (変更)"},
		},
		{
			name:    "監視するパスの外を指すglobでは除かない",
			change:  func(t *testing.T, root string) { writeFile(t, root, "plans/1_doing/foo.md", "x") },
			allowed: []string{"plans/1_doing/foo.md"},
			want:    []string{"plans/1_doing/foo.md (変更)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			writeFile(t, root, "settings.json", "{}")
			writeFile(t, root, "hooks/pre.sh", "exit 0\n")
			writeFile(t, root, "projects/a/session.jsonl", "1")
			writeFile(t, root, "plans/1_doing/foo.md", "計画書")
			writeFile(t, root, "plans/1_doing/bar.md", "計画書")
			if err := os.Symlink("settings.json", filepath.Join(root, "CLAUDE.md")); err != nil {
				t.Fatal(err)
			}

			w := OutsideWatcher{Watches: []config.Watch{{
				Name:        "docs",
				Path:        root,
				Exclude:     []string{"projects/**"},
				ProjectPath: "docs/private",
			}}}
			before, err := w.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("前のSnapshotのエラー = %v", err)
			}
			tt.change(t, root)
			after, err := w.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("後のSnapshotのエラー = %v", err)
			}

			got, err := w.Check(before, after, tt.allowed)
			if err != nil {
				t.Fatalf("Checkのエラー = %v", err)
			}
			if len(tt.want) == 0 {
				if len(got) != 0 {
					t.Errorf("Check = %+v、検知しないことを期待", got)
				}
				return
			}
			changes := make([]string, len(tt.want))
			for i, c := range tt.want {
				changes[i] = filepath.Join(root, c)
			}
			want := "リポジトリの外の変更: 監視するパス docs で次のパスが変わっています: " + strings.Join(changes, ", ")
			if len(got) != 1 || got[0].Kind != KindOutside || got[0].Message != want {
				t.Errorf("Check = %+v\n期待するメッセージ = %q", got, want)
			}
		})
	}
}

func TestOutsideWatcher_Check_ManyChanges(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w := OutsideWatcher{Watches: []config.Watch{{Name: "home", Path: root}}}
	before, err := w.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 25 {
		writeFile(t, root, fmt.Sprintf("f%02d", i), "x")
	}
	after, err := w.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	got, err := w.Check(before, after, nil)
	if err != nil {
		t.Fatal(err)
	}
	// ディレクトリ自身は変わらず、25件の追加のうち20件を載せる。
	if len(got) != 1 || !strings.Contains(got[0].Message, "f19 (追加), ほか5件") || strings.Contains(got[0].Message, "f20") {
		t.Errorf("Check = %+v", got)
	}
}

func TestOutsideWatcher_Snapshot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	writeFile(t, dir, "config.toml", "model = \"x\"\n")

	t.Run("ファイルを監視し、ランナーを経由して読む", func(t *testing.T) {
		t.Parallel()

		w := OutsideWatcher{Watches: []config.Watch{{Name: "codex", Path: file, Runner: []string{"env"}}}}
		got, err := w.Snapshot(context.Background())
		if err != nil {
			t.Fatalf("Snapshotのエラー = %v", err)
		}
		if len(got) != 1 || got[0].Watch != "codex" || len(got[0].Entries) != 1 || !strings.HasPrefix(got[0].Entries[""], "f 600  ") {
			t.Errorf("Snapshot = %+v", got)
		}
	})

	t.Run("監視するパスが無ければエラーにする", func(t *testing.T) {
		t.Parallel()

		missing := filepath.Join(dir, "missing")
		w := OutsideWatcher{Watches: []config.Watch{{Name: "gone", Path: missing}}}
		_, err := w.Snapshot(context.Background())
		if err == nil || !strings.Contains(err.Error(), "監視するパス "+missing+" がありません") {
			t.Errorf("Snapshotのエラー = %v", err)
		}
	})

	t.Run("ランナーが失敗したらエラーにする", func(t *testing.T) {
		t.Parallel()

		w := OutsideWatcher{Watches: []config.Watch{{Name: "codex", Path: file, Runner: []string{"false"}}}}
		if _, err := w.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "監視するパス codex") {
			t.Errorf("Snapshotのエラー = %v", err)
		}
	})
}

func TestOutsideWatcher_Snapshot_Prune(t *testing.T) {
	t.Parallel()

	// 監視するパスにfind -pathのメタ文字を含めても、そのままの文字列として飛ばす。
	root := filepath.Join(t.TempDir(), "home[1]*")
	writeFile(t, root, "settings.json", "{}")
	writeFile(t, root, "projects/a/session.jsonl", "1")
	writeFile(t, root, "todos/b.json", "1")
	// 読めないディレクトリは、中を読まずに飛ばさなければ読み取りのエラーになる。
	locked := filepath.Join(root, "projects/a")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o750); err != nil { //#nosec G302 -- 一時ディレクトリを削除できるよう検索権限を戻す
			t.Error(err)
		}
	})

	w := OutsideWatcher{Watches: []config.Watch{{Name: "home", Path: root, Exclude: []string{"projects/**", "todo*/**"}}}}
	got, err := w.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshotのエラー = %v", err)
	}
	var keys []string
	for k := range got[0].Entries {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"", "settings.json"}; !slices.Equal(keys, want) {
		t.Errorf("Snapshotのエントリー = %v、期待値 = %v", keys, want)
	}
}

func TestPrunePatterns(t *testing.T) {
	t.Parallel()

	watch := config.Watch{
		Path:    "/home/dev/.claude",
		Exclude: []string{"projects/**", "a/b/**", "todo*/**", "**", "*.log", "c/**/d"},
	}
	want := []string{"/home/dev/.claude/projects", "/home/dev/.claude/a/b"}
	if got := prunePatterns(watch); !slices.Equal(got, want) {
		t.Errorf("prunePatterns = %v、期待値 = %v", got, want)
	}

	root := config.Watch{Path: "/", Exclude: []string{"proc/**"}}
	if got, want := prunePatterns(root), []string{"/proc"}; !slices.Equal(got, want) {
		t.Errorf("prunePatterns (ルート) = %v、期待値 = %v", got, want)
	}

	meta := config.Watch{Path: `/a[1]*?\`, Exclude: []string{"x/**"}}
	if got, want := prunePatterns(meta), []string{`/a\[1]\*\?\\/x`}; !slices.Equal(got, want) {
		t.Errorf("prunePatterns (メタ文字) = %v、期待値 = %v", got, want)
	}
}

func TestOutsideWatcher_Check_MissingSnapshot(t *testing.T) {
	t.Parallel()

	w := OutsideWatcher{Watches: []config.Watch{{Name: "a", Path: "/a"}, {Name: "b", Path: "/b"}}}
	snapshots := []OutsideSnapshot{{Watch: "a"}}
	if _, err := w.Check(snapshots, snapshots, nil); err == nil || !strings.Contains(err.Error(), "監視するパス b") {
		t.Errorf("Checkのエラー = %v", err)
	}
}

func TestParseSnapshot_Invalid(t *testing.T) {
	t.Parallel()

	watch := config.Watch{Name: "a", Path: "/a"}
	tests := []struct {
		name string
		out  string
		want string
	}{
		{name: "NULで終わらない", out: "meta\x00/a\x00d 755 ", want: "NULで終わっていません"},
		{name: "ハッシュの記録が無い", out: "meta\x00/a\x00d 755 \x00", want: "内容のハッシュの記録がありません"},
		{name: "監視するパスの外", out: "meta\x00/ab\x00f 644 \x00hashes\x00end\x00", want: `監視するパスの外のパス "/ab"`},
		{name: "ハッシュの記録が終わらない", out: "meta\x00/a\x00d 755 \x00hashes\x00", want: "終わっていません"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := parseSnapshot([]byte(tt.out), watch); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("parseSnapshotのエラー = %v、含まれることを期待した文字列 = %q", err, tt.want)
			}
		})
	}
}

func TestOutsideWatcher_Check_AllowedParents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, root string)
		change  func(t *testing.T, root string)
		allowed []string
		want    []string
	}{
		{
			name:    "許可したファイルに必要な複数の親を新規作成できる",
			allowed: []string{"docs/private/plans/1_doing/foo.md"},
		},
		{
			name:    "globで許可したファイルに必要な親を新規作成できる",
			allowed: []string{"docs/private/plans/*/foo.md"},
		},
		{
			name: "許可外の兄弟ファイルは検知する",
			change: func(t *testing.T, root string) {
				writeFile(t, root, "plans/1_doing/foo.md", "計画書")
				writeFile(t, root, "plans/1_doing/bar.md", "別の計画書")
			},
			allowed: []string{"docs/private/plans/1_doing/foo.md"},
			want:    []string{"plans/1_doing/bar.md (追加)"},
		},
		{
			name: "必要のない空のディレクトリは検知する",
			change: func(t *testing.T, root string) {
				writeFile(t, root, "plans/1_doing/foo.md", "計画書")
				if err := os.Mkdir(filepath.Join(root, "plans/other"), 0o750); err != nil {
					t.Fatal(err)
				}
			},
			allowed: []string{"docs/private/plans/1_doing/foo.md"},
			want:    []string{"plans/other (追加)"},
		},
		{
			name: "既存の親ディレクトリの権限変更は検知する",
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "plans/1_doing"), 0o750); err != nil {
					t.Fatal(err)
				}
			},
			change: func(t *testing.T, root string) {
				writeFile(t, root, "plans/1_doing/foo.md", "計画書")
				if err := os.Chmod(filepath.Join(root, "plans"), 0o700); err != nil { //#nosec G302 -- ディレクトリの検索権限を保ったまま権限変更を再現する
					t.Fatal(err)
				}
			},
			allowed: []string{"docs/private/plans/1_doing/foo.md"},
			want:    []string{"plans (変更)"},
		},
		{
			name: "許可したパスに作ったシンボリックリンクは親も含めて検知する",
			change: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "plans/1_doing"), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/etc/passwd", filepath.Join(root, "plans/1_doing/foo.md")); err != nil {
					t.Fatal(err)
				}
			},
			allowed: []string{"docs/private/plans/1_doing/foo.md"},
			want:    []string{"plans (追加)", "plans/1_doing (追加)", "plans/1_doing/foo.md (追加)"},
		},
		{
			name:  "既存のファイルを親ディレクトリに変えると検知する",
			setup: func(t *testing.T, root string) { writeFile(t, root, "plans", "元のファイル") },
			change: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "plans")); err != nil {
					t.Fatal(err)
				}
				writeFile(t, root, "plans/1_doing/foo.md", "計画書")
			},
			allowed: []string{"docs/private/plans/1_doing/foo.md"},
			want:    []string{"plans (変更)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tt.setup != nil {
				tt.setup(t, root)
			}
			w := OutsideWatcher{Watches: []config.Watch{{Name: "docs", Path: root, ProjectPath: "docs/private"}}}
			before, err := w.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tt.change != nil {
				tt.change(t, root)
			} else {
				writeFile(t, root, "plans/1_doing/foo.md", "計画書")
			}
			after, err := w.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got, err := w.Check(before, after, tt.allowed)
			if err != nil {
				t.Fatal(err)
			}
			if len(tt.want) == 0 {
				if len(got) != 0 {
					t.Errorf("Check = %+v、検知しないことを期待", got)
				}
				return
			}
			changes := make([]string, len(tt.want))
			for i, c := range tt.want {
				changes[i] = filepath.Join(root, c)
			}
			want := "リポジトリの外の変更: 監視するパス docs で次のパスが変わっています: " + strings.Join(changes, ", ")
			if len(got) != 1 || got[0].Kind != KindOutside || got[0].Message != want {
				t.Errorf("Check = %+v\n期待するメッセージ = %q", got, want)
			}
		})
	}
}
