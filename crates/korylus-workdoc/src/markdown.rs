//! pulldown-cmarkで解析する前の、Markdownの本文の前処理。

/// `src` を、移植元のgoldmarkと同じコードブロックの判定になるよう整えた解析用の写しを返す。
///
/// pulldown-cmarkは、閉じるフェンスの後ろにタブがあるとフェンスを閉じない。
/// CommonMarkとgoldmarkは閉じるため、行末の空白とタブを同じバイト数のスペースに置き換える。
/// バイト数は変わらないため、写しを解析した位置は元の本文の位置としてそのまま使える。
pub(crate) fn parse_source(src: &str) -> String {
    src.split('\n')
        .map(|line| {
            let body = line.trim_end_matches([' ', '\t']);
            format!("{body}{}", " ".repeat(line.len() - body.len()))
        })
        .collect::<Vec<_>>()
        .join("\n")
}

#[cfg(test)]
mod tests {
    use super::parse_source;

    #[test]
    fn 行末の空白とタブだけを同じバイト数のスペースにする() {
        let src = "```\t\n\tコード\t \n本文\tの途中 \t\n";
        let got = parse_source(src);

        assert_eq!(
            got, "``` \n\tコード  \n本文\tの途中  \n",
            "解析用の写しが期待と異なる"
        );
        assert_eq!(got.len(), src.len(), "バイト数が変わった");
    }
}
