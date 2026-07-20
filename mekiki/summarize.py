"""要約入力の決定と要約実行・記事画像の決定（SPEC §7・§8）"""

from __future__ import annotations

import logging
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass

from .claude import (
    CONTENT_DISALLOWED_TOOLS,
    FALLBACK_ALLOWED_TOOLS,
    FALLBACK_DISALLOWED_TOOLS,
    FALLBACK_TIMEOUT,
    SUMMARY_TIMEOUT,
    UsageTracker,
    build_fallback_prompt,
    build_summary_prompt,
    run_claude,
)
from .config import Config, Limits
from .extract import extract_og_image, html_to_text, sanitize_image_url
from .feeds import USER_AGENT, Article

log = logging.getLogger(__name__)

PAGE_TIMEOUT = 15
FAILURE_MESSAGE = "（要約の生成に失敗しました。記事本文をご確認ください）"


@dataclass
class SummaryResult:
    article: Article
    summary: str
    ok: bool
    stage: str  # "feed" | "page" | "fallback" | "failed"
    page_html: str | None = None
    page_attempted: bool = False


def fetch_page(url: str, limits: Limits) -> str | None:
    """記事ページのHTMLを取得する。失敗はNone。先頭html_max_chars文字まで。"""
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
    try:
        with urllib.request.urlopen(req, timeout=PAGE_TIMEOUT) as resp:
            # 1文字最大4バイトとしてバイト数を制限してから文字数で切る
            data = resp.read(limits.html_max_chars * 4)
            charset = resp.headers.get_content_charset() or "utf-8"
    except Exception as e:
        log.info("ページ取得失敗: %s: %s", url, e)
        return None
    try:
        return data.decode(charset, errors="replace")[: limits.html_max_chars]
    except LookupError:  # 未知のcharset
        return data.decode("utf-8", errors="replace")[: limits.html_max_chars]


def _summarize_one(article: Article, cfg: Config, usage: UsageTracker) -> SummaryResult:
    """要約入力を フィード本文 → ページテキスト → WebFetch救済 の順で決めて要約する。"""
    lim = cfg.limits
    page_html: str | None = None
    page_attempted = False

    text = html_to_text(article.body) if article.body else ""
    stage = "feed"
    if len(text) < lim.text_min_chars:
        page_attempted = True
        page_html = fetch_page(article.link, lim)
        if page_html:
            text = html_to_text(page_html)
            stage = "page"
    text = text[: lim.text_max_chars]

    if len(text) >= lim.text_min_chars:
        try:
            summary, u = run_claude(
                build_summary_prompt(article.title, text),
                timeout=SUMMARY_TIMEOUT,
                model=cfg.model,
                disallowed_tools=CONTENT_DISALLOWED_TOOLS,
            )
            usage.record("summarize", u)
            summary = summary.strip()
            if summary:
                return SummaryResult(
                    article, summary, True, stage, page_html, page_attempted
                )
            log.warning("要約が空: %s", article.link)
        except Exception as e:
            log.warning("要約失敗: %s: %s", article.link, e)
        return SummaryResult(
            article, FAILURE_MESSAGE, False, "failed", page_html, page_attempted
        )

    # 本文がほぼ取れない → WebFetch救済（タイトルのみ要約もこのプロンプトが兼ねる）
    try:
        summary, u = run_claude(
            build_fallback_prompt(article.title, article.link),
            timeout=FALLBACK_TIMEOUT,
            model=cfg.model,
            allowed_tools=FALLBACK_ALLOWED_TOOLS,
            disallowed_tools=FALLBACK_DISALLOWED_TOOLS,
        )
        usage.record("summarize_fallback", u)
        summary = summary.strip()
        if summary:
            return SummaryResult(
                article, summary, True, "fallback", page_html, page_attempted
            )
        log.warning("救済要約が空: %s", article.link)
    except Exception as e:
        log.warning("救済要約失敗: %s: %s", article.link, e)
    return SummaryResult(
        article, FAILURE_MESSAGE, False, "failed", page_html, page_attempted
    )


def summarize_all(
    picked: list[Article], cfg: Config, usage: UsageTracker
) -> list[SummaryResult]:
    """選定記事をsummary_workers並列で要約する。入力の順序を保つ。"""
    with ThreadPoolExecutor(max_workers=cfg.limits.summary_workers) as pool:
        return list(pool.map(lambda a: _summarize_one(a, cfg, usage), picked))


def resolve_image(result: SummaryResult, cfg: Config) -> str | None:
    """記事画像URLを フィード由来 → og:image の順で決める（SPEC §8）。

    要約時にページを取得済みならそれを再利用する。フィード本文で要約が足りていて
    画像だけ必要な場合は、画像のためだけにページを取得してよい。
    """
    article = result.article
    if article.image:
        sanitized = sanitize_image_url(article.image, base_url=article.link)
        if sanitized:
            return sanitized

    html = result.page_html
    if html is None and not result.page_attempted:
        html = fetch_page(article.link, cfg.limits)
    if html:
        og = extract_og_image(html)
        if og:
            return sanitize_image_url(og, base_url=article.link)
    return None
