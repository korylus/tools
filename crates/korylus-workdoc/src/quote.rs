//! 作業ドキュメントの診断で使う、Goの `%q` と同じ形式の文字列の引用。

use std::fmt::Write as _;

use unicode_general_category::{GeneralCategory, get_general_category};

/// 表示できる文字は残し、制御文字や非表示文字はGoのエスケープで表す。
pub(crate) fn quote(value: &str) -> String {
    let mut quoted = String::with_capacity(value.len() + 2);
    quoted.push('"');
    for c in value.chars() {
        match c {
            '"' | '\\' => {
                quoted.push('\\');
                quoted.push(c);
            }
            '\u{7}' => quoted.push_str(r"\a"),
            '\u{8}' => quoted.push_str(r"\b"),
            '\u{c}' => quoted.push_str(r"\f"),
            '\n' => quoted.push_str(r"\n"),
            '\r' => quoted.push_str(r"\r"),
            '\t' => quoted.push_str(r"\t"),
            '\u{b}' => quoted.push_str(r"\v"),
            _ if printable(c) => quoted.push(c),
            _ => {
                let point = c as u32;
                if point < 0x20 || point == 0x7f {
                    let _ = write!(quoted, "\\x{point:02x}");
                } else if point < 0x10000 {
                    let _ = write!(quoted, "\\u{point:04x}");
                } else {
                    let _ = write!(quoted, "\\U{point:08x}");
                }
            }
        }
    }
    quoted.push('"');
    quoted
}

/// Goの表示可能文字はUnicodeのL・M・N・P・S分類と、ASCIIのスペースに限る。
fn printable(c: char) -> bool {
    c == ' '
        || !matches!(
            get_general_category(c),
            GeneralCategory::Control
                | GeneralCategory::Format
                | GeneralCategory::Unassigned
                | GeneralCategory::PrivateUse
                | GeneralCategory::Surrogate
                | GeneralCategory::LineSeparator
                | GeneralCategory::ParagraphSeparator
                | GeneralCategory::SpaceSeparator
        )
}

#[cfg(test)]
mod tests {
    use super::quote;

    #[test]
    fn goのstrconv_quoteと同じ形式で引用する() {
        let cases = [
            ("doc\u{3000}", r#""doc\u3000""#),
            ("a\0b", r#""a\x00b""#),
            ("a\u{c}b", r#""a\fb""#),
            ("a\u{7}\u{8}\u{b}\u{1b}b", r#""a\a\b\v\x1bb""#),
            ("a\t\n\rb", r#""a\t\n\rb""#),
            ("\u{7f}\u{85}\u{a0}", r#""\x7f\u0085\u00a0""#),
            ("\"\\'日本語😀", r#""\"\\'日本語😀""#),
            ("e\u{301}", "\"e\u{301}\""),
            ("\u{1e4d0}", "\"\u{1e4d0}\""),
            ("\u{1e5d0}", r#""\U0001e5d0""#),
            ("\u{e000}\u{100000}", r#""\ue000\U00100000""#),
        ];
        for (value, want) in cases {
            assert_eq!(quote(value), want, "{value:?}の引用がGoと異なる");
        }
    }
}
