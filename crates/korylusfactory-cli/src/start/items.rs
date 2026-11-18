//! 監督のループでの、人間キューの項目の積み上げと片付け。
//!
//! 人間待ちになった作業計画書と、状態ファイルに持たない待ち (並びの先頭の作業計画書を読み取れない・ブランチが取り込まれていない) を、
//! 人間キューの項目として積む。項目が公開される前に待ちが解けたら、本文を状態の置き場へ残してから下書きを片付ける。
//! 人間を待っている間は項目を読む間隔ごとに項目を読み、公開されていれば回答を適用して再開する。
//! 回答を適用できないときは、公開されたページに理由を書いた下書きを作り直して差し戻す。
//!
//! Wikinoの読み書きの間は、状態ファイルのロックを持たない。
//! 作成を送る前に項目のタイトルを記録に残し、作成の結果が分からないまま止まっても、次の回に下書きと公開済みページから探せるようにする。
//! Wikinoに読み書きできないときは警告を出して間隔を空けてやり直し、人間待ちの記録と進行中のタスクは止めない。

use std::fs;
use std::io;
use std::path::{Path, PathBuf};

use jiff::civil::DateTime;
use jiff::{SignedDuration, Timestamp};
use korylusfactory_core::flow;
use korylusfactory_core::item::{self, Item};
use korylusfactory_core::state::{
    ItemPage, Notice, NoticeKind, PlanState, QueuedItem, Wait, WaitItem,
};

use super::{Director, Error, minutes};
use crate::describe;
use crate::human_queue::{self, Location, Status, Store};

/// 作業計画書の状態の置き場のうち、片付けた項目の本文を残すディレクトリの名前。
const KEPT_DIR: &str = "queue";

/// プロジェクトのルートからの、片付けた知らせの項目の本文を残すディレクトリ。
pub(super) const NOTICE_KEPT_DIR: &str = "docs/private/factory/queue";

/// 項目を積めないか、片付けられないときのエラー。
#[derive(Debug, thiserror::Error)]
enum ItemError {
    /// 項目を積んだ置き場が分からないか、ページのURLと一致しない。
    #[error(
        "人間キューの項目「{0}」の作成時の置き場を確かめられません。項目の記録のスペース・トピックとページのURLを確認してください"
    )]
    Location(String),
    /// 置き場を読み書きできない。
    #[error(transparent)]
    Store(#[from] human_queue::Error),

    /// 項目の内容を決められない。
    #[error(transparent)]
    Content(#[from] flow::Error),

    /// 回答を適用するために、作業計画書を読めない。
    #[error(transparent)]
    Plan(Box<Error>),

    /// 片付ける項目の本文を残せない。
    #[error("片付ける項目の本文を {} に残せません", .path.display())]
    Keep {
        /// 本文を書こうとしたファイル。
        path: PathBuf,
        /// 書けなかった理由。
        #[source]
        source: io::Error,
    },
}

impl Director<'_, '_> {
    /// 人間キューを使う設定なら、プロジェクト名と置き場を返す。
    fn queue_place(&self) -> Option<(String, Location)> {
        let project = self.config.project.as_ref()?;
        Some((project.name.clone(), project.queue.clone()?))
    }

    /// 今の日時を、項目のタイトルと本文に書く日時にする。
    fn local_now(&self) -> DateTime {
        self.start
            .clock
            .now()
            .to_zoned(self.start.time_zone.clone())
            .datetime()
    }

    /// 人間キューを読み書きしてよいかを返す。前に失敗してから、やり直す間隔が過ぎていなければ偽。
    fn queue_due(&self) -> bool {
        self.queue_after
            .is_none_or(|after| self.start.clock.now() >= after)
    }

    /// 人間キューの読み書きの結果resultを受け取る。
    ///
    /// 失敗なら、何をできなかったかwhatを添えて警告し、やり直す間隔を空けるようにして `None` を返す。
    fn queue_result<T>(
        &mut self,
        what: &str,
        result: Result<T, ItemError>,
    ) -> Result<Option<T>, Error> {
        match result {
            Ok(value) => {
                self.queue_after = None;
                Ok(Some(value))
            }
            Err(err) => {
                let interval = self.start.timing.queue;
                let wait = SignedDuration::try_from(interval).unwrap_or(SignedDuration::MAX);
                self.queue_after = self.start.clock.now().checked_add(wait).ok();
                self.diagnostic(format!(
                    "{what}: {}\n  {}分後にやり直します",
                    describe(&err),
                    minutes(interval)
                ))?;
                Ok(None)
            }
        }
    }

    /// 作業計画書nameの状態を、ロックの中で読み直してchangeで変え、変えたら書く。変えたかを返す。
    pub(super) fn change(
        &self,
        name: &str,
        change: impl FnOnce(&mut PlanState) -> bool,
    ) -> Result<bool, Error> {
        let lock = self.store.plan(name)?.lock()?;
        let Some(mut state) = lock.load()? else {
            return Ok(false);
        };
        let changed = change(&mut state);
        if changed {
            lock.save(&state)?;
        }
        Ok(changed)
    }

    /// 人間を待っている作業計画書name (状態state・待ちwait) の項目を、まだ積んでいなければ積み、項目のページを返す。
    ///
    /// 人間キューを使わない設定のときと、積めなかったときは `None` を返す。
    pub(super) fn stack(
        &mut self,
        name: &str,
        state: &PlanState,
        wait: &Wait,
    ) -> Result<Option<ItemPage>, Error> {
        let Some((project, configured)) = self.queue_place() else {
            return Ok(None);
        };
        let recorded = state.waiting_item().map(|record| record.item.clone());
        if let Some(page) = recorded.as_ref().and_then(|record| record.page.clone()) {
            return Ok(Some(page));
        }
        if !self.queue_due() {
            return Ok(None);
        }
        let what = format!("{name}: 人間キューに項目を積めません");
        let location = recorded.as_ref().map_or_else(
            || Ok(configured.clone()),
            |record| recorded_location(record, Some(&configured)),
        );
        let Some(location) = self.queue_result(&what, location)? else {
            return Ok(None);
        };
        let at = recorded
            .as_ref()
            .map_or_else(|| self.local_now(), |record| record.at);
        let content =
            item::for_wait(&project, name, state, wait, &self.project, at).map_err(ItemError::from);
        let Some(content) = self.queue_result(&what, content)? else {
            return Ok(None);
        };
        let title = recorded
            .as_ref()
            .map_or_else(|| content.title(), |record| record.title.clone());
        if recorded.is_none() {
            // 作成を送る前にタイトルを残し、結果が分からないまま止まっても、次の回に一覧で探せるようにする。
            let queued = QueuedItem {
                location: Some((&location).into()),
                title: title.clone(),
                at,
                page: None,
            };
            let kept = self.change(name, |state| {
                // ロックを取るまでの間に待ちが解けていれば、積まない。
                if state.wait.as_ref() != Some(wait) {
                    return false;
                }
                state.items.push(WaitItem {
                    wait: wait.clone(),
                    item: queued,
                });
                true
            })?;
            if !kept {
                return Ok(None);
            }
        }
        let start = self.start;
        let store = start.queues.open(&location);
        let put = put(store.as_ref(), recorded.is_some(), &title, &content.body());
        let Some(number) = self.queue_result(&what, put)? else {
            return Ok(None);
        };
        let page = ItemPage {
            number,
            url: location.edit_url(number),
        };
        self.change(name, |state| {
            let Some(record) = state
                .items
                .iter_mut()
                .find(|record| record.item.title == title)
            else {
                return false;
            };
            record.item.page = Some(page.clone());
            true
        })?;
        // 積んだばかりの項目には、まだ回答が無いため、項目を読む間隔が過ぎてから読む。
        self.answer_read = Some(self.start.clock.now());
        self.say(&format!(
            "{name}: 人間キューに項目を積みました: {}",
            page.url
        ))?;
        Ok(Some(page))
    }

    /// 人間を待つ間に、前に項目を読んでから、項目を読む間隔が過ぎたかを返す。
    fn answer_due(&self) -> bool {
        let interval =
            SignedDuration::try_from(self.start.timing.queue).unwrap_or(SignedDuration::MAX);
        let now = self.start.clock.now();
        self.answer_read
            .is_none_or(|read: Timestamp| now.duration_since(read) >= interval)
    }

    /// 人間を待っている作業計画書name (状態state・待ちwait) の項目 (ページpage) を、項目を読む間隔ごとに読み、
    /// 公開されていれば回答を適用する。回答を適用して再開したら真を返す。
    ///
    /// 回答を適用できないときは、理由を書いた下書きを作り直して差し戻す。
    /// 項目の下書きもページも無くなっていれば、記録を除き、次の回に積み直す。
    pub(super) fn receive(
        &mut self,
        name: &str,
        state: &PlanState,
        wait: &Wait,
        page: &ItemPage,
    ) -> Result<bool, Error> {
        let Some(record) = state.waiting_item().cloned() else {
            return Ok(false);
        };
        if !self.answer_due() || !self.queue_due() {
            return Ok(false);
        }
        self.answer_read = Some(self.start.clock.now());
        let what = format!("{name}: 人間キューの項目を読めません");
        let configured = self.queue_place().map(|(_, location)| location);
        let location = recorded_location(&record.item, configured.as_ref());
        let Some(location) = self.queue_result(&what, location)? else {
            return Ok(false);
        };
        let start = self.start;
        let store = start.queues.open(&location);
        let status = store.read(page.number).map_err(ItemError::from);
        let Some(status) = self.queue_result(&what, status)? else {
            return Ok(false);
        };
        match status {
            Status::Drafted { .. } => Ok(false),
            Status::Missing => {
                self.change(name, |state| {
                    let before = state.items.len();
                    state.items.retain(|current| current != &record);
                    state.items.len() != before
                })?;
                self.say(&format!(
                    "{name}: 人間キューの項目「{}」の下書きもページも無くなっていたため、積み直します",
                    record.item.title
                ))?;
                Ok(false)
            }
            Status::Published { body, .. } => {
                self.apply(name, state, wait, &record, store.as_ref(), page, &body)
            }
        }
    }

    /// 公開された項目record (ページpage・本文body) の回答を、作業計画書nameの待ちwaitに適用する。適用したら真を返す。
    ///
    /// 作業計画書を読む間と、差し戻しの下書きを作る間は、状態ファイルのロックを持たない。
    #[expect(
        clippy::too_many_arguments,
        reason = "読んだ項目と開いた置き場を、読み直さずに適用と差し戻しへ渡すため"
    )]
    fn apply(
        &mut self,
        name: &str,
        state: &PlanState,
        wait: &Wait,
        record: &WaitItem,
        store: &dyn Store,
        page: &ItemPage,
        body: &str,
    ) -> Result<bool, Error> {
        let what = format!("{name}: 人間キューの項目の回答を適用できません");
        let doc = self
            .read_plan(&state.url)
            .map_err(|err| ItemError::Plan(Box::new(err)));
        let Some(doc) = self.queue_result(&what, doc)? else {
            return Ok(false);
        };
        let lock = self.store.plan(name)?.lock()?;
        let Some(mut current) = lock.load()? else {
            return Ok(false);
        };
        // ロックを取るまでの間に `resume` などで待ちが解けていれば、回答を適用しない。
        if current.wait.as_ref() != Some(wait) {
            return Ok(false);
        }
        match flow::resume_with(&mut current, &doc, &self.project, body) {
            Ok(()) => {
                // 公開されて回答済みになった項目は、Wikinoに残すため片付けずに記録から除く。
                current.items.retain(|current| current != record);
                lock.save(&current)?;
                drop(lock);
                self.say(&format!(
                    "{name}: 人間キューの項目の回答で、{}の人間待ちから再開しました: {}",
                    wait.kind.label(),
                    page.url
                ))?;
                Ok(true)
            }
            Err(flow::Error::Rejected(reasons)) => {
                drop(lock);
                let back = item::send_back(body, &reasons, self.local_now());
                let reopened = store.reopen(page.number, &back).map_err(ItemError::from);
                let what = format!("{name}: 人間キューの項目を差し戻せません");
                if self.queue_result(&what, reopened)?.is_some() {
                    let mut text = format!(
                        "{name}: 人間キューの項目の回答を適用できないため、理由を書いて差し戻しました: {}",
                        page.url
                    );
                    for reason in &reasons {
                        text.push_str("\n  ");
                        text.push_str(reason);
                    }
                    self.say(&text)?;
                    let scene = format!("{} (差し戻し)", wait.kind.label());
                    self.notify(name, &scene, Some(page.url.clone()))?;
                }
                Ok(false)
            }
            Err(err) => {
                drop(lock);
                self.queue_result(&what, Err::<(), _>(ItemError::Content(err)))?;
                Ok(false)
            }
        }
    }

    /// 待ちが解けた作業計画書の項目を片付ける。plansは、完了を記録したものも含む作業計画書の計画書名と状態。
    pub(super) fn tidy(&mut self, plans: &[(String, PlanState)]) -> Result<(), Error> {
        let configured = self.queue_place().map(|(_, location)| location);
        for (name, state) in plans {
            for record in &state.items {
                // 状態の待ちを人間待ちにするのはこのループだけのため、解けた待ちが同じ待ちに戻ることはない。
                if state.wait.as_ref() == Some(&record.wait) {
                    continue;
                }
                if !self.queue_due() {
                    return Ok(());
                }
                let keep = self.store.plan(name)?.path().join(KEPT_DIR);
                let cleared =
                    recorded_location(&record.item, configured.as_ref()).and_then(|location| {
                        let store = self.start.queues.open(&location);
                        clear(store.as_ref(), &record.item, &keep)
                    });
                let what = format!("{name}: 人間キューの項目を片付けられません");
                if self.queue_result(&what, cleared)?.is_none() {
                    return Ok(());
                }
                let title = &record.item.title;
                self.change(name, |state| {
                    let before = state.items.len();
                    state.items.retain(|current| current != record);
                    state.items.len() != before
                })?;
                self.say(&format!(
                    "{name}: 待ちが解けたため、人間キューの項目「{title}」を片付けました"
                ))?;
            }
        }
        Ok(())
    }

    /// 知らせの項目 (場面kind・作業計画書plan) を、まだ積んでいなければ積む。
    ///
    /// contentは、プロジェクト名と項目を積む日時から、項目の内容を作る。
    pub(super) fn raise(
        &mut self,
        kind: NoticeKind,
        plan: &str,
        content: impl FnOnce(&str, DateTime) -> Item,
    ) -> Result<(), Error> {
        let Some((project, configured)) = self.queue_place() else {
            return Ok(());
        };
        let mut notices = self.notices.load()?;
        let recorded = notices
            .iter()
            .find(|notice| !notice.resolved && notice.kind == kind && notice.plan == plan)
            .map(|notice| notice.item.clone());
        if recorded
            .as_ref()
            .is_some_and(|record| record.page.is_some())
            || !self.queue_due()
        {
            return Ok(());
        }
        let location = recorded.as_ref().map_or_else(
            || Ok(configured.clone()),
            |record| recorded_location(record, Some(&configured)),
        );
        let Some(location) = self.queue_result("人間キューに知らせの項目を積めません", location)?
        else {
            return Ok(());
        };
        let at = recorded
            .as_ref()
            .map_or_else(|| self.local_now(), |record| record.at);
        let content = content(&project, at);
        let title = recorded
            .as_ref()
            .map_or_else(|| content.title(), |record| record.title.clone());
        if recorded.is_none() {
            // 作成を送る前にタイトルを残し、結果が分からないまま止まっても、次の回に一覧で探せるようにする。
            notices.push(Notice {
                resolved: false,
                kind,
                plan: plan.to_owned(),
                item: QueuedItem {
                    location: Some((&location).into()),
                    title: title.clone(),
                    at,
                    page: None,
                },
            });
            self.notices.save(&notices)?;
        }
        let start = self.start;
        let store = start.queues.open(&location);
        let put = put(store.as_ref(), recorded.is_some(), &title, &content.body());
        let Some(number) = self.queue_result("人間キューに知らせの項目を積めません", put)?
        else {
            return Ok(());
        };
        let url = location.edit_url(number);
        let mut notices = self.notices.load()?;
        if let Some(notice) = notices.iter_mut().find(|notice| notice.item.title == title) {
            notice.item.page = Some(ItemPage {
                number,
                url: url.clone(),
            });
        }
        self.notices.save(&notices)?;
        self.say(&format!("人間キューに知らせの項目を積みました: {url}"))?;
        self.ring(&format!("{}\n{plan}", content.scene.label()))?;
        // 知らせを見分けるplanはロードマップのWikiリンクのタイトルのため、通知には項目の内容の計画書名を渡す。
        // 読み取れない作業計画書では、計画書名が分からないため、項目の内容がタイトルを計画書名にしている。
        self.notify(&content.plan.name, content.scene.label(), Some(url))?;
        Ok(())
    }

    /// 知らせの項目のうち、resolvedが真を返すもの (待ちが解けたもの) を片付ける。
    pub(super) fn settle(&mut self, resolved: impl Fn(&Notice) -> bool) -> Result<(), Error> {
        let mut notices = self.notices.load()?;
        let mut changed = false;
        for notice in &mut notices {
            if !notice.resolved && resolved(notice) {
                notice.resolved = true;
                changed = true;
            }
        }
        // 片付けに失敗して計画書が進んでも、次の回や起動し直したあとにやり直せるよう、先に解消済みと記録する。
        if changed {
            self.notices.save(&notices)?;
        }
        self.tidy_notices()
    }

    /// 解消済みの知らせを、今の作業計画書の進行とは独立して片付ける。
    fn tidy_notices(&mut self) -> Result<(), Error> {
        let configured = self.queue_place().map(|(_, location)| location);
        let keep = self.start.root.join(NOTICE_KEPT_DIR);
        for notice in self.notices.load()?.iter().filter(|notice| notice.resolved) {
            if !self.queue_due() {
                return Ok(());
            }
            let cleared =
                recorded_location(&notice.item, configured.as_ref()).and_then(|location| {
                    let store = self.start.queues.open(&location);
                    clear(store.as_ref(), &notice.item, &keep)
                });
            if self
                .queue_result("人間キューの知らせの項目を片付けられません", cleared)?
                .is_none()
            {
                return Ok(());
            }
            let mut notices = self.notices.load()?;
            notices.retain(|current| current != notice);
            self.notices.save(&notices)?;
            self.say(&format!(
                "待ちが解けたため、人間キューの知らせの項目「{}」を片付けました",
                notice.item.title
            ))?;
        }
        Ok(())
    }
}

/// 項目を積んだ置き場を返す。古い記録は、ページのURLが設定と一致するときだけ現在の置き場を使う。
fn recorded_location(
    item: &QueuedItem,
    configured: Option<&Location>,
) -> Result<Location, ItemError> {
    let location = match &item.location {
        Some(location) => Location::from(location),
        None if item.page.is_some() => configured
            .cloned()
            .ok_or_else(|| ItemError::Location(item.title.clone()))?,
        None => return Err(ItemError::Location(item.title.clone())),
    };
    if item
        .page
        .as_ref()
        .is_some_and(|page| page.url != location.edit_url(page.number))
    {
        return Err(ItemError::Location(item.title.clone()));
    }
    Ok(location)
}

/// タイトルtitle・本文bodyの項目を置き場storeに積み、ページ番号を返す。
///
/// 前の回に作成を送った記録があれば (recorded)、作れていたかもしれないため、送り直す前に一覧で探す。
fn put(store: &dyn Store, recorded: bool, title: &str, body: &str) -> Result<u64, ItemError> {
    if recorded && let Some(number) = store.find(title)? {
        return Ok(number);
    }
    Ok(store.create(title, body)?)
}

/// 待ちが解けた項目itemを置き場storeから片付ける。
///
/// 下書きがあれば、人間に何を尋ねたかを後から調べられるよう、本文をディレクトリkeepに残してから削除する。
/// 公開されたか、人間が片付けたあとなら、何もしない。
fn clear(store: &dyn Store, item: &QueuedItem, keep: &Path) -> Result<(), ItemError> {
    let number = match &item.page {
        Some(page) => page.number,
        None => match store.find(&item.title)? {
            Some(number) => number,
            // 作られていなければ、片付けるものが無い。
            None => return Ok(()),
        },
    };
    if let Status::Drafted { body, etag, .. } = store.read(number)? {
        let path = keep.join(format!("{number}.md"));
        fs::create_dir_all(keep)
            .and_then(|()| fs::write(&path, body))
            .map_err(|source| ItemError::Keep { path, source })?;
        store.delete(number, &etag)?;
    }
    Ok(())
}
