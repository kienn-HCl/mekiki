"""OPMLパース・フィード取得・キャッシュ・dedup・投稿済み除外（SPEC §4・§5）"""

from __future__ import annotations

import calendar
import fcntl
import json
import logging
import os
import random
import time
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

import feedparser

from .config import Config

log = logging.getLogger(__name__)

USER_AGENT = "Mozilla/5.0 mekiki/1.0"
FETCH_TIMEOUT = 15
MAX_RETRIES = 4  # 接続エラー・タイムアウト系のみ。HTTPエラー応答はリトライしない

# タイトル完全一致dedupの対象とする最小タイトル長（SPEC §5.4）
TITLE_DEDUP_MIN_CHARS = 10

_TRACKING_PARAMS = frozenset({"fbclid", "gclid", "ref", "source"})


@dataclass
class Feed:
    name: str
    url: str
    category: str


@dataclass
class Article:
    title: str
    link: str
    pub_date: datetime | None
    category: str
    feed_name: str
    body: str = ""
    image: str = ""

    def to_dict(self) -> dict:
        return {
            "title": self.title,
            "link": self.link,
            "pub_date": self.pub_date.isoformat() if self.pub_date else None,
            "category": self.category,
            "feed_name": self.feed_name,
            "body": self.body,
            "image": self.image,
        }

    @classmethod
    def from_dict(cls, d: dict) -> "Article":
        pub = d.get("pub_date")
        return cls(
            title=d.get("title", ""),
            link=d.get("link", ""),
            pub_date=datetime.fromisoformat(pub) if pub else None,
            category=d.get("category", ""),
            feed_name=d.get("feed_name", ""),
            body=d.get("body", ""),
            image=d.get("image", ""),
        )


def parse_opml(path: str | Path) -> list[Feed]:
    """2階層OPML: 第1階層outline=カテゴリ、その子=フィード。xmlUrlのないものは無視。"""
    tree = ET.parse(path)
    body = tree.find("body")
    if body is None:
        return []
    feeds: list[Feed] = []
    for category_node in body.findall("outline"):
        category = category_node.get("text", "")
        for node in category_node.findall("outline"):
            xml_url = node.get("xmlUrl")
            if not xml_url:
                continue
            feeds.append(Feed(name=node.get("text", ""), url=xml_url, category=category))
    return feeds


def normalize_url(url: str) -> str:
    """dedup・投稿済み判定用のURL正規化（SPEC §5.4）。"""
    try:
        parts = urlsplit(url.strip())
    except ValueError:
        return url
    host = parts.netloc.lower()
    path = parts.path.rstrip("/")
    pairs = [
        (k, v)
        for k, v in parse_qsl(parts.query, keep_blank_values=True)
        if not (k.lower().startswith("utm_") or k.lower() in _TRACKING_PARAMS)
    ]
    return urlunsplit((parts.scheme.lower(), host, path, urlencode(pairs), ""))


def dedup(articles: list[Article]) -> list[Article]:
    """正規化URL同一→統合、タイトル完全一致（一定長以上）→統合。最初の出現を残す。"""
    out: list[Article] = []
    seen_urls: set[str] = set()
    for a in articles:
        key = normalize_url(a.link)
        if key in seen_urls:
            continue
        seen_urls.add(key)
        out.append(a)

    result: list[Article] = []
    seen_titles: set[str] = set()
    for a in out:
        title = a.title.strip()
        if len(title) >= TITLE_DEDUP_MIN_CHARS:
            if title in seen_titles:
                continue
            seen_titles.add(title)
        result.append(a)
    return result


def exclude_posted(articles: list[Article], posted: dict[str, str]) -> list[Article]:
    """投稿済みURL履歴に載っている記事を候補から外す（SPEC §5.5）。"""
    return [a for a in articles if normalize_url(a.link) not in posted]


def filter_recent(
    articles: list[Article], hours_back: int, now: datetime | None = None
) -> list[Article]:
    """hours_backより古い記事を除外。publishedのない記事は対象期間内として扱う（SPEC §5.2）。"""
    cutoff = (now or datetime.now(timezone.utc)) - timedelta(hours=hours_back)
    return [a for a in articles if a.pub_date is None or a.pub_date >= cutoff]


class CacheLockedError(Exception):
    pass


class Cache:
    """プロファイルごとのJSONキャッシュ（SPEC §5.3）。

    - 排他ロック（flock・非ブロッキング）。取れなければ多重起動として CacheLockedError
    - 書き込みは一時ファイル + os.replace の原子的置換
    """

    def __init__(self, path: str | Path):
        self.path = Path(path)
        self.data: dict = {"feeds": {}, "posted": {}}
        self._lock_fd: int | None = None

    def open(self) -> "Cache":
        self.path.parent.mkdir(parents=True, exist_ok=True)
        lock_path = self.path.with_name(self.path.name + ".lock")
        fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            os.close(fd)
            raise CacheLockedError(f"キャッシュのロックが取れません: {lock_path}")
        self._lock_fd = fd

        if self.path.is_file():
            try:
                loaded = json.loads(self.path.read_text(encoding="utf-8"))
                if isinstance(loaded, dict):
                    self.data = {
                        "feeds": loaded.get("feeds", {}),
                        "posted": loaded.get("posted", {}),
                    }
            except (json.JSONDecodeError, OSError) as e:
                log.warning("キャッシュの読み込みに失敗（空の状態から再開）: %s", e)
        return self

    def save(self) -> None:
        tmp = self.path.with_name(self.path.name + ".tmp")
        tmp.write_text(
            json.dumps(self.data, ensure_ascii=False, indent=1), encoding="utf-8"
        )
        os.replace(tmp, self.path)

    def close(self) -> None:
        if self._lock_fd is not None:
            os.close(self._lock_fd)
            self._lock_fd = None

    # --- feeds ---

    def feed(self, url: str) -> dict:
        return self.data["feeds"].get(url, {})

    def update_feed_success(
        self,
        url: str,
        etag: str | None,
        last_modified: str | None,
        articles: list[Article],
        now: datetime,
    ) -> None:
        entry = self.data["feeds"].setdefault(url, {})
        entry["etag"] = etag
        entry["last_modified"] = last_modified
        entry["failures"] = 0
        entry["last_attempt"] = now.isoformat()
        entry["last_success"] = now.isoformat()
        entry["articles"] = [a.to_dict() for a in articles]

    def mark_feed_success(self, url: str, now: datetime) -> None:
        """304等でキャッシュ記事をそのまま使う場合（articlesは保持）。"""
        entry = self.data["feeds"].setdefault(url, {})
        entry["failures"] = 0
        entry["last_attempt"] = now.isoformat()
        entry["last_success"] = now.isoformat()

    def update_feed_failure(self, url: str, now: datetime) -> None:
        entry = self.data["feeds"].setdefault(url, {})
        entry["failures"] = entry.get("failures", 0) + 1
        entry["last_attempt"] = now.isoformat()

    def cached_articles(self, url: str) -> list[Article]:
        return [Article.from_dict(d) for d in self.feed(url).get("articles", [])]

    def reset_failures(self) -> None:
        for entry in self.data["feeds"].values():
            entry["failures"] = 0

    # --- posted (SPEC §5.5) ---

    def posted(self) -> dict[str, str]:
        return self.data["posted"]

    def record_posted(self, normalized_urls: list[str], now: datetime) -> None:
        for url in normalized_urls:
            self.data["posted"][url] = now.isoformat()

    def prune_posted(self, retention_days: int, now: datetime) -> None:
        cutoff = now - timedelta(days=retention_days)
        self.data["posted"] = {
            url: ts
            for url, ts in self.data["posted"].items()
            if _parse_iso(ts) is None or _parse_iso(ts) >= cutoff
        }


def _parse_iso(ts: str) -> datetime | None:
    try:
        return datetime.fromisoformat(ts)
    except (ValueError, TypeError):
        return None


@dataclass
class _FetchOutcome:
    feed: Feed
    ok: bool
    articles: list[Article]
    etag: str | None = None
    last_modified: str | None = None
    from_cache: bool = False
    error: str = ""


def _entry_image(entry) -> str:
    for m in entry.get("media_thumbnail") or []:
        if m.get("url"):
            return m["url"]
    for m in entry.get("media_content") or []:
        if m.get("url") and (
            m.get("medium") == "image" or (m.get("type") or "").startswith("image/")
        ):
            return m["url"]
    for e in entry.get("enclosures") or []:
        if (e.get("type") or "").startswith("image/") and e.get("href"):
            return e["href"]
    return ""


def _entry_to_article(entry, feed: Feed) -> Article | None:
    title = (entry.get("title") or "").strip()
    link = (entry.get("link") or "").strip()
    if not title or not link:
        return None
    t = entry.get("published_parsed") or entry.get("updated_parsed")
    pub_date = (
        datetime.fromtimestamp(calendar.timegm(t), tz=timezone.utc) if t else None
    )
    body = ""
    content = entry.get("content")
    if content:
        body = content[0].get("value", "")
    if not body:
        body = entry.get("summary") or ""
    return Article(
        title=title,
        link=link,
        pub_date=pub_date,
        category=feed.category,
        feed_name=feed.name,
        body=body,
        image=_entry_image(entry),
    )


def _fetch_one(feed: Feed, state: dict, articles_per_feed: int) -> _FetchOutcome:
    headers = {"User-Agent": USER_AGENT}
    if state.get("etag"):
        headers["If-None-Match"] = state["etag"]
    if state.get("last_modified"):
        headers["If-Modified-Since"] = state["last_modified"]

    last_err: Exception | None = None
    for attempt in range(1 + MAX_RETRIES):
        req = urllib.request.Request(feed.url, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=FETCH_TIMEOUT) as resp:
                data = resp.read()
                resp_headers = resp.headers
            break
        except urllib.error.HTTPError as e:
            if e.code == 304:
                return _FetchOutcome(feed, ok=True, articles=[], from_cache=True)
            return _FetchOutcome(feed, ok=False, articles=[], error=f"HTTP {e.code}")
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            last_err = e
            if attempt < MAX_RETRIES:
                time.sleep(2**attempt + random.random())
    else:
        return _FetchOutcome(feed, ok=False, articles=[], error=str(last_err))

    parsed = feedparser.parse(data)
    if parsed.get("bozo") and not parsed.entries and not parsed.get("version"):
        return _FetchOutcome(
            feed, ok=False, articles=[],
            error=f"パース失敗: {parsed.get('bozo_exception')}",
        )

    articles = []
    for entry in parsed.entries[:articles_per_feed]:
        a = _entry_to_article(entry, feed)
        if a:
            articles.append(a)
    return _FetchOutcome(
        feed,
        ok=True,
        articles=articles,
        etag=resp_headers.get("ETag"),
        last_modified=resp_headers.get("Last-Modified"),
    )


def fetch_all(
    feeds: list[Feed], cache: Cache, cfg: Config, now: datetime | None = None
) -> tuple[list[Article], list[Feed]]:
    """全フィードを並列取得し、(対象期間内の記事, スキップ中フィード) を返す。

    キャッシュの更新はこのスレッド（メインスレッド）で行う。
    """
    now = now or datetime.now(timezone.utc)
    lim = cfg.limits

    to_fetch: list[Feed] = []
    skipped: list[Feed] = []
    for feed in feeds:
        state = cache.feed(feed.url)
        if state.get("failures", 0) >= lim.max_failures:
            last_attempt = _parse_iso(state.get("last_attempt", ""))
            if last_attempt and now - last_attempt < timedelta(
                hours=lim.failure_retry_hours
            ):
                skipped.append(feed)
                continue
            log.info("スキップ中フィードを再試行: %s", feed.name)
        to_fetch.append(feed)

    results: list[_FetchOutcome] = []
    with ThreadPoolExecutor(max_workers=lim.max_workers) as pool:
        futures = [
            pool.submit(_fetch_one, feed, cache.feed(feed.url), lim.articles_per_feed)
            for feed in to_fetch
        ]
        for fut in futures:
            results.append(fut.result())

    all_articles: list[Article] = []
    for r in results:
        if not r.ok:
            cache.update_feed_failure(r.feed.url, now)
            failures = cache.feed(r.feed.url).get("failures", 0)
            log.warning(
                "フィード取得失敗 (%d/%d): %s: %s",
                failures, lim.max_failures, r.feed.name, r.error,
            )
            continue
        if r.from_cache:
            cache.mark_feed_success(r.feed.url, now)
            articles = cache.cached_articles(r.feed.url)
        else:
            cache.update_feed_success(
                r.feed.url, r.etag, r.last_modified, r.articles, now
            )
            articles = r.articles
        all_articles.extend(filter_recent(articles, cfg.hours_back, now))

    return all_articles, skipped
