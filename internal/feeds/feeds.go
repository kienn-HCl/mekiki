// Package feeds はOPMLの読み書き、フィードの並列取得、記事の重複統合を担う。
package feeds

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/kienn-HCl/mekiki/internal/extract"
	"github.com/kienn-HCl/mekiki/internal/store"
)

const (
	UserAgent    = "Mozilla/5.0 mekiki/2.0"
	fetchTimeout = 15 * time.Second
	maxRetries   = 4 // 接続エラー・タイムアウト系のみ。HTTPエラー応答はリトライしない
	maxFeedBytes = 10 << 20
	workers      = 10
	// タイトル完全一致dedupの対象とする最小タイトル長（短い定型タイトルの誤統合を防ぐ）
	titleDedupMinChars = 10
)

// ---- OPML ----

type OPMLFeed struct {
	Title    string
	URL      string
	Category string
}

type opmlOutline struct {
	Text     string        `xml:"text,attr"`
	Title    string        `xml:"title,attr,omitempty"`
	Type     string        `xml:"type,attr,omitempty"`
	XMLURL   string        `xml:"xmlUrl,attr,omitempty"`
	Outlines []opmlOutline `xml:"outline"`
}

type opmlDoc struct {
	XMLName xml.Name `xml:"opml"`
	Version string   `xml:"version,attr"`
	Head    struct {
		Title string `xml:"title"`
	} `xml:"head"`
	Body struct {
		Outlines []opmlOutline `xml:"outline"`
	} `xml:"body"`
}

// ParseOPML はOPMLを読む。第1階層のoutlineがカテゴリ、その子がフィード。
// 第1階層に直接フィードがあればカテゴリなしとして扱う。xmlUrlのないoutlineは無視。
func ParseOPML(r io.Reader) ([]OPMLFeed, error) {
	var doc opmlDoc
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("OPMLのパースに失敗: %w", err)
	}
	var out []OPMLFeed
	add := func(o opmlOutline, category string) {
		title := o.Text
		if title == "" {
			title = o.Title
		}
		if title == "" {
			title = o.XMLURL
		}
		out = append(out, OPMLFeed{Title: title, URL: strings.TrimSpace(o.XMLURL), Category: category})
	}
	for _, top := range doc.Body.Outlines {
		if top.XMLURL != "" {
			add(top, "")
			continue
		}
		cat := top.Text
		if cat == "" {
			cat = top.Title
		}
		for _, o := range top.Outlines {
			if o.XMLURL != "" {
				add(o, cat)
			}
		}
	}
	return out, nil
}

// WriteOPML は登録フィードをカテゴリ別の2階層OPMLで書き出す。
func WriteOPML(w io.Writer, list []store.Feed) error {
	doc := opmlDoc{Version: "1.0"}
	doc.Head.Title = "mekiki feeds"
	idx := map[string]int{}
	for _, f := range list {
		i, ok := idx[f.Category]
		if !ok {
			doc.Body.Outlines = append(doc.Body.Outlines, opmlOutline{Text: f.Category})
			i = len(doc.Body.Outlines) - 1
			idx[f.Category] = i
		}
		doc.Body.Outlines[i].Outlines = append(doc.Body.Outlines[i].Outlines,
			opmlOutline{Text: f.Title, Type: "rss", XMLURL: f.URL})
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(doc)
}

// ---- 取得 ----

type Options struct {
	ArticlesPerFeed   int
	MaxFailures       int
	FailureRetryHours int
}

type Result struct {
	Feeds    int // 取得を試みたフィード数
	Failed   int
	Skipped  []store.Feed // 連続失敗でスキップ中のフィード
	Added    int          // 新規に追加された記事数
	Duration time.Duration
}

type outcome struct {
	feed          store.Feed
	articles      []store.Article
	notModified   bool
	etag, lastMod string
	err           error
}

// FetchAll は有効なフィード（profileIDが0なら全プロファイル）を並列取得して新着記事をDBに保存する。
func FetchAll(ctx context.Context, st *store.Store, profileID int64, opt Options, now time.Time) (Result, error) {
	start := time.Now()
	all, err := st.Feeds(ctx, profileID)
	if err != nil {
		return Result{}, err
	}
	var res Result
	var targets []store.Feed
	for _, f := range all {
		if !f.Enabled {
			continue
		}
		if f.Failures >= opt.MaxFailures {
			// 一時障害でフィードが静かに永久死しないよう、一定時間ごとに再試行する
			if !f.LastAttempt.IsZero() && now.Sub(f.LastAttempt) < time.Duration(opt.FailureRetryHours)*time.Hour {
				res.Skipped = append(res.Skipped, f)
				continue
			}
			slog.Info("スキップ中フィードを再試行", "feed", f.Title)
		}
		targets = append(targets, f)
	}
	res.Feeds = len(targets)

	jobs := make(chan store.Feed)
	results := make(chan outcome)
	var wg sync.WaitGroup
	client := &http.Client{Timeout: fetchTimeout}
	for range min(workers, max(1, len(targets))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				results <- fetchOne(ctx, client, f, opt.ArticlesPerFeed, now)
			}
		}()
	}
	go func() {
		for _, f := range targets {
			jobs <- f
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	// DB更新はこのゴルーチンだけで行う
	for o := range results {
		if o.err != nil {
			res.Failed++
			slog.Warn("フィード取得失敗", "feed", o.feed.Title, "failures", o.feed.Failures+1, "err", o.err)
			if err := st.MarkFeedFailure(ctx, o.feed.ID, o.err.Error(), now); err != nil {
				return res, err
			}
			continue
		}
		if err := st.MarkFeedSuccess(ctx, o.feed.ID, o.etag, o.lastMod, now, o.notModified); err != nil {
			return res, err
		}
		if o.notModified {
			continue
		}
		n, err := st.InsertArticles(ctx, o.articles)
		if err != nil {
			return res, err
		}
		res.Added += n
	}
	res.Duration = time.Since(start)
	return res, nil
}

func fetchOne(ctx context.Context, client *http.Client, f store.Feed, perFeed int, now time.Time) outcome {
	o := outcome{feed: f}
	var data []byte
	var hdr http.Header
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
		if err != nil {
			o.err = err
			return o
		}
		req.Header.Set("User-Agent", UserAgent)
		if f.ETag != "" {
			req.Header.Set("If-None-Match", f.ETag)
		}
		if f.LastModified != "" {
			req.Header.Set("If-Modified-Since", f.LastModified)
		}
		resp, err := client.Do(req)
		if err != nil {
			if attempt < maxRetries && retryable(err) && ctx.Err() == nil {
				time.Sleep(time.Duration(1<<attempt)*time.Second + time.Duration(rand.IntN(1000))*time.Millisecond)
				continue
			}
			o.err = err
			return o
		}
		if resp.StatusCode == http.StatusNotModified {
			resp.Body.Close()
			o.notModified = true
			return o
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			o.err = fmt.Errorf("HTTP %d", resp.StatusCode)
			return o
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
		resp.Body.Close()
		if err != nil {
			if attempt < maxRetries && retryable(err) && ctx.Err() == nil {
				continue
			}
			o.err = err
			return o
		}
		hdr = resp.Header
		break
	}

	parsed, err := gofeed.NewParser().Parse(bytes.NewReader(data))
	if err != nil {
		o.err = fmt.Errorf("パース失敗: %w", err)
		return o
	}
	o.etag, o.lastMod = hdr.Get("ETag"), hdr.Get("Last-Modified")
	for i, it := range parsed.Items {
		if i >= perFeed {
			break
		}
		if a, ok := itemToArticle(it, f, now); ok {
			o.articles = append(o.articles, a)
		}
	}
	return o
}

func retryable(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "connection reset")
}

func itemToArticle(it *gofeed.Item, f store.Feed, now time.Time) (store.Article, bool) {
	title := strings.TrimSpace(extract.HTMLToText(it.Title))
	link := strings.TrimSpace(it.Link)
	if link == "" && len(it.Links) > 0 {
		link = strings.TrimSpace(it.Links[0])
	}
	if title == "" || link == "" {
		return store.Article{}, false
	}
	var pub time.Time
	if it.PublishedParsed != nil {
		pub = *it.PublishedParsed
	} else if it.UpdatedParsed != nil {
		pub = *it.UpdatedParsed
	}
	if pub.After(now.Add(time.Hour)) {
		pub = now // 未来日時のフィードは取得時刻に丸める
	}
	body := it.Content
	if body == "" {
		body = it.Description
	}
	return store.Article{
		FeedID:      f.ID,
		Title:       title,
		Link:        link,
		NormLink:    extract.NormalizeURL(link),
		PublishedAt: pub,
		FetchedAt:   now,
		Body:        body,
		Image:       itemImage(it),
	}, true
}

func itemImage(it *gofeed.Item) string {
	if it.Image != nil && it.Image.URL != "" {
		return it.Image.URL
	}
	if media, ok := it.Extensions["media"]; ok {
		for _, key := range []string{"thumbnail", "content"} {
			for _, e := range media[key] {
				u := e.Attrs["url"]
				if u == "" {
					continue
				}
				if key == "thumbnail" || e.Attrs["medium"] == "image" ||
					strings.HasPrefix(e.Attrs["type"], "image/") {
					return u
				}
			}
		}
		for _, g := range media["group"] {
			for _, e := range g.Children["thumbnail"] {
				if u := e.Attrs["url"]; u != "" {
					return u
				}
			}
		}
	}
	for _, e := range it.Enclosures {
		if strings.HasPrefix(e.Type, "image/") && e.URL != "" {
			return e.URL
		}
	}
	return ""
}

// ---- dedup ----

// Dedup は正規化URLが同一の記事、タイトルが完全一致（一定長以上）の記事を統合する。
// 最初に出現したものを残す。excludeTitles に載っているタイトル（過去のページ掲載分）も除外する。
func Dedup(articles []store.Article, excludeTitles map[string]bool) (kept, dropped []store.Article) {
	seenURL := map[string]bool{}
	seenTitle := map[string]bool{}
	for _, a := range articles {
		if seenURL[a.NormLink] {
			dropped = append(dropped, a)
			continue
		}
		seenURL[a.NormLink] = true
		t := strings.TrimSpace(a.Title)
		if len([]rune(t)) >= titleDedupMinChars {
			if seenTitle[t] || excludeTitles[t] {
				dropped = append(dropped, a)
				continue
			}
			seenTitle[t] = true
		}
		kept = append(kept, a)
	}
	return kept, dropped
}
