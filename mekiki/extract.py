"""html_to_text・og:image抽出・画像URLサニタイズ（SPEC §7.1・§8）"""

from __future__ import annotations

import re
from html import unescape
from html.parser import HTMLParser
from urllib.parse import quote, urljoin, urlsplit, urlunsplit

# Discordのembed URL上限。超える画像URLは400になるため破棄する
MAX_IMAGE_URL_LENGTH = 2048

# 中身ごと捨てるタグ（SPEC §7.1）
_SKIP_TAGS = frozenset(
    {"script", "style", "noscript", "template", "svg", "iframe", "head"}
)


class _TextExtractor(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self._skip_depth = 0
        self.chunks: list[str] = []

    def handle_starttag(self, tag, attrs):
        if tag in _SKIP_TAGS:
            self._skip_depth += 1

    def handle_endtag(self, tag):
        if tag in _SKIP_TAGS and self._skip_depth > 0:
            self._skip_depth -= 1

    def handle_data(self, data):
        if self._skip_depth == 0:
            text = data.strip()
            if text:
                self.chunks.append(text)


def html_to_text(html: str) -> str:
    """「どこが本文か」の推定はしない機械的なタグ除去。

    壊れたHTMLで例外が出ても、そこまでに集めたテキストを返す。
    """
    parser = _TextExtractor()
    try:
        parser.feed(html)
        parser.close()
    except Exception:
        pass
    return "\n".join(parser.chunks)


# og:image抽出（SPEC §8）。属性の順序に依存しないよう、metaタグごとに属性を辞書化する
_META_TAG_RE = re.compile(r"<meta\s[^>]*>", re.IGNORECASE | re.DOTALL)
_ATTR_RE = re.compile(r"""([a-zA-Z:_-]+)\s*=\s*("([^"]*)"|'([^']*)')""")

_IMAGE_META_PRIORITY = (
    "og:image",
    "og:image:url",
    "og:image:secure_url",
    "twitter:image",
    "twitter:image:src",
)


def extract_og_image(html: str) -> str | None:
    found: dict[str, str] = {}
    for tag in _META_TAG_RE.findall(html):
        attrs: dict[str, str] = {}
        for m in _ATTR_RE.finditer(tag):
            attrs[m.group(1).lower()] = m.group(3) if m.group(3) is not None else m.group(4)
        key = (attrs.get("property") or attrs.get("name") or "").lower()
        content = attrs.get("content")
        if key in _IMAGE_META_PRIORITY and content and key not in found:
            # 属性値のHTMLエンティティ（&amp;等）をデコードする
            found[key] = unescape(content.strip())
    for key in _IMAGE_META_PRIORITY:
        if found.get(key):
            return found[key]
    return None


def sanitize_image_url(url: str, base_url: str = "") -> str | None:
    """Discordに渡せる形に画像URLを整える。不正なら None（SPEC §8・付録A.3）。

    - 相対URLは記事ページURL基準で絶対化
    - スキームはhttp/httpsのみ許可（javascript:/data:等は破棄）
    - パス・クエリの未エンコード文字（空白・非ASCII）をパーセントエンコード
    - フラグメントは捨てる
    - Discordのembed URL上限（2048字）を超えるものは破棄
    """
    url = url.strip()
    if not url:
        return None
    try:
        if base_url:
            url = urljoin(base_url, url)
        parts = urlsplit(url)
    except ValueError:
        return None
    if parts.scheme not in ("http", "https") or not parts.netloc:
        return None
    path = quote(parts.path, safe="/%")
    query = quote(parts.query, safe="=&%")
    result = urlunsplit((parts.scheme, parts.netloc, path, query, ""))
    if len(result) > MAX_IMAGE_URL_LENGTH:
        return None
    return result
