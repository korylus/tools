package branch

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Commitはコミット1件。
type Commit struct {
	// Hashは短縮したコミットのハッシュ。
	Hash    string
	Subject string
}

// FileChangeは変更したファイル1件の行数。
type FileChange struct {
	Path    string
	Added   int
	Removed int
	// Binaryは、バイナリのファイルで行数を数えられないかどうか。
	Binary bool
}

// Commitsは、リポジトリrepoでbaseに無くheadにあるコミットを、古い順に返す。
func Commits(ctx context.Context, repo, base, head string) ([]Commit, error) {
	out, err := git(ctx, repo, "log", "--reverse", "--format=%h%x09%s", base+".."+head)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		hash, subject, _ := strings.Cut(line, "\t")
		commits = append(commits, Commit{Hash: hash, Subject: subject})
	}
	return commits, nil
}

// Changesは、リポジトリrepoでbaseとheadの分岐点からheadまでに変更したファイルを返す。
// 名前の変更は、元のファイルの削除と新しいファイルの追加として数える。
func Changes(ctx context.Context, repo, base, head string) ([]FileChange, error) {
	out, err := git(ctx, repo, "diff", "--numstat", "--no-renames", "-z", base+"..."+head)
	if err != nil {
		return nil, err
	}
	var files []FileChange
	for rec := range strings.SplitSeq(out, "\x00") {
		if rec == "" {
			continue
		}
		fields := strings.SplitN(rec, "\t", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("git diff --numstat の出力を読み取れません: %q", rec)
		}
		fc := FileChange{Path: fields[2]}
		if fields[0] == "-" && fields[1] == "-" {
			fc.Binary = true
		} else {
			if fc.Added, err = strconv.Atoi(fields[0]); err != nil {
				return nil, fmt.Errorf("git diff --numstat の出力を読み取れません: %q", rec)
			}
			if fc.Removed, err = strconv.Atoi(fields[1]); err != nil {
				return nil, fmt.Errorf("git diff --numstat の出力を読み取れません: %q", rec)
			}
		}
		files = append(files, fc)
	}
	return files, nil
}
