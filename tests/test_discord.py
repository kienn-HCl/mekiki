"""discord.py: embed組み立てと文字数バジェットによるバッチ分割（SPEC §10）"""

from mekiki import discord as discord_mod
from mekiki.discord import (
    EMBED_CHAR_BUDGET,
    DiscordError,
    _with_wait,
    batch_indexes,
    build_embed,
    embed_char_count,
    post_embeds,
    truncate,
)
from mekiki.feeds import Article


class TestWithWait:
    def test_appended_as_query(self):
        assert (
            _with_wait("https://discord.com/api/webhooks/1/token")
            == "https://discord.com/api/webhooks/1/token?wait=true"
        )

    def test_appended_to_existing_query(self):
        assert (
            _with_wait("https://discord.com/api/webhooks/1/token?thread_id=5")
            == "https://discord.com/api/webhooks/1/token?thread_id=5&wait=true"
        )


def make_article(**kw):
    defaults = dict(
        title="記事タイトル", link="https://ex.com/a", pub_date=None,
        category="カテゴリ", feed_name="フィード",
    )
    defaults.update(kw)
    return Article(**defaults)


def make_embed(chars: int) -> dict:
    return {"title": "", "description": "あ" * chars, "footer": {"text": ""}}


class TestTruncate:
    def test_within_limit(self):
        assert truncate("abc", 5) == "abc"

    def test_over_limit_ends_with_ellipsis(self):
        result = truncate("a" * 300, 256)
        assert len(result) == 256
        assert result.endswith("…")


class TestBuildEmbed:
    def test_fields(self):
        article = make_article()
        e = build_embed(article, "要約です", "https://ex.com/img.png", 0x2ECC71)
        assert e["title"] == "記事タイトル"
        assert e["url"] == "https://ex.com/a"
        assert e["description"] == "要約です"
        assert e["color"] == 0x2ECC71
        assert e["footer"]["text"] == "カテゴリ / フィード"
        assert e["image"] == {"url": "https://ex.com/img.png"}

    def test_no_image_key_omitted(self):
        e = build_embed(make_article(), "要約", None, 0)
        assert "image" not in e

    def test_title_truncated(self):
        e = build_embed(make_article(title="あ" * 300), "要約", None, 0)
        assert len(e["title"]) == 256
        assert e["title"].endswith("…")


class TestBatchIndexes:
    def test_max_10_per_message(self):
        embeds = [make_embed(10) for _ in range(25)]
        assert batch_indexes(embeds) == [
            list(range(10)), list(range(10, 20)), list(range(20, 25)),
        ]

    def test_char_budget_splits_before_10(self):
        # 2000字×3件目で6000字 > 5800字予算 → 2件+1件に分割
        embeds = [make_embed(2000) for _ in range(3)]
        assert batch_indexes(embeds) == [[0, 1], [2]]

    def test_exact_budget_fits(self):
        embeds = [make_embed(EMBED_CHAR_BUDGET // 2) for _ in range(2)]
        assert batch_indexes(embeds) == [[0, 1]]

    def test_empty(self):
        assert batch_indexes([]) == []

    def test_char_count_counts_title_desc_footer(self):
        e = {"title": "ab", "description": "cde", "footer": {"text": "fg"}}
        assert embed_char_count(e) == 7


class TestPostEmbedsDegradation:
    def _pairs(self, n=1):
        out = []
        for i in range(n):
            article = make_article(link=f"https://ex.com/{i}")
            out.append(
                (build_embed(article, "要約", "https://ex.com/i.png", 0), article)
            )
        return out

    def _patch(self, monkeypatch, errors):
        """最初のlen(errors)回は指定エラーを投げ、以降成功する_postに差し替える。"""
        calls: list[dict] = []

        def fake_post(url, payload):
            calls.append(payload)
            if len(calls) <= len(errors):
                raise errors[len(calls) - 1]

        monkeypatch.setattr(discord_mod, "_post", fake_post)
        monkeypatch.setattr(discord_mod.time, "sleep", lambda s: None)
        return calls

    def test_5xx_batch_retried_with_same_payload(self, monkeypatch):
        # 5xxはDiscord側の一時障害なので、同一内容（画像込み）で再送する
        calls = self._patch(monkeypatch, [DiscordError("HTTP 500", code=500)])
        posted = post_embeds("https://wh", self._pairs())
        assert len(posted) == 1
        assert calls[1] == calls[0]
        assert "image" in calls[1]["embeds"][0]

    def test_5xx_twice_falls_to_individual_with_images(self, monkeypatch):
        # バッチが2回5xxでも、個別送信では画像を保持する
        errors = [DiscordError("HTTP 500", code=500)] * 2
        calls = self._patch(monkeypatch, errors)
        posted = post_embeds("https://wh", self._pairs(n=3))
        assert len(posted) == 3
        assert [len(c["embeds"]) for c in calls] == [3, 3, 1, 1, 1]
        assert all("image" in c["embeds"][0] for c in calls[2:])

    def test_4xx_goes_straight_to_individual_with_images(self, monkeypatch):
        calls = self._patch(monkeypatch, [DiscordError("HTTP 400", code=400)])
        posted = post_embeds("https://wh", self._pairs(n=2))
        assert len(posted) == 2
        assert len(calls) == 3  # バッチ→個別×2（5xx再送はしない）
        assert "image" in calls[1]["embeds"][0]

    def test_individual_failure_strips_image_of_that_embed_only(self, monkeypatch):
        # バッチ400→個別400→そのembedだけ画像を外して成功
        errors = [DiscordError("HTTP 400", code=400)] * 2
        calls = self._patch(monkeypatch, errors)
        posted = post_embeds("https://wh", self._pairs())
        assert len(posted) == 1
        assert "image" not in calls[2]["embeds"][0]

    def test_all_failures_skip_article(self, monkeypatch):
        errors = [DiscordError("HTTP 400", code=400)] * 3
        calls = self._patch(monkeypatch, errors)
        posted = post_embeds("https://wh", self._pairs())
        assert posted == []
        assert len(calls) == 3  # バッチ→個別→画像除去個別

    def test_embeds_per_message_limits_batch_size(self, monkeypatch):
        calls = self._patch(monkeypatch, [])
        posted = post_embeds("https://wh", self._pairs(n=5), embeds_per_message=2)
        assert len(posted) == 5
        assert [len(c["embeds"]) for c in calls] == [2, 2, 1]
