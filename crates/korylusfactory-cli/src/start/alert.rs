//! 人間待ちの知らせ。通知のコマンドの実行と、端末のタイトルとベル。
//!
//! 通知のコマンドは、人間待ちになったとき (項目を積んだあと) と、差し戻したときに1回ずつ実行する。
//! 項目を積めなかったときはURLを渡さずに知らせ、あとで積めたらURLを渡してもう一度知らせる。
//! 通知のコマンドを実行する間は、状態ファイルのロックを持たない。失敗と時間切れは、警告を出すだけにする。
//!
//! 端末への表示は、標準エラーが端末のときだけ行う。
//! 端末のタイトルに今の状態を出し (OSC 2)、人間待ちになったときにベル (BEL) を1回鳴らす。

use korylusfactory_core::state::{ItemPage, Notified, PlanState, Wait};

use super::{Director, Error};
use crate::describe;
use crate::notify::Message;

/// 端末のタイトルの先頭に付ける名前。
pub(super) const TITLE_PREFIX: &str = "korylusfactory";

impl Director<'_, '_> {
    /// 人間を待っている作業計画書name (状態state・待ちwait) を、端末に表示し、まだ知らせていなければ通知のコマンドで知らせる。
    ///
    /// pageは、待ちに積んだ人間キューの項目のページ。積めていなければ `None` で、URLを渡さずに知らせる。
    pub(super) fn alert(
        &mut self,
        name: &str,
        state: &PlanState,
        wait: &Wait,
        page: Option<&ItemPage>,
    ) -> Result<(), Error> {
        self.title(&format!(
            "{TITLE_PREFIX} - 人間待ち - {}",
            wait.kind.label()
        ))?;
        self.ring(&format!("{name}\n{wait:?}"))?;
        let url = page.map(|page| page.url.clone());
        // 同じ待ちを知らせたあとは、URLを渡さずに知らせた待ちの項目を積めたときだけ、もう一度知らせる。
        let notified = state.notified.as_ref().is_some_and(|notified| {
            &notified.wait == wait && (notified.url.is_some() || url.is_none())
        });
        if notified || !self.notify(name, wait.kind.label(), url.clone())? {
            return Ok(());
        }
        self.change(name, |state| {
            // 知らせている間に待ちが解けていれば、記録しない。
            if state.wait.as_ref() != Some(wait) {
                return false;
            }
            state.notified = Some(Notified {
                wait: wait.clone(),
                url,
            });
            true
        })?;
        Ok(())
    }

    /// 作業計画書planが場面sceneで人間を待っていることを、通知のコマンドで知らせる。urlは人間キューの項目のURL。
    ///
    /// 通知のコマンドを使わない設定なら、何もせずに偽を返す。
    /// 通知のコマンドが失敗しても警告を出すだけにし、知らせたものとして真を返す。
    pub(super) fn notify(
        &mut self,
        plan: &str,
        scene: &str,
        url: Option<String>,
    ) -> Result<bool, Error> {
        let Some(project) = &self.config.project else {
            return Ok(false);
        };
        let Some(command) = &project.notify else {
            return Ok(false);
        };
        let message = Message {
            project: project.name.clone(),
            plan: plan.to_owned(),
            scene: scene.to_owned(),
            url,
        };
        let result = self
            .start
            .notifier
            .notify(command, &message, self.start.timing.notify);
        if let Err(err) = result {
            self.diagnostic(format!(
                "{plan}: 人間待ちを通知のコマンドで知らせられません: {}",
                describe(&err)
            ))?;
        }
        Ok(true)
    }

    /// 標準エラーが端末なら、端末のタイトルをtextにする。前に出したタイトルと同じなら、出し直さない。
    pub(super) fn title(&mut self, text: &str) -> Result<(), Error> {
        if !self.start.terminal || self.title.as_deref() == Some(text) {
            return Ok(());
        }
        // 作業計画書から来た文字が、端末の制御シーケンスとして読まれないようにする。
        let clean: String = text.chars().filter(|c| !c.is_control()).collect();
        write!(self.err, "\x1b]2;{clean}\x1b\\")
            .and_then(|()| self.err.flush())
            .map_err(Error::Output)?;
        self.title = Some(text.to_owned());
        Ok(())
    }

    /// 標準エラーが端末なら、人間待ちkeyになったことをベルで1回知らせる。
    ///
    /// 作業へ戻るか、進める作業計画書が無くなるまでに鳴らした人間待ちなら、鳴らさない。
    /// 別の人間待ちのベルを挟んでも、続いている人間待ちで鳴らし直さないためである。
    pub(super) fn ring(&mut self, key: &str) -> Result<(), Error> {
        if !self.start.terminal || self.rung.contains(key) {
            return Ok(());
        }
        self.err
            .write_all(b"\x07")
            .and_then(|()| self.err.flush())
            .map_err(Error::Output)?;
        self.rung.insert(key.to_owned());
        Ok(())
    }
}
