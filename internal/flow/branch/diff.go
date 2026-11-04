package branch

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// FileChangeは変更したファイル1件の行数。
type FileChange struct {
	Path    string
	Added   int
	Removed int
	// Binaryは、バイナリのファイルで行数を数えられないかどうか。
	Binary bool
}

// Changesは、リポジトリrepoでローカルのブランチbaseとheadの分岐点からheadまでに変更したファイルを返す。
// 同名のタグがあってもブランチどうしを比べるよう、baseとheadは完全な参照名で指す。
// 名前の変更は、元のファイルの削除と新しいファイルの追加として数える。
func Changes(ctx context.Context, repo, base, head string) ([]FileChange, error) {
	out, err := git(ctx, repo, "diff", "--numstat", "--no-renames", "-z", "refs/heads/"+base+"...refs/heads/"+head)
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
