"""extract.py: html_to_text・og:image抽出・画像URLサニタイズ（SPEC §7.1・§8）"""

from mekiki.extract import extract_og_image, html_to_text, sanitize_image_url


class TestHtmlToText:
    def test_basic(self):
        html = "<html><body><p>Hello</p><p>二行目</p></body></html>"
        assert html_to_text(html) == "Hello\n二行目"

    def test_skip_tags(self):
        html = (
            "<head><title>捨てる</title></head>"
            "<script>var a = 1;</script>"
            "<style>.x { color: red }</style>"
            "<noscript>JSなし</noscript>"
            "<template><p>雛形</p></template>"
            "<svg><text>ベクタ</text></svg>"
            "<iframe>枠</iframe>"
            "<p>本文</p>"
        )
        assert html_to_text(html) == "本文"

    def test_nested_skip(self):
        html = "<svg><g><text>中身</text></g></svg><p>残る</p>"
        assert html_to_text(html) == "残る"

    def test_charref(self):
        assert html_to_text("<p>A &amp; B &copy;</p>") == "A & B ©"

    def test_whitespace_only_nodes_dropped(self):
        assert html_to_text("<div>  \n\t </div><p>x</p>") == "x"

    def test_broken_html_returns_collected(self):
        assert "text" in html_to_text("<div><p>text<a href='")

    def test_empty(self):
        assert html_to_text("") == ""


class TestExtractOgImage:
    def test_og_image(self):
        html = '<meta property="og:image" content="https://ex.com/a.png">'
        assert extract_og_image(html) == "https://ex.com/a.png"

    def test_attr_order_reversed(self):
        html = "<meta content='https://ex.com/b.png' property='og:image' />"
        assert extract_og_image(html) == "https://ex.com/b.png"

    def test_twitter_fallback(self):
        html = '<meta name="twitter:image" content="https://ex.com/t.png">'
        assert extract_og_image(html) == "https://ex.com/t.png"

    def test_priority_og_over_twitter(self):
        html = (
            '<meta name="twitter:image" content="https://ex.com/t.png">'
            '<meta property="og:image" content="https://ex.com/og.png">'
        )
        assert extract_og_image(html) == "https://ex.com/og.png"

    def test_none(self):
        assert extract_og_image("<meta charset='utf-8'><p>本文</p>") is None

    def test_html_entities_unescaped(self):
        # content属性の&amp;がデコードされないと壊れたクエリになる（本番障害の再発防止）
        html = '<meta property="og:image" content="https://ex.com/a.png?w=1200&amp;h=630">'
        assert extract_og_image(html) == "https://ex.com/a.png?w=1200&h=630"


class TestSanitizeImageUrl:
    def test_valid_passthrough(self):
        assert (
            sanitize_image_url("https://ex.com/a.png")
            == "https://ex.com/a.png"
        )

    def test_reject_javascript_and_data(self):
        assert sanitize_image_url("javascript:alert(1)") is None
        assert sanitize_image_url("data:image/png;base64,xxxx") is None

    def test_reject_empty_and_no_netloc(self):
        assert sanitize_image_url("") is None
        assert sanitize_image_url("not a url") is None

    def test_encode_space_and_nonascii(self):
        # 付録A.3: 未エンコードのog:imageがDiscordでHTTP 400になる
        assert (
            sanitize_image_url("https://ex.com/画像 1.png?a=あ")
            == "https://ex.com/%E7%94%BB%E5%83%8F%201.png?a=%E3%81%82"
        )

    def test_already_encoded_not_double_encoded(self):
        url = "https://ex.com/%E7%94%BB.png"
        assert sanitize_image_url(url) == url

    def test_relative_resolved_from_article_url(self):
        assert (
            sanitize_image_url("/img/a.png", base_url="https://ex.com/article/1")
            == "https://ex.com/img/a.png"
        )

    def test_fragment_dropped(self):
        assert sanitize_image_url("https://ex.com/a.png#frag") == "https://ex.com/a.png"

    def test_over_discord_url_limit_rejected(self):
        # Discordのembed URL上限2048字超はHTTP 400になるため破棄（本番障害の再発防止）
        url = "https://ex.com/" + "a" * 2100
        assert sanitize_image_url(url) is None
