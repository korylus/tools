//! セッションのPATHにだけ置くGitの入口。既存のpushurl設定に依存せずpushを拒否する。

use std::env;
use std::ffi::{OsStr, OsString};
use std::fs;
use std::io;
use std::os::unix::ffi::OsStrExt as _;
use std::os::unix::fs::PermissionsExt as _;
use std::path::Path;

use super::Error;

/// セッション専用のGitの入口を記録のディレクトリlogsの下に作り、そのPATHと補助の拒否設定を返す。
///
/// 本物のgitは、korylusfactoryのPATHの値inheritedから探す。
pub(super) fn environment(
    logs: &Path,
    inherited: &OsStr,
) -> Result<Vec<(OsString, OsString)>, Error> {
    prepare(logs, inherited).map_err(Error::Guard)
}

fn prepare(logs: &Path, inherited: &OsStr) -> io::Result<Vec<(OsString, OsString)>> {
    let real = env::split_paths(inherited)
        .map(|dir| dir.join("git"))
        .find(|path| {
            path.metadata().is_ok_and(|metadata| {
                metadata.is_file() && metadata.permissions().mode() & 0o111 != 0
            })
        })
        .ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::NotFound,
                "PATHにgitの実行ファイルがありません",
            )
        })?;
    let real = fs::canonicalize(real)?;
    let dir = logs.join("git-guard");
    fs::create_dir_all(&dir)?;
    let mut script = b"#!/bin/sh\n".to_vec();
    script.extend_from_slice(
        r#"check_command() {
    while [ "$#" -gt 0 ]; do
        case "$1" in
            -C|-c|--git-dir|--work-tree|--namespace|--config-env|--attr-source|--shallow-file)
                shift
                [ "$#" -gt 0 ] || return 0
                shift
                ;;
            --) shift; break ;;
            -*) shift ;;
            *) break ;;
        esac
    done
    case "${1-}" in
        push|send-pack)
            echo 'korylusfactory: セッションからのgit pushは許可されていません' >&2
            return 1
            ;;
    esac
}
check_command "$@" || exit 1
exec "#
            .as_bytes(),
    );
    script.extend(quote(real.as_os_str()));
    script.extend_from_slice(b" \"$@\"\n");
    let path = dir.join("git");
    fs::write(&path, script)?;
    fs::set_permissions(&path, fs::Permissions::from_mode(0o700))?;
    let dir = fs::canonicalize(dir)?;
    // Bashのログイン設定がPATHを作り直しても、非対話の起動時にGitの入口を戻す。
    let mut startup = Vec::new();
    if let Some(previous) = env::var_os("BASH_ENV") {
        startup.extend_from_slice(b"if [ -r ");
        startup.extend(quote(&previous));
        startup.extend_from_slice(b" ]; then\n    . ");
        startup.extend(quote(&previous));
        startup.extend_from_slice(b"\nfi\n");
    }
    startup.extend_from_slice(b"export PATH=");
    startup.extend(quote(dir.as_os_str()));
    startup.extend_from_slice(b":\"$PATH\"\n");
    let startup_path = dir.join("bash-env");
    fs::write(&startup_path, startup)?;
    fs::set_permissions(&startup_path, fs::Permissions::from_mode(0o600))?;
    let path = env::join_paths(std::iter::once(dir).chain(env::split_paths(inherited)))
        .map_err(io::Error::other)?;
    let mut variables = super::guard_env();
    variables.push(("PATH".into(), path));
    variables.push(("BASH_ENV".into(), startup_path.into()));
    Ok(variables)
}

/// Unixのパスを、POSIXシェルで1つの引数になるように引用する。
fn quote(value: &OsStr) -> Vec<u8> {
    let mut out = vec![b'\''];
    for &byte in value.as_bytes() {
        if byte == b'\'' {
            out.extend_from_slice(b"'\\''");
        } else {
            out.push(byte);
        }
    }
    out.push(b'\'');
    out
}
