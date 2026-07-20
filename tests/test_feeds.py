"""feeds.py: URL正規化・dedup・投稿済み除外・キャッシュ（SPEC §5.3〜§5.5）"""

from datetime import datetime, timedelta, timezone

import pytest

from mekiki.feeds import (
    Article,
    Cache,
    CacheLockedError,
    dedup,
    exclude_posted,
    filter_recent,
    normalize_url,
)


def make_article(title="十分に長い記事タイトルです", link="https://ex.com/a", **kw):
    defaults = dict(pub_date=None, category="カテゴリ", feed_name="フィード")
    defaults.update(kw)
    return Article(title=title, link=link, **defaults)


class TestNormalizeUrl:
    def test_tracking_params_removed(self):
        assert (
            normalize_url("https://ex.com/p?utm_source=x&utm_medium=y&id=3")
            == "https://ex.com/p?id=3"
        )
        assert normalize_url("https://ex.com/p?fbclid=x") == "https://ex.com/p"
        assert normalize_url("https://ex.com/p?gclid=x&ref=t&source=s") == "https://ex.com/p"

    def test_fragment_removed(self):
        assert normalize_url("https://ex.com/p#section") == "https://ex.com/p"

    def test_trailing_slash_unified(self):
        assert normalize_url("https://ex.com/p/") == normalize_url("https://ex.com/p")
        assert normalize_url("https://ex.com/") == normalize_url("https://ex.com")

    def test_host_lowercased(self):
        assert normalize_url("https://EX.com/P") == "https://ex.com/P"

    def test_other_params_preserved(self):
        assert normalize_url("https://ex.com/p?b=2&a=1") == "https://ex.com/p?b=2&a=1"


class TestDedup:
    def test_same_normalized_url_merged_first_kept(self):
        a1 = make_article(title="最初に出現した記事のタイトル", link="https://ex.com/a?utm_source=rss")
        a2 = make_article(title="後から出現した記事のタイトル", link="https://ex.com/a")
        result = dedup([a1, a2])
        assert result == [a1]

    def test_same_title_merged(self):
        a1 = make_article(title="同一タイトルの配信記事です", link="https://site1.com/a")
        a2 = make_article(title="同一タイトルの配信記事です", link="https://site2.com/b")
        assert dedup([a1, a2]) == [a1]

    def test_short_title_not_merged(self):
        # 10文字未満の定型タイトルはタイトル統合の対象外（SPEC §5.4）
        a1 = make_article(title="今週の話題", link="https://site1.com/a")
        a2 = make_article(title="今週の話題", link="https://site2.com/b")
        assert dedup([a1, a2]) == [a1, a2]

    def test_distinct_articles_kept(self):
        a1 = make_article(title="一つ目の記事のタイトルです", link="https://ex.com/1")
        a2 = make_article(title="二つ目の記事のタイトルです", link="https://ex.com/2")
        assert dedup([a1, a2]) == [a1, a2]


class TestExcludePosted:
    def test_posted_excluded_by_normalized_url(self):
        a1 = make_article(link="https://ex.com/a?utm_source=feed")
        a2 = make_article(title="投稿されていない記事のタイトル", link="https://ex.com/b")
        posted = {normalize_url("https://ex.com/a"): "2026-07-11T00:00:00+00:00"}
        assert exclude_posted([a1, a2], posted) == [a2]


class TestFilterRecent:
    def test_old_excluded_recent_kept_undated_kept(self):
        now = datetime(2026, 7, 12, tzinfo=timezone.utc)
        recent = make_article(link="https://ex.com/1", pub_date=now - timedelta(hours=3))
        old = make_article(link="https://ex.com/2", pub_date=now - timedelta(hours=30))
        undated = make_article(link="https://ex.com/3", pub_date=None)
        assert filter_recent([recent, old, undated], 24, now) == [recent, undated]


class TestCache:
    def test_save_and_reload_roundtrip(self, tmp_path):
        path = tmp_path / "cache.json"
        now = datetime(2026, 7, 12, tzinfo=timezone.utc)
        c = Cache(path).open()
        c.update_feed_success(
            "https://ex.com/feed", '"etag"', "Sat, 11 Jul 2026 00:00:00 GMT",
            [make_article()], now,
        )
        c.record_posted(["https://ex.com/a"], now)
        c.save()
        c.close()

        c2 = Cache(path).open()
        assert c2.feed("https://ex.com/feed")["etag"] == '"etag"'
        assert c2.cached_articles("https://ex.com/feed")[0].title == "十分に長い記事タイトルです"
        assert "https://ex.com/a" in c2.posted()
        c2.close()

    def test_prune_posted(self, tmp_path):
        now = datetime(2026, 7, 12, tzinfo=timezone.utc)
        c = Cache(tmp_path / "cache.json").open()
        c.record_posted(["https://ex.com/old"], now - timedelta(days=20))
        c.record_posted(["https://ex.com/new"], now - timedelta(days=3))
        c.prune_posted(14, now)
        assert list(c.posted()) == ["https://ex.com/new"]
        c.close()

    def test_failure_count_and_reset(self, tmp_path):
        now = datetime(2026, 7, 12, tzinfo=timezone.utc)
        c = Cache(tmp_path / "cache.json").open()
        c.update_feed_failure("https://ex.com/feed", now)
        c.update_feed_failure("https://ex.com/feed", now)
        assert c.feed("https://ex.com/feed")["failures"] == 2
        c.reset_failures()
        assert c.feed("https://ex.com/feed")["failures"] == 0
        c.close()

    def test_lock_prevents_concurrent_open(self, tmp_path):
        path = tmp_path / "cache.json"
        c1 = Cache(path).open()
        with pytest.raises(CacheLockedError):
            Cache(path).open()
        c1.close()

    def test_corrupt_cache_starts_fresh(self, tmp_path):
        path = tmp_path / "cache.json"
        path.write_text("{broken json", encoding="utf-8")
        c = Cache(path).open()
        assert c.data == {"feeds": {}, "posted": {}}
        c.close()
