#!/usr/bin/env python3
"""各段階を個別に実行する手動テスト（SPEC §14.2）。

usage: test_parts.py <プロファイルTOML> <コマンド>

フィード取得系のコマンドはキャッシュパスをテスト用（.test付き）に差し替え、
本番キャッシュを汚さない。
"""

from __future__ import annotations

import logging
import os
import sys
from collections import Counter
from datetime import datetime, timezone

from mekiki.claude import UsageTracker
from mekiki.config import Config, load_config
from mekiki.discord import build_embed, post_embeds, send_content
from mekiki.feeds import Article, Cache, dedup, fetch_all, normalize_url, parse_opml
from mekiki.pick import pick_articles
from mekiki.summarize import _summarize_one, resolve_image


def _test_cache(cfg: Config) -> Cache:
    cfg.cache_path += ".test"
    return Cache(cfg.cache_path).open()


def _dummy(title, link, body="", image="", category="カテゴリ", feed="フィード"):
    return Article(
        title=title, link=link, pub_date=datetime.now(timezone.utc),
        category=category, feed_name=feed, body=body, image=image,
    )


def cmd_opml(cfg: Config) -> None:
    feeds = parse_opml(cfg.opml_path)
    by_category = Counter(f.category for f in feeds)
    print(f"フィード{len(feeds)}件 / カテゴリ{len(by_category)}件")
    for category, count in by_category.items():
        print(f"  {category}: {count}件")
    for f in feeds[:5]:
        print(f"  例: [{f.category}] {f.name} {f.url}")


def cmd_fetch(cfg: Config) -> None:
    feeds = parse_opml(cfg.opml_path)[:3]
    cache = _test_cache(cfg)
    try:
        articles, skipped = fetch_all(feeds, cache, cfg)
        cache.save()
        print(f"取得記事{len(articles)}件（対象期間内） / スキップ{len(skipped)}件")
        for f in feeds:
            state = cache.feed(f.url)
            print(
                f"  {f.name}: 記事{len(state.get('articles', []))}件"
                f" failures={state.get('failures')} etag={state.get('etag')!r}"
            )
        for a in articles[:5]:
            print(f"  例: {a.title} ({a.pub_date}) image={'あり' if a.image else 'なし'}")
        print(f"テスト用キャッシュ: {cfg.cache_path}")
    finally:
        cache.close()


def cmd_fetch_all(cfg: Config) -> None:
    feeds = parse_opml(cfg.opml_path)
    cache = _test_cache(cfg)
    try:
        articles, skipped = fetch_all(feeds, cache, cfg)
        cache.save()
        print(f"フィード{len(feeds)}件 → 対象期間内の記事{len(articles)}件")
        print(f"スキップ中フィード{len(skipped)}件: {[f.name for f in skipped]}")
        for category, count in Counter(a.category for a in articles).most_common():
            print(f"  {category}: {count}件")
        print(f"dedup後: {len(dedup(articles))}件")
    finally:
        cache.close()


def cmd_dedup(cfg: Config) -> None:
    articles = [
        _dummy("URLがトラッキング付きで重複している記事", "https://ex.com/a?utm_source=rss"),
        _dummy("URLがトラッキング付きで重複している記事(2)", "https://ex.com/a"),
        _dummy("タイトルが完全一致する別URLの記事", "https://site1.com/x"),
        _dummy("タイトルが完全一致する別URLの記事", "https://site2.com/y"),
        _dummy("今週の話題", "https://site1.com/w"),  # 短いタイトルは統合されない
        _dummy("今週の話題", "https://site2.com/w"),
        _dummy("独立した普通の記事のタイトル", "https://ex.com/b"),
    ]
    result = dedup(articles)
    print(f"入力{len(articles)}件 → dedup後{len(result)}件（期待: 5件）")
    for a in result:
        print(f"  {a.title} {a.link}")


def cmd_pick(cfg: Config) -> None:
    titles = [
        "Linuxカーネル6.16でRust製ドライバが安定版入り",
        "React 20のリリースノートを読む",
        "PostgreSQL 18の非同期I/O改善を検証した",
        "芸能人の熱愛スクープまとめ",
        "OpenSSHに致命的なRCE脆弱性、即時アップデートを推奨",
        "今日のランチはカレーでした",
        "Kubernetes 1.34のGateway API GA解説",
        "プログラミング言語Zig 1.0がリリース",
        "占いで見る今週の運勢",
        "Cloudflareの大規模障害の原因分析レポート",
    ]
    cfg.pick_count = 3
    articles = [
        _dummy(t, f"https://ex.com/{i}", category=f"cat{i % 3}", feed=f"feed{i}")
        for i, t in enumerate(titles)
    ]
    usage = UsageTracker()
    picked, fell_back = pick_articles(articles, cfg, usage)
    print(f"選定{len(picked)}件（フォールバック: {fell_back}）")
    for a in picked:
        idx = articles.index(a)
        print(f"  [{idx}] {a.title}")
    print("※ 上のindexとタイトルの対応が入力と一致していることを確認")
    print("\n".join(usage.summary_lines()))


def cmd_summarize(cfg: Config) -> None:
    long_body = "<p>" + "これはフィード本文が十分に長いケースのテスト用の本文です。" * 20 + "</p>"
    cases = [
        ("フィード本文あり", _dummy(
            "フィード本文で足りる記事", "https://ex.invalid/feed-body", body=long_body)),
        ("ページ取得成功", _dummy(
            "RSS - Wikipedia", "https://ja.wikipedia.org/wiki/RSS")),
        ("403サイト", _dummy(
            "Bloombergの記事（bot遮断される想定）",
            "https://www.bloomberg.co.jp/news/articles/2026-07-01/test")),
        ("存在しないURL", _dummy(
            "存在しないドメインの記事", "https://nonexistent-domain.invalid/article")),
    ]
    usage = UsageTracker()
    for label, article in cases:
        print(f"--- {label}: {article.link}")
        result = _summarize_one(article, cfg, usage)
        image = resolve_image(result, cfg)
        print(f"  stage={result.stage} ok={result.ok}")
        print(f"  要約: {result.summary}")
        print(f"  画像: {image}")
    print("\n".join(usage.summary_lines()))


def _webhook(cfg: Config) -> str:
    url = os.environ.get(cfg.webhook_env)
    if not url:
        print(f"環境変数 {cfg.webhook_env} が未設定です", file=sys.stderr)
        sys.exit(1)
    return url


def cmd_discord(cfg: Config) -> None:
    webhook = _webhook(cfg)
    send_content(webhook, f"## mekiki test（{cfg.name}）\nembed送信テスト")
    article = _dummy(
        "テスト記事: mekiki疎通確認", "https://example.com/",
        category="テスト", feed="test_parts",
    )
    embed = build_embed(article, "これはtest_parts.pyからの疎通確認です。", None, cfg.embed_color)
    posted = post_embeds(webhook, [(embed, article)])
    print(f"送信完了: {len(posted)}/1件")


def cmd_discord_raw(cfg: Config) -> None:
    send_content(_webhook(cfg), "mekiki test_parts.py raw message")
    print("送信完了")


def cmd_cache(cfg: Config) -> None:
    cache = Cache(cfg.cache_path)
    if not cache.path.is_file():
        print(f"キャッシュなし: {cfg.cache_path}")
        return
    import json

    data = json.loads(cache.path.read_text(encoding="utf-8"))
    feeds_data = data.get("feeds", {})
    print(f"{cfg.cache_path}: フィード{len(feeds_data)}件 / 投稿済み{len(data.get('posted', {}))}件")
    failing = 0
    for url, st in feeds_data.items():
        if st.get("failures", 0) > 0:
            failing += 1
            print(f"  failures={st['failures']} last_success={st.get('last_success')} {url}")
    print(f"失敗カウント>0のフィード: {failing}件")


def cmd_cache_reset(cfg: Config) -> None:
    cache = Cache(cfg.cache_path).open()
    try:
        cache.reset_failures()
        cache.save()
        print("全フィードの失敗カウントをリセットしました")
    finally:
        cache.close()


COMMANDS = {
    "opml": cmd_opml,
    "fetch": cmd_fetch,
    "fetch_all": cmd_fetch_all,
    "dedup": cmd_dedup,
    "pick": cmd_pick,
    "summarize": cmd_summarize,
    "discord": cmd_discord,
    "discord_raw": cmd_discord_raw,
    "cache": cmd_cache,
    "cache_reset": cmd_cache_reset,
}


def main() -> int:
    logging.basicConfig(
        level=logging.INFO, stream=sys.stderr,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    )
    if len(sys.argv) != 3 or sys.argv[2] not in COMMANDS:
        print(f"usage: test_parts.py <プロファイル> <{'|'.join(COMMANDS)}>", file=sys.stderr)
        return 2
    cfg = load_config(sys.argv[1])
    COMMANDS[sys.argv[2]](cfg)
    return 0


if __name__ == "__main__":
    sys.exit(main())
