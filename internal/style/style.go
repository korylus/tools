// Package styleはkorylus-lang.md §3の日本語テキストスタイルを検査する。
//
// 検査するのは2点。
//
//   - §3.1: 地の文に全角丸括弧を使っていないか
//   - §3.2: 半角英数字と日本語が隣り合う箇所に半角スペースを入れていないか
//
// 検査はテキスト1行ごとに行う。
// コードコメントの本文とMarkdownの地の文のどちらにも同じルールを適用できるよう、
// 行単位のAPIにしている。
// CheckLineはインラインコード・URL・見出しの番号を除外する。
// 構文解析でコード範囲を除外する呼び出し元にはCheckMaskedLineを提供し、
// URL・見出しの番号の除外とスタイルの検査を共有する。
package style

import (
	"regexp"
	"strings"
	"unicode"
)

// 節番号。報告の並び順を安定させるために違反へ持たせる。
const (
	sectionParen   = "3.1"
	sectionSpacing = "3.2"
)

const (
	msgParen   = "§3.1: 全角丸括弧が使われている (半角丸括弧に直す)"
	msgSpacing = "§3.2: 半角英数字と日本語の間に半角スペースがある (スペースを詰める)"
)

// reNumberTokenは見出しや箇条書きの番号にあたるトークンにマッチする。
// 数字だけのトークン (`10` など) は番号か地の文かを区別できないため、区切りか
// 末尾の記号を持つもの (`3.2` / `1.` / `1)` / `(1)` / `8-1`) だけを番号とみなす。
var reNumberToken = regexp.MustCompile(`^(?:\(\d+\)|\d+(?:[.-]\d+)+[.):]?|\d+[.):])$`)

// markerPrefixは番号の前に置かれても番号としての判定を妨げない記号。
// 見出し・箇条書き・引用・表・コメント記号が該当する。
const markerPrefix = " \t#>-*+/|"

// Violationは1行のテキストに見つかったスタイル違反1件。
type Violation struct {
	// Sectionは違反したガイドラインの節番号。
	Section string
	// Messageは報告に使うメッセージ。
	Message string
}

// CheckLineはtextを §3のスタイルで検査し、見つかった違反を節番号の順に返す。
// textにはコードコメントの本文か、Markdownの1行を渡す。
// 報告は行単位なので、同じ節の違反が1行に複数あっても1件に畳む。
func CheckLine(text string) []Violation {
	runes := []rune(text)
	return checkLine(runes, maskedRunes(runes))
}

// CheckMaskedLineはコード範囲を除外済みの1行を §3のスタイルで検査する。
// 呼び出し元は構文解析で特定したコード本文を空白に置き換えて渡す。
// 残るバッククォートをコードの区切りとして再解釈せず、URL・見出しの番号の
// 除外と、違反の並び順・行単位での集約はCheckLineと共通にする。
func CheckMaskedLine(text string) []Violation {
	runes := []rune(text)
	masked := make([]bool, len(runes))
	maskURLs(runes, masked)
	return checkLine(runes, masked)
}

// checkLineは除外範囲を適用し、各節の違反を1件ずつ節番号順に返す。
func checkLine(runes []rune, masked []bool) []Violation {
	var vs []Violation
	if hasFullWidthParen(runes, masked) {
		vs = append(vs, Violation{Section: sectionParen, Message: msgParen})
	}
	if hasSpacing(runes, masked) {
		vs = append(vs, Violation{Section: sectionSpacing, Message: msgSpacing})
	}
	return vs
}

// hasFullWidthParenは検査対象の範囲に全角丸括弧があるかを返す。
func hasFullWidthParen(runes []rune, masked []bool) bool {
	for i, r := range runes {
		if masked[i] {
			continue
		}
		if r == '（' || r == '）' {
			return true
		}
	}
	return false
}

// hasSpacingは半角英数字と日本語が半角スペース1つで隔てられた箇所があるかを返す。
//
// 対象は単一の半角スペースに限る。
// 連続したスペースやタブは表の桁合わせやgodocの整形済みブロックの整列であり、
// §3.2が対象とする組版目的のスペースではない。
// 半角丸括弧の両端のスペースは §3.1が優先するが、丸括弧は英数字でも日本語でも
// ないため判定に掛からず、個別の除外を必要としない。
func hasSpacing(runes []rune, masked []bool) bool {
	for i, r := range runes {
		if r != ' ' || masked[i] {
			continue
		}
		if i == 0 || i == len(runes)-1 {
			continue
		}
		left, right := runes[i-1], runes[i+1]
		if left == ' ' || right == ' ' {
			continue
		}
		if masked[i-1] || masked[i+1] {
			continue
		}
		if !isAlnumJapanesePair(left, right) {
			continue
		}
		if isNumberedHeadingGap(runes, i) {
			continue
		}
		return true
	}
	return false
}

// isAlnumJapanesePairは2つのルーンが半角英数字と日本語の組み合わせかを返す。
// どちらが前でも対象になる。
func isAlnumJapanesePair(left, right rune) bool {
	return isASCIIAlnum(left) && isJapanese(right) || isJapanese(left) && isASCIIAlnum(right)
}

// isNumberedHeadingGapはspの位置のスペースが、見出しや箇条書きの番号と
// それに続く見出し文の間にあるかを返す (§3.2の対象外)。
// 番号は行の先頭にあるものだけを認め、その前に置けるのは見出し記号・箇条書き記号
// などのmarkerPrefixに限る。
func isNumberedHeadingGap(runes []rune, sp int) bool {
	start := sp
	for start > 0 && runes[start-1] != ' ' && runes[start-1] != '\t' {
		start--
	}
	if !reNumberToken.MatchString(string(runes[start:sp])) {
		return false
	}
	for _, r := range runes[:start] {
		if !strings.ContainsRune(markerPrefix, r) {
			return false
		}
	}
	return true
}

// maskedRunesは検査対象外のルーンに印を付けたスライスを返す。
// 対象外はインラインコードと、地の文にそのまま置くURL。
func maskedRunes(runes []rune) []bool {
	masked := make([]bool, len(runes))
	maskInlineCode(runes, masked)
	maskURLs(runes, masked)
	return masked
}

// maskInlineCodeは同じ連続数のバッククォートで囲まれた範囲に印を付ける。
// 閉じ記号がなければ開始記号だけを読み飛ばし、残りのコード範囲の探索を続ける。
func maskInlineCode(runes []rune, masked []bool) {
	for i := 0; i < len(runes); {
		if runes[i] != '`' {
			i++
			continue
		}
		startEnd := i + 1
		for startEnd < len(runes) && runes[startEnd] == '`' {
			startEnd++
		}
		end := -1
		for j := startEnd; j < len(runes); {
			if runes[j] != '`' {
				j++
				continue
			}
			runEnd := j + 1
			for runEnd < len(runes) && runes[runEnd] == '`' {
				runEnd++
			}
			if runEnd-j == startEnd-i {
				end = runEnd
				break
			}
			j = runEnd
		}
		if end < 0 {
			i = startEnd
			continue
		}
		for k := i; k < end; k++ {
			masked[k] = true
		}
		i = end
	}
}

// maskURLsはスキーム付きのURLの範囲に印を付ける。
// URLの前後に置く半角スペースは語の境界を示すためのもので、§3.2の対象外。
func maskURLs(runes []rune, masked []bool) {
	for i := 0; i < len(runes); i++ {
		if !hasSchemeAt(runes, i) {
			continue
		}
		end := i
		for end < len(runes) && !isURLEnd(runes[end]) {
			end++
		}
		for k := i; k < end; k++ {
			masked[k] = true
		}
		i = end
	}
}

// hasSchemeAtはrunesのiの位置からURLのスキームが始まるかを返す。
func hasSchemeAt(runes []rune, i int) bool {
	for _, scheme := range []string{"https://", "http://"} {
		if i+len(scheme) <= len(runes) && string(runes[i:i+len(scheme)]) == scheme {
			return true
		}
	}
	return false
}

// isURLEndはrがURLの終わりとみなす文字かを返す。
// 日本語のパスもURLに含め、空白と地の文の区切り記号で終了する。
// Markdownのリンク記法や山括弧で囲む形に合わせて ")" と ">" も終わりとみなす。
func isURLEnd(r rune) bool {
	switch {
	case unicode.IsSpace(r) || strings.ContainsRune("`)<>\"'。、！？「」", r):
		return true
	default:
		return false
	}
}

// isASCIIAlnumはrが半角英数字かを返す。
func isASCIIAlnum(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
}

// isJapaneseはrが日本語の文字かを返す。
// 長音符と繰り返し記号はUnicodeの用字がCommonでひらがな・カタカナ・漢字の
// いずれにも含まれないため、語の一部として個別に足す。
func isJapanese(r rune) bool {
	switch r {
	case 'ー', '々':
		return true
	}
	return unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Han, r)
}
