package extract

import (
	"strings"
	"testing"
)

func TestHTMLToText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"basic", "<html><body><p>Hello</p><p>二行目</p></body></html>", "Hello\n二行目"},
		{"skip tags", "<head><title>捨てる</title></head><script>var a = 1;</script>" +
			"<style>.x { color: red }</style><noscript>JSなし</noscript>" +
			"<template><p>雛形</p></template><svg><text>ベクタ</text></svg>" +
			"<iframe>枠</iframe><p>本文</p>", "本文"},
		{"nested skip", "<svg><g><text>中身</text></g></svg><p>残る</p>", "残る"},
		{"charref", "<p>A &amp; B &copy;</p>", "A & B ©"},
		{"whitespace only", "<div>  \n\t </div><p>x</p>", "x"},
		{"empty", "", ""},
		{"plain text", "タグのない本文", "タグのない本文"},
	}
	for _, c := range cases {
		if got := HTMLToText(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := HTMLToText("<div><p>text<a href='"); !strings.Contains(got, "text") {
		t.Errorf("broken html: got %q", got)
	}
}

func TestOGImage(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"og", `<meta property="og:image" content="https://ex.com/a.png">`, "https://ex.com/a.png"},
		{"reversed attrs", `<meta content='https://ex.com/b.png' property='og:image' />`, "https://ex.com/b.png"},
		{"twitter", `<meta name="twitter:image" content="https://ex.com/t.png">`, "https://ex.com/t.png"},
		{"og over twitter", `<meta name="twitter:image" content="https://ex.com/t.png">` +
			`<meta property="og:image" content="https://ex.com/og.png">`, "https://ex.com/og.png"},
		{"none", `<meta charset="utf-8"><p>本文</p>`, ""},
		// content属性の&amp;がデコードされないと壊れたクエリになる（本番障害の再発防止）
		{"entities", `<meta property="og:image" content="https://ex.com/a.png?w=1200&amp;h=630">`,
			"https://ex.com/a.png?w=1200&h=630"},
		{"empty html", "", ""},
	}
	for _, c := range cases {
		if got := OGImage(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSanitizeImageURL(t *testing.T) {
	cases := []struct{ name, in, base, want string }{
		{"valid", "https://ex.com/a.png", "", "https://ex.com/a.png"},
		{"javascript", "javascript:alert(1)", "", ""},
		{"data", "data:image/png;base64,xxxx", "", ""},
		{"empty", "", "", ""},
		{"not a url", "not a url", "", ""},
		// 未エンコードのog:imageをDiscordに渡すとHTTP 400になる
		{"encode", "https://ex.com/画像 1.png?a=あ", "",
			"https://ex.com/%E7%94%BB%E5%83%8F%201.png?a=%E3%81%82"},
		{"no double encode", "https://ex.com/%E7%94%BB.png", "", "https://ex.com/%E7%94%BB.png"},
		{"relative", "/img/a.png", "https://ex.com/article/1", "https://ex.com/img/a.png"},
		{"fragment", "https://ex.com/a.png#frag", "", "https://ex.com/a.png"},
		{"query kept", "https://ex.com/a.png?w=1&h=2", "", "https://ex.com/a.png?w=1&h=2"},
		// Discordのembed URL上限2048字超はHTTP 400になるため破棄（本番障害の再発防止）
		{"too long", "https://ex.com/" + strings.Repeat("a", 2100), "", ""},
	}
	for _, c := range cases {
		if got := SanitizeImageURL(c.in, c.base); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://ex.com/p?utm_source=x&utm_medium=y&id=3", "https://ex.com/p?id=3"},
		{"https://ex.com/p?fbclid=x", "https://ex.com/p"},
		{"https://ex.com/p?gclid=x&ref=t&source=s", "https://ex.com/p"},
		{"https://ex.com/p#section", "https://ex.com/p"},
		{"https://EX.com/P", "https://ex.com/P"},
		{"https://ex.com/p?b=2&a=1", "https://ex.com/p?a=1&b=2"},
	}
	for _, c := range cases {
		if got := NormalizeURL(c.in); got != c.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if NormalizeURL("https://ex.com/p/") != NormalizeURL("https://ex.com/p") {
		t.Error("末尾スラッシュが統一されていない")
	}
	if NormalizeURL("https://ex.com/") != NormalizeURL("https://ex.com") {
		t.Error("ルートの末尾スラッシュが統一されていない")
	}
}
