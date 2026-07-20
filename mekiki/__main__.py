"""エントリポイント: プロファイル読込→パイプライン実行（SPEC §1・§11）"""

from __future__ import annotations

import logging
import os
import sys
from datetime import datetime, timedelta, timezone

from .claude import UsageTracker
from .config import Config, ConfigError, load_config
from .discord import build_embed, notify_failure, post_embeds, send_content
from .feeds import (
    Cache,
    CacheLockedError,
    dedup,
    exclude_posted,
    fetch_all,
    normalize_url,
    parse_opml,
)
from .pick import pick_articles
from .summarize import SummaryResult, resolve_image, summarize_all

log = logging.getLogger("mekiki")

JST = timezone(timedelta(hours=9))


def main(argv: list[str] | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    logging.basicConfig(
        level=logging.INFO,
        stream=sys.stderr,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    )
    if len(args) != 1:
        print("usage: mekiki <プロファイルTOML>", file=sys.stderr)
        return 2

    try:
        cfg = load_config(args[0])
    except ConfigError as e:
        log.error("プロファイル読み込み失敗: %s", e)
        return 2

    webhook = os.environ.get(cfg.webhook_env) or None
    try:
        return run(cfg, webhook)
    except CacheLockedError as e:
        reason = f"多重起動の可能性: {e}"
        log.error(reason)
        notify_failure(webhook, cfg.name, reason)
        return 1
    except Exception as e:
        log.exception("実行失敗")
        notify_failure(webhook, cfg.name, f"{type(e).__name__}: {str(e)[:200]}")
        return 1


def run(cfg: Config, webhook: str | None) -> int:
    now = datetime.now(timezone.utc)
    cache = Cache(cfg.cache_path).open()
    try:
        cache.prune_posted(cfg.limits.posted_retention_days, now)

        feeds = parse_opml(cfg.opml_path)
        log.info("[%s] フィード%d件を取得開始", cfg.name, len(feeds))
        articles, skipped_feeds = fetch_all(feeds, cache, cfg, now)
        cache.save()  # ETag・失敗カウントを確定
        log.info(
            "対象期間内の記事%d件（スキップ中フィード%d件）",
            len(articles), len(skipped_feeds),
        )

        candidates = exclude_posted(dedup(articles), cache.posted())
        log.info("dedup・投稿済み除外後の候補%d件", len(candidates))
        if not candidates:
            log.info("対象記事0件のため正常終了（Discordには何も送らない）")
            return 0

        usage = UsageTracker()
        picked, select_fell_back = pick_articles(candidates, cfg, usage)
        log.info("選定%d件（フォールバック: %s）", len(picked), select_fell_back)

        results = summarize_all(picked, cfg, usage)
        ok_count = sum(1 for r in results if r.ok)
        log.info("要約成功%d/%d件", ok_count, len(results))

        # サーキットブレーカー（SPEC §11）: 選定も要約も全滅ならClaude全体障害とみなす
        if select_fell_back and ok_count == 0:
            raise RuntimeError(
                "選定がフォールバックし要約成功0件（claude CLIの全体障害の疑い）"
            )

        pairs = [
            (build_embed(r.article, r.summary, resolve_image(r, cfg), cfg.embed_color),
             r.article)
            for r in results
        ]

        for line in usage.summary_lines():
            print(line)
        usage_content = usage.discord_line()
        if skipped_feeds:
            names = "、".join(f.name for f in skipped_feeds)
            usage_content += f"\n-# スキップ中フィード{len(skipped_feeds)}件: {names}"

        if not webhook:
            _print_dry_run(cfg, results, usage_content)
            return 0

        header = (
            f"## {cfg.header_title}\n"
            f"{now.astimezone(JST):%Y/%m/%d %H:%M} JST — {len(results)}件"
        )
        # ヘッダーメッセージすら送れない場合は非0終了（SPEC §11）
        send_content(webhook, header)

        posted = post_embeds(webhook, pairs, cfg.limits.embeds_per_message)
        log.info("Discord投稿完了: %d/%d件", len(posted), len(pairs))
        cache.record_posted([normalize_url(a.link) for a in posted], now)
        cache.save()

        try:
            send_content(webhook, usage_content)
        except Exception as e:
            log.warning("使用量サマリの送信失敗（記事投稿は完了済み）: %s", e)
        return 0
    finally:
        cache.close()


def _print_dry_run(
    cfg: Config, results: list[SummaryResult], usage_content: str
) -> None:
    """webhook未設定時: 送信せず記事一覧を標準出力に表示（SPEC §10）。"""
    print(f"=== ドライラン（環境変数 {cfg.webhook_env} 未設定）===")
    for r in results:
        print(f"- [{r.article.category} / {r.article.feed_name}] {r.article.title}")
        print(f"  {r.article.link}")
        print(f"  ({r.stage}) {r.summary}")
    print(usage_content)


if __name__ == "__main__":
    sys.exit(main())
