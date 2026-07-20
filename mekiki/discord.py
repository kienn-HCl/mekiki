"""Discord Webhook投稿: embed組み立て・バッチ送信・レートリミット・縮退（SPEC §10）"""

from __future__ import annotations

import json
import logging
import time
import urllib.error
import urllib.request

from .feeds import Article

log = logging.getLogger(__name__)

USER_AGENT = "mekiki/1.0"
POST_TIMEOUT = 15
MAX_EMBEDS_PER_MESSAGE = 10
# Discordは1メッセージ内の全embedの文字数合計を6000字に制限している。安全マージン込みの予算
EMBED_CHAR_BUDGET = 5800
TITLE_LIMIT = 256
DESCRIPTION_LIMIT = 4096
CONTENT_LIMIT = 2000
MAX_ATTEMPTS = 3  # 429時の再試行を含む1リクエストあたりの最大送信回数


class DiscordError(Exception):
    def __init__(self, message: str, code: int | None = None):
        super().__init__(message)
        self.code = code


def truncate(text: str, limit: int) -> str:
    if len(text) <= limit:
        return text
    return text[: limit - 1] + "…"


def build_embed(
    article: Article, summary: str, image_url: str | None, color: int
) -> dict:
    embed = {
        "title": truncate(article.title, TITLE_LIMIT),
        "url": article.link,
        "description": truncate(summary, DESCRIPTION_LIMIT),
        "color": color,
        "footer": {"text": f"{article.category} / {article.feed_name}"},
    }
    if image_url:
        embed["image"] = {"url": image_url}
    return embed


def embed_char_count(embed: dict) -> int:
    return (
        len(embed.get("title", ""))
        + len(embed.get("description", ""))
        + len(embed.get("footer", {}).get("text", ""))
    )


def batch_indexes(
    embeds: list[dict], max_per_message: int = MAX_EMBEDS_PER_MESSAGE
) -> list[list[int]]:
    """embed列を 最大max_per_message個 かつ 文字数合計がEMBED_CHAR_BUDGET以内 のバッチに分割する。"""
    max_per_message = max(1, min(MAX_EMBEDS_PER_MESSAGE, max_per_message))
    batches: list[list[int]] = []
    current: list[int] = []
    current_chars = 0
    for i, embed in enumerate(embeds):
        chars = embed_char_count(embed)
        if current and (
            len(current) >= max_per_message
            or current_chars + chars > EMBED_CHAR_BUDGET
        ):
            batches.append(current)
            current = []
            current_chars = 0
        current.append(i)
        current_chars += chars
    if current:
        batches.append(current)
    return batches


def _with_wait(webhook_url: str) -> str:
    """?wait=true を付ける。デフォルト（wait=false）はメッセージが保存されなくても
    エラーを返さないため、silent dropを検知できない。"""
    sep = "&" if "?" in webhook_url else "?"
    return f"{webhook_url}{sep}wait=true"


def _post(webhook_url: str, payload: dict) -> None:
    """Webhookに1リクエスト送る（保存確認つき）。

    429はヘッダに従い待って再試行、5xx（Discord側の一時障害）も短い待機で再試行。
    合わせて最大3回。
    """
    body = json.dumps(payload).encode("utf-8")
    for attempt in range(MAX_ATTEMPTS):
        req = urllib.request.Request(
            _with_wait(webhook_url),
            data=body,
            headers={"Content-Type": "application/json", "User-Agent": USER_AGENT},
        )
        try:
            with urllib.request.urlopen(req, timeout=POST_TIMEOUT):
                return
        except urllib.error.HTTPError as e:
            detail = ""
            try:
                detail = e.read(500).decode("utf-8", errors="replace")
            except Exception:
                pass
            if attempt < MAX_ATTEMPTS - 1:
                if e.code == 429:
                    wait = _retry_after(e.headers)
                    log.warning("Discordレートリミット、%.1f秒待って再試行", wait)
                    time.sleep(wait)
                    continue
                if e.code >= 500:
                    log.warning("Discord HTTP %d、2秒待って再試行", e.code)
                    time.sleep(2)
                    continue
            raise DiscordError(f"HTTP {e.code}: {detail}", code=e.code)
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            raise DiscordError(str(e))


def _retry_after(headers) -> float:
    for name in ("X-RateLimit-Reset-After", "Retry-After"):
        value = headers.get(name)
        if value:
            try:
                return float(value) + 0.5
            except ValueError:
                pass
    return 5.0 + 0.5


def send_content(webhook_url: str, content: str) -> None:
    _post(webhook_url, {"content": truncate(content, CONTENT_LIMIT)})


def _strip_image(embed: dict) -> dict:
    return {k: v for k, v in embed.items() if k != "image"}


def post_embeds(
    webhook_url: str,
    pairs: list[tuple[dict, Article]],
    embeds_per_message: int = MAX_EMBEDS_PER_MESSAGE,
) -> list[Article]:
    """記事embedをバッチ送信し、投稿に成功した記事を返す。

    バッチ失敗時の縮退（SPEC §10・付録A.8）:
    5xxなら同一内容を1回再送 → 1embedずつ個別送信（画像は保持）→
    個別でも失敗したembedだけ画像を外して再送 → それでも失敗したらスキップ。
    複数embedメッセージ自体が時間帯によって500になるため、
    「バッチ全体から画像除去」は画像を失うだけで解決せず廃止した。
    """
    posted: list[Article] = []
    embeds = [e for e, _ in pairs]
    for batch in batch_indexes(embeds, embeds_per_message):
        batch_pairs = [pairs[i] for i in batch]
        payload = {"embeds": [e for e, _ in batch_pairs]}
        try:
            _post(webhook_url, payload)
            posted.extend(a for _, a in batch_pairs)
            continue
        except DiscordError as e:
            # 5xxはDiscord側の一時障害。同一内容をもう1回だけ再送してみる
            if e.code is not None and e.code >= 500:
                log.warning("バッチ送信が5xx（10秒後に同一内容で再送）: %s", e)
                time.sleep(10)
                try:
                    _post(webhook_url, payload)
                    posted.extend(a for _, a in batch_pairs)
                    continue
                except DiscordError as e2:
                    log.warning("再送も失敗（1embedずつ個別送信に切替）: %s", e2)
            else:
                log.warning("バッチ送信失敗（1embedずつ個別送信に切替）: %s", e)

        # 個別送信。画像は保持する（個別送信は5xx障害時間帯でも成功実績しかない）
        for embed, article in batch_pairs:
            try:
                _post(webhook_url, {"embeds": [embed]})
                posted.append(article)
                continue
            except DiscordError as e:
                log.warning("個別送信失敗: %s: %s", article.link, e)
            if "image" in embed:
                try:
                    _post(webhook_url, {"embeds": [_strip_image(embed)]})
                    posted.append(article)
                    log.info("画像を外して投稿: %s", article.link)
                    continue
                except DiscordError as e:
                    log.warning("画像除去でも失敗: %s: %s", article.link, e)
            log.error("embed送信失敗（スキップ）: %s", article.link)
    return posted


def notify_failure(webhook_url: str | None, name: str, reason: str) -> None:
    """異常終了時のベストエフォート通知（SPEC §11）。それ自体の失敗はログのみ。"""
    if not webhook_url:
        return
    try:
        send_content(webhook_url, f"-# ⚠️ mekiki 実行失敗（{name}）: {reason}")
    except Exception as e:
        log.error("失敗通知の送信自体に失敗: %s", e)
