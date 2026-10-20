// Package extract はHTMLのタグ除去・og:image抽出・URLの正規化とサニタイズを担う純関数群。
package extract

import (
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// Discordのembed URL上限。超える画像URLはembed全体が400になるため破棄する
const MaxImageURLLength = 2048

// 中身ごと捨てるタグ
var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
	"svg": true, "iframe": true, "head": true,
}

// HTMLToText は「どこが本文か」の推定はしない機械的なタグ除去。
// 文字参照は文字に変換し、空白のみのテキストノードは捨て、残りを改行で連結する。
// 生HTMLの大半はマークアップなので、これだけでClaudeへの入力が1/10〜1/30になる。
func HTMLToText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var chunks []string
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.Join(chunks, "\n")
		case html.StartTagToken:
			name, _ := z.TagName()
			if skipTags[string(name)] {
				skip++
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if skipTags[string(name)] && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				if t := strings.TrimSpace(string(z.Text())); t != "" {
					chunks = append(chunks, t)
				}
			}
		}
	}
}

var imageMetaPriority = []string{
	"og:image", "og:image:url", "og:image:secure_url", "twitter:image", "twitter:image:src",
}

// OGImage はページHTMLの<meta>から記事画像URLを取り出す（og:image → twitter:image の優先順）。
// 属性値のHTMLエンティティ（&amp;等）はトークナイザがデコードする。
func OGImage(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	found := map[string]string{}
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if string(name) == "body" {
			break // metaはheadにある。巨大な本文を走査しない
		}
		if string(name) != "meta" || !hasAttr {
			continue
		}
		var key, content string
		for {
			k, v, more := z.TagAttr()
			switch strings.ToLower(string(k)) {
			case "property", "name":
				if key == "" {
					key = strings.ToLower(strings.TrimSpace(string(v)))
				}
			case "content":
				content = strings.TrimSpace(string(v))
			}
			if !more {
				break
			}
		}
		if key != "" && content != "" {
			if _, ok := found[key]; !ok {
				found[key] = content
			}
		}
	}
	for _, k := range imageMetaPriority {
		if v := found[k]; v != "" {
			return v
		}
	}
	return ""
}

// SanitizeImageURL はDiscord・Webページに渡せる形に画像URLを整える。不正なら空文字列。
//   - 相対URLは記事ページURL基準で絶対化
//   - スキームはhttp/httpsのみ許可（javascript:/data:等は破棄）
//   - パス・クエリの未エンコード文字（空白・非ASCII）をパーセントエンコード
//   - フラグメントは捨てる
//   - 2048字を超えるものは破棄
func SanitizeImageURL(raw, base string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		u, err = url.Parse(escapeLoose(raw))
		if err != nil {
			return ""
		}
	}
	if base != "" {
		if b, err := url.Parse(base); err == nil {
			u = b.ResolveReference(u)
		}
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	u.RawQuery = escapeLoose(u.RawQuery)
	out := u.String()
	if len(out) > MaxImageURLLength {
		return ""
	}
	return out
}

// escapeLoose は空白・非ASCII・制御文字だけをパーセントエンコードする（既存の%xxや区切り文字は保つ）。
func escapeLoose(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c <= 0x20 || c >= 0x7f || c == '"' || c == '<' || c == '>' {
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex(c)))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func hex(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&0xf]})
}

var trackingParams = map[string]bool{"fbclid": true, "gclid": true, "ref": true, "source": true}

// NormalizeURL は重複判定・投稿済み判定用のURL正規化。
// フラグメント除去、トラッキング系クエリ（utm_*, fbclid, gclid, ref, source）除去、
// 末尾スラッシュの統一、スキーム・ホスト名の小文字化。
func NormalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment, u.RawFragment = "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || trackingParams[lk] {
			q.Del(k)
		}
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	u.RawQuery = strings.Join(parts, "&")
	u.ForceQuery = false
	return u.String()
}
