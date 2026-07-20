"""pick.py: 選定応答のパースと後処理（SPEC §6）"""

import json

import pytest

from mekiki import pick
from mekiki.claude import UsageTracker
from mekiki.config import Config
from mekiki.feeds import Article
from mekiki.pick import parse_index_array, pick_articles, strip_code_fence


def make_config(**kw):
    defaults = dict(
        name="test", opml_path="x.opml", webhook_env="X",
        select_criteria="- 面白いもの", pick_count=3,
    )
    defaults.update(kw)
    return Config(**defaults)


def make_articles(n):
    return [
        Article(
            title=f"テスト記事タイトル その{i}",
            link=f"https://ex.com/{i}",
            pub_date=None, category="カテゴリ", feed_name="フィード",
        )
        for i in range(n)
    ]


class TestStripCodeFence:
    def test_plain(self):
        assert strip_code_fence("[0, 1]") == "[0, 1]"

    def test_fenced(self):
        assert strip_code_fence("```json\n[0, 1]\n```") == "[0, 1]"

    def test_fenced_no_lang(self):
        assert strip_code_fence("```\n[0, 1]\n```") == "[0, 1]"


class TestParseIndexArray:
    def test_basic(self):
        assert parse_index_array("[0, 5, 2]", 10) == [0, 5, 2]

    def test_out_of_range_ignored(self):
        assert parse_index_array("[0, 99, 2, -1]", 3) == [0, 2]

    def test_non_int_ignored(self):
        assert parse_index_array('[0, "x", 1.5, true, 2]', 3) == [0, 2]

    def test_fenced_json(self):
        assert parse_index_array("```json\n[1, 2]\n```", 5) == [1, 2]

    def test_leading_prose_salvaged(self):
        assert parse_index_array("選定結果: [1, 2] です", 5) == [1, 2]

    def test_invalid_raises(self):
        with pytest.raises((ValueError, json.JSONDecodeError)):
            parse_index_array("配列がありません", 5)

    def test_non_array_raises(self):
        with pytest.raises(ValueError):
            parse_index_array('{"index": 0}', 5)


class TestPickArticles:
    def test_picked_by_claude_indexes(self, monkeypatch):
        monkeypatch.setattr(pick, "run_claude", lambda *a, **k: ("[3, 1, 4]", {}))
        articles = make_articles(6)
        picked, fell_back = pick_articles(articles, make_config(), UsageTracker())
        assert not fell_back
        assert [a.link for a in picked] == [
            "https://ex.com/3", "https://ex.com/1", "https://ex.com/4",
        ]

    def test_shortfall_filled_from_head(self, monkeypatch):
        monkeypatch.setattr(pick, "run_claude", lambda *a, **k: ("[4]", {}))
        picked, fell_back = pick_articles(make_articles(6), make_config(), UsageTracker())
        assert not fell_back
        assert [a.link for a in picked] == [
            "https://ex.com/4", "https://ex.com/0", "https://ex.com/1",
        ]

    def test_duplicate_links_deduped(self, monkeypatch):
        monkeypatch.setattr(pick, "run_claude", lambda *a, **k: ("[0, 1, 0, 2]", {}))
        articles = make_articles(4)
        articles[1].link = "https://ex.com/0"  # index 0とlink重複
        articles[1].title = "リンクが重複している別記事"
        picked, _ = pick_articles(articles, make_config(), UsageTracker())
        assert [a.link for a in picked] == [
            "https://ex.com/0", "https://ex.com/2", "https://ex.com/3",
        ]

    def test_claude_failure_falls_back_to_head(self, monkeypatch):
        def boom(*a, **k):
            raise RuntimeError("boom")

        monkeypatch.setattr(pick, "run_claude", boom)
        picked, fell_back = pick_articles(make_articles(6), make_config(), UsageTracker())
        assert fell_back
        assert [a.link for a in picked] == [
            "https://ex.com/0", "https://ex.com/1", "https://ex.com/2",
        ]

    def test_prescore_reduces_candidates(self, monkeypatch):
        # select_max_articles=4超過時、スコア上位だけが選定に渡る
        from mekiki.config import Limits

        cfg = make_config(pick_count=2)
        cfg.limits = Limits(select_max_articles=4, score_chunk_size=3)
        articles = make_articles(6)

        def fake_run_claude(prompt, **kw):
            if "スコアリング" in prompt:
                # チャンク内index。後方の記事ほど高得点にする
                lines = [l for l in prompt.splitlines() if l.startswith("[")]
                n = len(lines)
                return (
                    json.dumps([{"index": i, "score": i * 10} for i in range(n)]),
                    {},
                )
            return ("[0, 1]", {})

        monkeypatch.setattr(pick, "run_claude", fake_run_claude)
        picked, fell_back = pick_articles(articles, cfg, UsageTracker())
        assert not fell_back
        # 各チャンク(0,1,2)(3,4,5)の後方2件=1,2,4,5が候補になり、その先頭2件が選ばれる
        assert [a.link for a in picked] == ["https://ex.com/1", "https://ex.com/2"]
