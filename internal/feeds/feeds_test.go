package feeds

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/kienn-HCl/mekiki/internal/extract"
	"github.com/kienn-HCl/mekiki/internal/store"
)

const sampleOPML = `<?xml version="1.0"?>
<opml version="1.0">
  <body>
    <outline text="Linux・OS">
      <outline text="LWN.net" xmlUrl="https://lwn.net/headlines/rss" />
      <outline text="URLなし" />
    </outline>
    <outline text="直下のフィード" xmlUrl="https://ex.com/feed" />
  </body>
</opml>`

func TestParseOPML(t *testing.T) {
	list, err := ParseOPML(strings.NewReader(sampleOPML))
	if err != nil {
		t.Fatal(err)
	}
	want := []OPMLFeed{
		{Title: "LWN.net", URL: "https://lwn.net/headlines/rss", Category: "Linux・OS", Enabled: true},
		{Title: "直下のフィード", URL: "https://ex.com/feed", Category: "", Enabled: true},
	}
	if len(list) != len(want) {
		t.Fatalf("got %d feeds, want %d: %+v", len(list), len(want), list)
	}
	for i := range want {
		if list[i] != want[i] {
			t.Errorf("[%d] got %+v, want %+v", i, list[i], want[i])
		}
	}
}

func TestOPMLRoundTrip(t *testing.T) {
	in := []store.Feed{
		{Title: "A", URL: "https://a.example/feed", Category: "技術"},
		{Title: "B & C", URL: "https://b.example/rss?x=1&y=2", Category: "技術"},
		{Title: "D", URL: "https://d.example/atom", Category: "ニュース", Enabled: false},
	}
	in[0].Enabled, in[1].Enabled = true, true
	var buf bytes.Buffer
	if err := WriteOPML(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ParseOPML(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Title != in[i].Title || out[i].URL != in[i].URL || out[i].Category != in[i].Category ||
			out[i].Enabled != in[i].Enabled {
			t.Errorf("[%d] got %+v, want %+v", i, out[i], in[i])
		}
	}
}

func art(title, link string) store.Article {
	return store.Article{Title: title, Link: link, NormLink: extract.NormalizeURL(link)}
}

func titles(list []store.Article) []string {
	var out []string
	for _, a := range list {
		out = append(out, a.Title)
	}
	return out
}

func TestDedup(t *testing.T) {
	t.Run("same normalized url, first kept", func(t *testing.T) {
		kept, dropped := Dedup([]store.Article{
			art("最初に出現した記事のタイトル", "https://ex.com/a?utm_source=rss"),
			art("後から出現した記事のタイトル", "https://ex.com/a"),
		}, nil)
		if len(kept) != 1 || kept[0].Title != "最初に出現した記事のタイトル" || len(dropped) != 1 {
			t.Errorf("kept=%v dropped=%v", titles(kept), titles(dropped))
		}
	})
	t.Run("same title merged", func(t *testing.T) {
		kept, _ := Dedup([]store.Article{
			art("同一タイトルの配信記事です", "https://site1.com/a"),
			art("同一タイトルの配信記事です", "https://site2.com/b"),
		}, nil)
		if len(kept) != 1 {
			t.Errorf("kept=%v", titles(kept))
		}
	})
	t.Run("short title not merged", func(t *testing.T) {
		// 10文字未満の定型タイトルはタイトル統合の対象外
		kept, _ := Dedup([]store.Article{
			art("今週の話題", "https://site1.com/a"),
			art("今週の話題", "https://site2.com/b"),
		}, nil)
		if len(kept) != 2 {
			t.Errorf("kept=%v", titles(kept))
		}
	})
	t.Run("title already on a page excluded", func(t *testing.T) {
		kept, dropped := Dedup([]store.Article{
			art("過去のページに載った記事のタイトル", "https://other.com/x"),
			art("新しく出てきた別の記事のタイトル", "https://ex.com/y"),
		}, map[string]bool{"過去のページに載った記事のタイトル": true})
		if len(kept) != 1 || kept[0].Link != "https://ex.com/y" || len(dropped) != 1 {
			t.Errorf("kept=%v dropped=%v", titles(kept), titles(dropped))
		}
	})
}

const sampleRSS = `<?xml version="1.0"?>
<rss version="2.0" xmlns:media="http://search.yahoo.com/mrss/" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>Example</title>
<item>
  <title>記事&amp;タイトル</title>
  <link>https://ex.com/1?utm_source=rss</link>
  <pubDate>Mon, 19 Oct 2026 10:00:00 GMT</pubDate>
  <description>短い説明</description>
  <content:encoded><![CDATA[<p>本文です</p>]]></content:encoded>
  <media:thumbnail url="https://ex.com/thumb.jpg" />
</item>
<item>
  <title>日付も画像もない記事</title>
  <link>https://ex.com/2</link>
  <enclosure url="https://ex.com/a.mp3" type="audio/mpeg" length="1" />
</item>
<item><title>リンクなし</title></item>
</channel></rss>`

func TestItemToArticle(t *testing.T) {
	parsed, err := gofeed.NewParser().ParseString(sampleRSS)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	f := store.Feed{ID: 7, Title: "Example", Category: "技術"}
	var got []store.Article
	for _, it := range parsed.Items {
		if a, ok := itemToArticle(it, f, now); ok {
			got = append(got, a)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d articles, want 2", len(got))
	}
	a := got[0]
	if a.Title != "記事&タイトル" || a.NormLink != "https://ex.com/1" || a.FeedID != 7 {
		t.Errorf("unexpected article: %+v", a)
	}
	if a.Body != "<p>本文です</p>" {
		t.Errorf("body should prefer content:encoded, got %q", a.Body)
	}
	if a.Image != "https://ex.com/thumb.jpg" {
		t.Errorf("image = %q", a.Image)
	}
	if !a.PublishedAt.Equal(time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("published = %v", a.PublishedAt)
	}
	b := got[1]
	if !b.PublishedAt.IsZero() || b.Image != "" {
		t.Errorf("undated/no-image article: %+v", b)
	}
}
