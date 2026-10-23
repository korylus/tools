package gate

import (
	"path"
	"strings"
)

// matchGlobはスラッシュ区切りのパスnameが、globに当たるかを返す。
// `**` だけの要素は0個以上のディレクトリに当たり、それ以外の要素はpath.Matchの
// 構文で照合する。globの構文の誤りは設定の読み込みで検証しているため、
// ここでは当たらないものとして扱う。
func matchGlob(glob, name string) bool {
	return matchElems(strings.Split(glob, "/"), strings.Split(name, "/"))
}

// matchAnyはnameがglobsのどれかに当たるかを返す。
func matchAny(globs []string, name string) bool {
	for _, g := range globs {
		if matchGlob(g, name) {
			return true
		}
	}
	return false
}

func matchElems(glob, name []string) bool {
	for len(glob) > 0 {
		if glob[0] == "**" {
			for len(glob) > 0 && glob[0] == "**" {
				glob = glob[1:]
			}
			if len(glob) == 0 {
				return true
			}
			for i := range len(name) + 1 {
				if matchElems(glob, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(glob[0], name[0]); err != nil || !ok {
			return false
		}
		glob, name = glob[1:], name[1:]
	}
	return len(name) == 0
}
