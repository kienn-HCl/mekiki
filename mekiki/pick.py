"""記事選定＋チャンクスコアリング（SPEC §6）"""

from __future__ import annotations

import json
import logging
from concurrent.futures import ThreadPoolExecutor

from .claude import (
    CONTENT_DISALLOWED_TOOLS,
    SCORE_TIMEOUT,
    SELECT_TIMEOUT,
    UsageTracker,
    build_score_prompt,
    build_select_prompt,
    run_claude,
)
from .config import Config
from .feeds import Article, normalize_url

log = logging.getLogger(__name__)

DEFAULT_SCORE = 50.0  # チャンクスコアリング失敗時のスコア


def strip_code_fence(text: str) -> str:
    """応答にコードブロック記号（```）が付いていたら剥がす。"""
    t = text.strip()
    if not t.startswith("```"):
        return t
    lines = t.splitlines()
    lines = lines[1:]
    if lines and lines[-1].strip() == "```":
        lines = lines[:-1]
    return "\n".join(lines).strip()


def _loads_lenient(text: str):
    """JSONパース。失敗したら最初の [ から最後の ] までを切り出して再試行。"""
    t = strip_code_fence(text)
    try:
        return json.loads(t)
    except json.JSONDecodeError:
        start, end = t.find("["), t.rfind("]")
        if start != -1 and end > start:
            return json.loads(t[start : end + 1])
        raise


def parse_index_array(text: str, n_candidates: int) -> list[int]:
    """選定応答をパースし、範囲内の整数indexだけを返す（範囲外・非整数は無視）。"""
    data = _loads_lenient(text)
    if not isinstance(data, list):
        raise ValueError(f"JSON配列ではありません: {type(data).__name__}")
    return [
        i
        for i in data
        if isinstance(i, int) and not isinstance(i, bool) and 0 <= i < n_candidates
    ]


def _article_lines(articles: list[Article]) -> str:
    return "\n".join(
        f"[{i}] [{a.category} / {a.feed_name}] {a.title}"
        for i, a in enumerate(articles)
    )


def _score_chunk(
    offset: int, chunk: list[Article], cfg: Config, usage: UsageTracker
) -> dict[int, float]:
    """1チャンクをスコアリングし {グローバルindex: score} を返す。失敗は空dict。"""
    prompt = build_score_prompt(
        _article_lines(chunk), cfg.select_criteria, cfg.user_memory
    )
    try:
        text, u = run_claude(
            prompt,
            timeout=SCORE_TIMEOUT,
            model=cfg.model,
            disallowed_tools=CONTENT_DISALLOWED_TOOLS,
        )
        usage.record("score", u)
        data = _loads_lenient(text)
        if not isinstance(data, list):
            raise ValueError("JSON配列ではありません")
    except Exception as e:
        log.warning(
            "スコアリングチャンク失敗（全記事%d点として続行）: %s", int(DEFAULT_SCORE), e
        )
        return {}
    scores: dict[int, float] = {}
    for item in data:
        if not isinstance(item, dict):
            continue
        idx, score = item.get("index"), item.get("score")
        if (
            isinstance(idx, int)
            and not isinstance(idx, bool)
            and 0 <= idx < len(chunk)
            and isinstance(score, (int, float))
            and not isinstance(score, bool)
        ):
            scores[offset + idx] = float(score)
    return scores


def _prescore(articles: list[Article], cfg: Config, usage: UsageTracker) -> list[Article]:
    """select_max_articles超過時の事前絞り込み。スコア降順の上位を元の順序で返す。"""
    lim = cfg.limits
    log.info(
        "記事%d件 > select_max_articles=%d のためチャンクスコアリングを実行",
        len(articles), lim.select_max_articles,
    )
    chunks = [
        (offset, articles[offset : offset + lim.score_chunk_size])
        for offset in range(0, len(articles), lim.score_chunk_size)
    ]
    scores = [DEFAULT_SCORE] * len(articles)
    with ThreadPoolExecutor(max_workers=lim.score_workers) as pool:
        futures = [
            pool.submit(_score_chunk, offset, chunk, cfg, usage)
            for offset, chunk in chunks
        ]
        for fut in futures:
            for idx, score in fut.result().items():
                scores[idx] = score
    top = sorted(
        sorted(range(len(articles)), key=lambda i: -scores[i])[: lim.select_max_articles]
    )
    return [articles[i] for i in top]


def pick_articles(
    articles: list[Article], cfg: Config, usage: UsageTracker
) -> tuple[list[Article], bool]:
    """記事を選定する。戻り値は (選定記事, フォールバックしたか)。

    選定失敗時はdedup済みリストの先頭からpick_count件を機械的に選出（SPEC §11）。
    """
    candidates = articles
    if len(candidates) > cfg.limits.select_max_articles:
        candidates = _prescore(candidates, cfg, usage)

    prompt = build_select_prompt(
        _article_lines(candidates), cfg.pick_count, cfg.select_criteria, cfg.user_memory
    )
    try:
        text, u = run_claude(
            prompt,
            timeout=SELECT_TIMEOUT,
            model=cfg.model,
            disallowed_tools=CONTENT_DISALLOWED_TOOLS,
        )
        usage.record("select", u)
        indexes = parse_index_array(text, len(candidates))
    except Exception as e:
        log.warning("選定に失敗（先頭%d件で継続）: %s", cfg.pick_count, e)
        return candidates[: cfg.pick_count], True

    picked: list[Article] = []
    seen_links: set[str] = set()
    for i in indexes:
        a = candidates[i]
        key = normalize_url(a.link)
        if key in seen_links:
            continue
        seen_links.add(key)
        picked.append(a)
        if len(picked) >= cfg.pick_count:
            break

    # 件数不足なら候補リストの先頭から補充（SPEC §6）
    if len(picked) < cfg.pick_count:
        for a in candidates:
            if len(picked) >= cfg.pick_count:
                break
            key = normalize_url(a.link)
            if key in seen_links:
                continue
            seen_links.add(key)
            picked.append(a)

    return picked, False
