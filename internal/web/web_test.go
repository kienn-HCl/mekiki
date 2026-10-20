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
	if err := st.EnsureProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
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
	st.AddFeed(ctx, 1, "Feed <A>", "https://a.example/rss", "技術")
	feeds, _ := st.Feeds(ctx, 1)
	now := time.Now()
	st.InsertArticles(ctx, []store.Article{{
		FeedID: feeds[0].ID, Title: "記事 <script>", Link: "https://a.example/1",
		NormLink: "https://a.example/1", FetchedAt: now, Body: "<p>本文</p>",
	}})
	date := now.In(config.Defaults().Location()).Format("2006-01-02")
	runID, _ := st.StartRun(ctx, store.Run{ProfileID: 1, Date: date, Kind: "append", Trigger: "manual", StartedAt: now})
	st.AppendItems(ctx, 1, date, []store.PageItem{{
		RunID: runID, Title: "掲載記事", Link: "https://a.example/1", NormLink: "https://a.example/1",
		Category: "技術", FeedName: "Feed", Summary: "要約です", SummaryOK: true, Stage: "feed",
	}}, now, false)
	st.SetOverview(ctx, 1, date, "- まとめ1\n- まとめ2", now)
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
	for _, p := range []string{"/", "/p/main/", "/p/main/day/" + date, "/p/main/day/2020-01-01",
		"/p/main/archive", "/p/main/articles", "/p/main/articles?unread=1", "/p/main/feeds",
		"/p/main/settings", "/p/main/runs", "/p/main/feeds/export.opml", "/settings", "/profiles",
		"/api/status", "/static/style.css"} {
		code, body := get(t, ts, p)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d: %.300s", p, code, body)
		}
	}
	_, body := get(t, ts, "/p/main/")
	for _, want := range []string{"掲載記事", "要約です", "<li>まとめ1</li>", "今日のページを作り直す"} {
		if !strings.Contains(body, want) {
			t.Errorf("今日のページに %q がない", want)
		}
	}
	_, body = get(t, ts, "/p/main/articles")
	if strings.Contains(body, "記事 <script>") {
		t.Error("記事タイトルがエスケープされていない")
	}
	if code, _ := get(t, ts, "/p/main/day/not-a-date"); code != http.StatusNotFound {
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
	req, _ := http.NewRequest("POST", ts.URL+"/p/main/feeds/reset", nil)
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

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func post(t *testing.T, ts *httptest.Server, path string, form url.Values) string {
	t.Helper()
	resp, err := noRedirect().PostForm(ts.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.Header.Get("Location")
}

func TestSaveGlobalSettings(t *testing.T) {
	ts, st := newServer(t, "")
	form := url.Values{
		"backend": {"openrouter"}, "model.openrouter": {"openai/gpt-5.1"}, "effort": {""},
		"api_key.openrouter": {"sk-or-test-1234567890"}, "cli_path": {"claude"},
		"timezone": {"Asia/Tokyo"}, "fetch_interval": {"30"}, "articles_per_feed": {"10"},
		"max_failures": {"5"}, "failure_retry_hours": {"24"}, "retention_days": {"30"},
	}
	if loc := post(t, ts, "/settings", form); !strings.Contains(loc, "msg=") {
		t.Fatalf("保存に失敗: %s", loc)
	}
	kv, _ := st.Settings(context.Background())
	s := config.Load(kv)
	if s.Backend != "openrouter" || s.Model("openrouter") != "openai/gpt-5.1" ||
		s.APIKeys["openrouter"] != "sk-or-test-1234567890" || s.Effort != "" || s.WebFetch {
		t.Errorf("保存内容が違う: %+v", s)
	}
	// APIキーは画面に平文で出さない。空欄で再保存してもキーは消えない
	_, body := get(t, ts, "/settings")
	if strings.Contains(body, "sk-or-test-1234567890") {
		t.Error("APIキーが設定画面に平文で出ている")
	}
	form.Del("api_key.openrouter")
	post(t, ts, "/settings", form)
	kv, _ = st.Settings(context.Background())
	if config.Load(kv).APIKeys["openrouter"] == "" {
		t.Error("空欄の保存でAPIキーが消えた")
	}
	// モデル未指定はエラー
	form.Set("model.openrouter", "")
	if loc := post(t, ts, "/settings", form); !strings.Contains(loc, "err=") {
		t.Errorf("モデル未指定が通った: %s", loc)
	}
}

func TestProfilesFlow(t *testing.T) {
	ts, st := newServer(t, "")
	ctx := context.Background()

	loc := post(t, ts, "/profiles", url.Values{"name": {"ニュース"}, "slug": {"news"}, "copy_from": {"main"}})
	if !strings.HasPrefix(loc, "/p/news/settings") {
		t.Fatalf("作成後のリダイレクト: %s", loc)
	}
	if loc := post(t, ts, "/profiles", url.Values{"name": {"重複"}, "slug": {"news"}}); !strings.Contains(loc, "err=") {
		t.Errorf("重複IDが通った: %s", loc)
	}

	loc = post(t, ts, "/p/news/settings", url.Values{
		"name": {"世界のニュース"}, "slug": {"world"}, "select_criteria": {"- 信頼できる通信社を優先"},
		"user_memory": {"経済"}, "pick_count": {"8"}, "hours_back": {"12"}, "overview": {"1"},
		"digest_times": {"18:00, 07:30"}, "discord_embeds": {"5"}, "discord_title": {"ニュース"},
		"discord_color": {"3498DB"},
	})
	if !strings.HasPrefix(loc, "/p/world/settings") {
		t.Fatalf("ID変更後のリダイレクト: %s", loc)
	}
	p, err := st.ProfileBySlug(ctx, "world")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "世界のニュース" || p.PickCount != 8 || strings.Join(p.DigestTimes, ",") != "07:30,18:00" ||
		p.DiscordColor != 0x3498DB || p.UserMemory != "経済" {
		t.Errorf("保存内容が違う: %+v", p)
	}

	post(t, ts, "/p/world/feeds/add", url.Values{"url": {"https://b.example/feed"}, "title": {"B"}, "category": {"国際"}})
	if f, _ := st.Feeds(ctx, p.ID); len(f) != 1 || f[0].Title != "B" {
		t.Errorf("フィード追加: %+v", f)
	}
	if f, _ := st.Feeds(ctx, 1); len(f) != 0 {
		t.Errorf("別プロファイルにフィードが入った: %+v", f)
	}
	_, body := get(t, ts, "/p/main/")
	if !strings.Contains(body, `href="/p/world/"`) {
		t.Error("ヘッダーにプロファイル切り替えがない")
	}

	// 削除は確認入力が必要
	if loc := post(t, ts, "/p/world/delete", url.Values{"confirm": {"wrong"}}); !strings.Contains(loc, "err=") {
		t.Errorf("確認なしで削除された: %s", loc)
	}
	post(t, ts, "/p/world/delete", url.Values{"confirm": {"world"}})
	if _, err := st.ProfileBySlug(ctx, "world"); err == nil {
		t.Error("削除されていない")
	}
	// 最後の1つは削除できない
	if loc := post(t, ts, "/p/main/delete", url.Values{"confirm": {"main"}}); !strings.Contains(loc, "err=") {
		t.Errorf("最後のプロファイルが削除された: %s", loc)
	}
	if code, _ := get(t, ts, "/p/nothing/"); code != http.StatusOK { // /profiles にリダイレクトされる
		t.Errorf("存在しないプロファイル: %d", code)
	}
}
