package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kienn-HCl/mekiki/internal/app"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/store"
)

func newServer(t *testing.T, password string) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(app.New(st), password)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func seed(t *testing.T, st *store.Store) string {
	ctx := context.Background()
	st.AddFeed(ctx, "Feed <A>", "https://a.example/rss", "技術")
	feeds, _ := st.Feeds(ctx)
	now := time.Now()
	st.InsertArticles(ctx, []store.Article{{
		FeedID: feeds[0].ID, Title: "記事 <script>", Link: "https://a.example/1",
		NormLink: "https://a.example/1", FetchedAt: now, Body: "<p>本文</p>",
	}})
	date := now.In(config.Defaults().Location()).Format("2006-01-02")
	runID, _ := st.StartRun(ctx, store.Run{Date: date, Kind: "append", Trigger: "manual", StartedAt: now})
	st.AppendItems(ctx, date, []store.PageItem{{
		RunID: runID, Title: "掲載記事", Link: "https://a.example/1", NormLink: "https://a.example/1",
		Category: "技術", FeedName: "Feed", Summary: "要約です", SummaryOK: true, Stage: "feed",
	}}, now)
	st.SetOverview(ctx, date, "- まとめ1\n- まとめ2", now)
	return date
}

func get(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

func TestPagesRender(t *testing.T) {
	ts, st := newServer(t, "")
	date := seed(t, st)
	for _, p := range []string{"/", "/day/" + date, "/day/2020-01-01", "/archive", "/articles",
		"/articles?unread=1", "/feeds", "/settings", "/runs", "/feeds/export.opml", "/api/status",
		"/static/style.css"} {
		code, body := get(t, ts, p)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d: %.300s", p, code, body)
		}
	}
	_, body := get(t, ts, "/")
	for _, want := range []string{"掲載記事", "要約です", "<li>まとめ1</li>", "今日のページを作り直す"} {
		if !strings.Contains(body, want) {
			t.Errorf("今日のページに %q がない", want)
		}
	}
	_, body = get(t, ts, "/articles")
	if strings.Contains(body, "記事 <script>") {
		t.Error("記事タイトルがエスケープされていない")
	}
	if code, _ := get(t, ts, "/day/not-a-date"); code != http.StatusNotFound {
		t.Errorf("不正な日付 = %d", code)
	}
}

func TestBasicAuth(t *testing.T) {
	ts, _ := newServer(t, "secret")
	if code, _ := get(t, ts, "/"); code != http.StatusUnauthorized {
		t.Errorf("認証なし = %d", code)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.SetBasicAuth("me", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("認証あり = %d", resp.StatusCode)
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	ts, _ := newServer(t, "")
	req, _ := http.NewRequest("POST", ts.URL+"/feeds/reset", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("別オリジンのPOST = %d", resp.StatusCode)
	}
}

func TestSaveSettingsAndAddFeed(t *testing.T) {
	ts, st := newServer(t, "")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	form := url.Values{
		"backend": {"cli"}, "model": {"claude-opus-5-5"}, "effort": {"low"}, "cli_path": {"claude"},
		"select_criteria": {"- 何でも"}, "user_memory": {"Goが好き"}, "pick_count": {"7"},
		"hours_back": {"12"}, "timezone": {"Asia/Tokyo"}, "fetch_interval": {"30"},
		"digest_times": {"18:00, 07:30"}, "articles_per_feed": {"10"}, "max_failures": {"5"},
		"failure_retry_hours": {"24"}, "retention_days": {"30"}, "discord_embeds": {"5"},
		"discord_title": {"見出し"}, "discord_color": {"3498DB"}, "api_key": {"sk-ant-test-1234567890"},
	}
	resp, err := client.PostForm(ts.URL+"/settings", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "msg=") {
		t.Fatalf("保存に失敗: %s", loc)
	}
	kv, _ := st.Settings(context.Background())
	s := config.Load(kv)
	if s.Backend != "cli" || s.PickCount != 7 || strings.Join(s.DigestTimes, ",") != "07:30,18:00" ||
		s.DiscordColor != 0x3498DB || s.APIKey != "sk-ant-test-1234567890" || s.WebFetch {
		t.Errorf("保存内容が違う: %+v", s)
	}
	// APIキーは画面に平文で出さない
	_, body := get(t, ts, "/settings")
	if strings.Contains(body, "sk-ant-test-1234567890") {
		t.Error("APIキーが設定画面に平文で出ている")
	}

	resp, err = client.PostForm(ts.URL+"/feeds/add", url.Values{
		"url": {"https://b.example/feed"}, "title": {"B"}, "category": {"ニュース"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	feeds, _ := st.Feeds(context.Background())
	if len(feeds) != 1 || feeds[0].Title != "B" || feeds[0].Category != "ニュース" {
		t.Errorf("フィード追加: %+v", feeds)
	}
}
