package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kienn-HCl/mekiki/internal/app"
	"github.com/kienn-HCl/mekiki/internal/conffile"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/store"
)

func newServer(t *testing.T, password string) (*httptest.Server, *store.Store) {
	ts, st, _ := newServerWith(t, password)
	return ts, st
}

func newServerWithConf(t *testing.T) (*httptest.Server, *store.Store, *conffile.Manager) {
	return newServerWith(t, "")
}

func newServerWith(t *testing.T, password string) (*httptest.Server, *store.Store, *conffile.Manager) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := app.New(st)
	a.Conf = conffile.New(st, t.TempDir())
	if err := a.Conf.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv, err := New(a, "me", password)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st, a.Conf
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
	for _, c := range []struct{ user, pass string }{{"me", "wrong"}, {"other", "secret"}} {
		req, _ := http.NewRequest("GET", ts.URL+"/", nil)
		req.SetBasicAuth(c.user, c.pass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s/%s で通った: %d", c.user, c.pass, resp.StatusCode)
		}
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
		"backend": {"openrouter"}, "model.openrouter": {"openai/gpt-5.1"}, "effort.openrouter": {""},
		"effort.anthropic": {"high"}, "web_fetch.cli": {"1"},
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
		s.APIKeys["openrouter"] != "sk-or-test-1234567890" || s.Effort("openrouter") != "" ||
		s.Effort("anthropic") != "high" || !s.UseWebFetch("cli") || s.UseWebFetch("anthropic") {
		t.Errorf("保存内容が違う: %+v", s)
	}
	// APIキーは画面に平文で出さない。空欄で再保存してもキーは消えない
	_, body := get(t, ts, "/settings")
	if strings.Contains(body, "sk-or-test-1234567890") {
		t.Error("APIキーが設定画面に平文で出ている")
	}
	for _, want := range []string{`<option value="Asia/Tokyo" selected>`, `name="web_fetch.cli" value="1" checked`, `<option selected>high</option>`,
		`name="model.openrouter" value="openai/gpt-5.1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("設定画面に %q がない", want)
		}
	}
	if strings.Contains(body, `name="web_fetch.openrouter"`) {
		t.Error("WebFetch非対応のAIにチェックが出ている")
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

func TestDiscordWebhookFormAndTest(t *testing.T) {
	ts, st := newServer(t, "")
	ctx := context.Background()

	var mu sync.Mutex
	var bodies []string
	hook := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()
	// テスト用のTLSサーバの証明書を既定のHTTPクライアントで信頼する
	old := http.DefaultTransport
	http.DefaultTransport = hook.Client().Transport
	defer func() { http.DefaultTransport = old }()

	form := url.Values{
		"name": {"メイン"}, "slug": {"main"}, "select_criteria": {"- 基準"}, "pick_count": {"5"},
		"hours_back": {"24"}, "digest_times": {"07:00"}, "discord_embeds": {"5"},
		"discord_title": {"テスト見出し"}, "discord_color": {"112233"}, "discord_webhook": {hook.URL + "/api/webhooks/1/x"},
	}
	loc := post(t, ts, "/p/main/discord/test", form)
	if !strings.Contains(loc, "msg=") {
		t.Fatalf("テスト投稿に失敗: %s", loc)
	}
	mu.Lock()
	if len(bodies) != 2 || !strings.Contains(bodies[0], "テスト見出し") || !strings.Contains(bodies[1], `"color":1122867`) {
		t.Errorf("送信内容: %v", bodies)
	}
	mu.Unlock()
	if p, _ := st.ProfileBySlug(ctx, "main"); p.DiscordWebhook != "" {
		t.Error("テスト投稿で設定が保存された")
	}

	// 保存すると入力欄に表示され、空欄で保存すると削除される
	post(t, ts, "/p/main/settings", form)
	p, _ := st.ProfileBySlug(ctx, "main")
	if p.DiscordWebhook != hook.URL+"/api/webhooks/1/x" {
		t.Fatalf("Webhookが保存されない: %q", p.DiscordWebhook)
	}
	_, body := get(t, ts, "/p/main/settings")
	if !strings.Contains(body, `value="`+hook.URL+`/api/webhooks/1/x"`) {
		t.Error("保存済みのWebhookが入力欄に出ていない")
	}
	form.Set("discord_webhook", "")
	post(t, ts, "/p/main/settings", form)
	if p, _ := st.ProfileBySlug(ctx, "main"); p.DiscordWebhook != "" {
		t.Error("空欄で保存してもWebhookが消えない")
	}
	if loc := post(t, ts, "/p/main/discord/test", form); !strings.Contains(loc, "err=") {
		t.Errorf("URLなしのテスト投稿が通った: %s", loc)
	}
}

func TestReorderProfilesAndDefault(t *testing.T) {
	ts, st := newServer(t, "")
	ctx := context.Background()
	for _, slug := range []string{"tech", "news"} {
		post(t, ts, "/profiles", url.Values{"name": {slug}, "slug": {slug}})
	}
	order := func() string {
		list, _ := st.Profiles(ctx)
		var s []string
		for _, p := range list {
			s = append(s, p.Slug)
		}
		return strings.Join(s, ",")
	}
	if got := order(); got != "main,tech,news" {
		t.Fatalf("初期順: %s", got)
	}
	post(t, ts, "/profiles/news/move", url.Values{"to": {"up"}})
	if got := order(); got != "main,news,tech" {
		t.Errorf("上へ: %s", got)
	}
	post(t, ts, "/profiles/tech/move", url.Values{"to": {"top"}})
	if got := order(); got != "tech,main,news" {
		t.Errorf("先頭へ: %s", got)
	}
	post(t, ts, "/profiles/tech/move", url.Values{"to": {"up"}}) // 先頭より上はそのまま
	post(t, ts, "/profiles/news/move", url.Values{"to": {"down"}})
	if got := order(); got != "tech,main,news" {
		t.Errorf("端での移動: %s", got)
	}
	// トップページは先頭のプロファイルを開く
	resp, err := noRedirect().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/p/tech/" {
		t.Errorf("トップページの行き先: %s", loc)
	}
	_, body := get(t, ts, "/profiles")
	if !strings.Contains(body, `<span class="tag">既定</span>`) {
		t.Error("既定の表示がない")
	}
}

func TestConfigFileBackupAndRestore(t *testing.T) {
	ts, st, conf := newServerWithConf(t)
	ctx := context.Background()
	post(t, ts, "/p/main/feeds/add", url.Values{"url": {"https://a.example/feed"}, "title": {"A"}})

	// 画面での変更がフィード一覧のOPMLに書き出される
	opml, _ := os.ReadFile(filepath.Join(conf.Dir, "feeds", "main.opml"))
	if !strings.Contains(string(opml), "https://a.example/feed") {
		t.Fatalf("OPMLに書き出されていない:\n%s", opml)
	}
	code, body := get(t, ts, "/backup/config.zip")
	if code != http.StatusOK || !strings.HasPrefix(body, "PK") {
		t.Fatalf("設定一式のダウンロード: %d", code)
	}
	if code, db := get(t, ts, "/backup/mekiki.db"); code != http.StatusOK || !strings.HasPrefix(db, "SQLite format 3") {
		t.Errorf("DBのダウンロード: %d", code)
	}

	upload := func(name string, data []byte) string {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("config", name)
		fw.Write(data)
		mw.Close()
		req, _ := http.NewRequest("POST", ts.URL+"/backup/restore", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := noRedirect().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	// zipから復元（フィードを差し替えたもの）
	b, _ := conffile.ReadZip([]byte(body))
	b["feeds/main.opml"] = bytes.Replace(b["feeds/main.opml"], []byte("a.example"), []byte("b.example"), 1)
	var zbuf bytes.Buffer
	b.Zip(&zbuf)
	if loc := upload("config.zip", zbuf.Bytes()); !strings.Contains(loc, "msg=") {
		t.Fatalf("復元に失敗: %s", loc)
	}
	if feeds, _ := st.Feeds(ctx, 1); len(feeds) != 1 || feeds[0].URL != "https://b.example/feed" {
		t.Errorf("復元後のフィード: %+v", feeds)
	}
	if loc := upload("x.toml", []byte("これはTOMLではない")); !strings.Contains(loc, "err=") {
		t.Errorf("壊れたファイルで復元できてしまう: %s", loc)
	}

	// サーバ上で設定ファイルが編集されたら、再起動するまで画面からの設定変更を止める
	cfg := filepath.Join(conf.Dir, "config.toml")
	data, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, append(data, []byte("\n# サーバで編集\n")...), 0o600)
	if loc := post(t, ts, "/p/main/feeds/add", url.Values{"url": {"https://c.example/feed"}}); !strings.Contains(loc, "err=") {
		t.Errorf("編集中のファイルがあるのに設定を変更できた: %s", loc)
	}
	if after, _ := os.ReadFile(cfg); !strings.Contains(string(after), "# サーバで編集") {
		t.Error("サーバでの編集が上書きされた")
	}
	_, page := get(t, ts, "/settings")
	if !strings.Contains(page, "mekikiを再起動して反映") {
		t.Error("再起動を促す表示がない")
	}
	// 設定に関係ない操作（要約実行・既読など）は止めない
	if loc := post(t, ts, "/p/main/articles/read", url.Values{}); strings.Contains(loc, "err=") {
		t.Errorf("既読操作まで止まった: %s", loc)
	}
}

func TestBuildCalendar(t *testing.T) {
	pages := []store.Page{{Date: "2026-10-20", ItemCount: 5}, {Date: "2026-09-30", ItemCount: 2}}
	c := buildCalendar("2026-10", pages, "2026-10-22")
	if c.Label != "2026年10月" || c.Prev != "2026-09" || c.Next != "" {
		t.Errorf("見出し・前後: %+v", c)
	}
	// 2026-10-01 は木曜
	if len(c.Weeks) != 5 || c.Weeks[0][3].Day != 0 || c.Weeks[0][4].Day != 1 {
		t.Errorf("週の並び: %+v", c.Weeks[0])
	}
	var has, today []int
	for _, w := range c.Weeks {
		if len(w) != 7 {
			t.Fatalf("週が7日でない: %d", len(w))
		}
		for _, d := range w {
			if d.Has {
				has = append(has, d.Day)
			}
			if d.IsToday {
				today = append(today, d.Day)
			}
		}
	}
	if fmt.Sprint(has) != "[20]" || fmt.Sprint(today) != "[22]" {
		t.Errorf("ページのある日 %v・今日 %v", has, today)
	}
	if c := buildCalendar("2026-09", pages, "2026-10-22"); c.Prev != "" || c.Next != "2026-10" {
		t.Errorf("最も古い月: prev=%q next=%q", c.Prev, c.Next)
	}
}

func TestArchiveMonth(t *testing.T) {
	ts, st := newServer(t, "")
	date := seed(t, st)
	_, body := get(t, ts, "/p/main/archive")
	if !strings.Contains(body, `class="cal"`) || !strings.Contains(body, fmtDate(date)) {
		t.Errorf("今月のアーカイブ: %.500s", body)
	}
	_, body = get(t, ts, "/p/main/archive?month=2001-01")
	if !strings.Contains(body, "2001年1月のページはありません") || strings.Contains(body, fmtDate(date)) {
		t.Errorf("ページのない月: %.500s", body)
	}
}

func TestUpdateFeedURL(t *testing.T) {
	ts, st := newServer(t, "")
	ctx := context.Background()
	st.AddFeed(ctx, 1, "A", "https://a.example/rss", "技術")
	st.AddFeed(ctx, 1, "B", "https://b.example/rss", "技術")
	feeds, _ := st.Feeds(ctx, 1)
	a := feeds[0]
	st.MarkFeedFailure(ctx, a.ID, "HTTP 404", time.Now())

	form := url.Values{"title": {"A"}, "category": {"技術"}, "enabled": {"1"}, "url": {"https://b.example/rss"}}
	if loc := post(t, ts, fmt.Sprintf("/p/main/feeds/%d/update", a.ID), form); !strings.Contains(loc, "err=") {
		t.Errorf("登録済みのURLに変更できてしまう: %s", loc)
	}
	form.Set("url", "ftp://a.example/rss")
	if loc := post(t, ts, fmt.Sprintf("/p/main/feeds/%d/update", a.ID), form); !strings.Contains(loc, "err=") {
		t.Errorf("不正なURLに変更できてしまう: %s", loc)
	}
	form.Set("url", "https://a.example/atom.xml")
	if loc := post(t, ts, fmt.Sprintf("/p/main/feeds/%d/update", a.ID), form); strings.Contains(loc, "err=") {
		t.Fatalf("URLを変更できない: %s", loc)
	}
	feeds, _ = st.Feeds(ctx, 1)
	if feeds[0].URL != "https://a.example/atom.xml" || feeds[0].Failures != 0 || feeds[0].LastError != "" {
		t.Errorf("変更後のフィード: %+v", feeds[0])
	}
}

func TestSavedAndOpens(t *testing.T) {
	ts, st := newServer(t, "")
	ctx := context.Background()
	date := seed(t, st)
	items, _ := st.PageItems(ctx, 1, date)
	it := items[0]

	// 後で読むに入れて、もう一度押すと外れる（JSON で状態が返る）
	toggle := func(form url.Values) bool {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/p/main/saved/toggle", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var res struct{ On bool }
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&res) != nil {
			t.Fatalf("toggle %v: %d", form, resp.StatusCode)
		}
		return res.On
	}
	later := url.Values{"item": {fmt.Sprint(it.ID)}, "kind": {"later"}}
	if !toggle(later) || toggle(later) || !toggle(later) {
		t.Error("後で読むの切り替え")
	}
	feeds, _ := st.Feeds(ctx, 1)
	arts, _ := st.Articles(ctx, store.ArticleFilter{ProfileID: 1, FeedID: feeds[0].ID, Limit: 10})
	if !toggle(url.Values{"article": {fmt.Sprint(arts[0].ID)}, "kind": {"favorite"}}) {
		t.Error("RSSの記事をお気に入りにできない")
	}
	_, body := get(t, ts, "/p/main/saved")
	if !strings.Contains(body, "掲載記事") || !strings.Contains(body, `aria-pressed="true"`) {
		t.Errorf("後で読む一覧: %.500s", body)
	}
	_, body = get(t, ts, "/p/main/saved?kind=favorite")
	if !strings.Contains(body, "記事 &lt;script&gt;") {
		t.Errorf("お気に入り一覧: %.500s", body)
	}

	// 日付ページのリンクは開いた記録を取ってから元記事へ
	resp, err := noRedirect().Get(ts.URL + fmt.Sprintf("/p/main/go/item/%d", it.ID))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != it.Link {
		t.Errorf("リダイレクト: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	sig, _ := st.Signals(ctx, 1)
	if g := sig[it.NormLink]; !g.Opened || !g.Later || !g.Favorite {
		t.Errorf("反応: %+v", g)
	}
	_, body = get(t, ts, "/p/main/runs")
	if !strings.Contains(body, "役に立った1件あたり") || !strings.Contains(body, "開 1 / 保 1") {
		t.Errorf("実行履歴の集計: %.800s", body)
	}
	_, body = get(t, ts, "/p/main/")
	if !strings.Contains(body, "/p/main/go/item/") {
		t.Error("日付ページのリンクが記録用のURLになっていない")
	}
}

func TestStatsAPI(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := app.New(st)
	a.Conf = conffile.New(st, t.TempDir())
	if err := a.Conf.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	kv, _ := st.Settings(ctx)
	cfg := config.Load(kv)
	cfg.APIKeys[config.BackendAnthropic] = "sk-ant-secret"
	st.SetSettings(ctx, cfg.Map())
	seed(t, st)
	srv, _ := New(a, "me", "pw")
	srv.APIToken, srv.Version = "tok", "v9"
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	do := func(path, auth string) (int, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		_, _ = io.Copy(&b, resp.Body)
		return resp.StatusCode, b.String()
	}
	if code, _ := do("/api/stats", ""); code != http.StatusUnauthorized {
		t.Errorf("認証なしで読めた: %d", code)
	}
	if code, _ := do("/p/main/", "Bearer tok"); code != http.StatusUnauthorized {
		t.Errorf("トークンで画面まで読めた: %d", code)
	}
	code, body := do("/api/stats?days=7", "Bearer tok")
	if code != 200 {
		t.Fatalf("トークンで読めない: %d %s", code, body)
	}
	if strings.Contains(body, "sk-ant-secret") {
		t.Error("APIキーが含まれている")
	}
	var out struct {
		Version string
		Days    int
		AI      struct {
			Backend string
			KeySet  bool `json:"key_set"`
		}
		Profiles []struct {
			Slug       string
			Summary    struct{ Items int }
			ItemStages map[string]int `json:"item_stages"`
			Feeds      []struct{ Title string }
			Runs       []struct{ ID int64 }
		}
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Version != "v9" || out.Days != 7 || !out.AI.KeySet || len(out.Profiles) != 1 {
		t.Fatalf("内容: %+v", out)
	}
	p := out.Profiles[0]
	if p.Summary.Items != 1 || p.ItemStages["feed"] != 1 || len(p.Feeds) != 1 || len(p.Runs) != 1 {
		t.Errorf("プロファイルの内容: %+v", p)
	}
	if code, _ := do("/api/stats", "Basic "+base64.StdEncoding.EncodeToString([]byte("me:pw"))); code != 200 {
		t.Errorf("Basic認証で読めない: %d", code)
	}
}

func TestEvalAPI(t *testing.T) {
	ts, st := newServer(t, "")
	seed(t, st)
	resp, err := http.PostForm(ts.URL+"/api/eval", url.Values{"kind": {"summarize"}, "profile": {"main"}})
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		ID     int
		Status string
		Output string
	}
	json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || job.ID == 0 {
		t.Fatalf("開始: %d %+v", resp.StatusCode, job)
	}
	for i := 0; i < 200 && job.Status != "done" && job.Status != "failed"; i++ {
		time.Sleep(10 * time.Millisecond)
		_, body := get(t, ts, fmt.Sprintf("/api/eval/%d", job.ID))
		json.Unmarshal([]byte(body), &job)
	}
	if job.Status != "done" || !strings.Contains(job.Output, "要約のやり直し") {
		t.Errorf("結果: %+v", job)
	}
	if code, _ := get(t, ts, "/api/eval/999"); code != http.StatusNotFound {
		t.Errorf("存在しない eval: %d", code)
	}
}

func TestFeedReview(t *testing.T) {
	ts, st := newServer(t, "")
	ctx := context.Background()
	now := time.Now()
	st.AddFeed(ctx, 1, "止まったフィード", "https://dead.example/rss", "")
	st.AddFeed(ctx, 1, "空のフィード", "https://empty.example/rss", "")
	st.AddFeed(ctx, 1, "選ばれないフィード", "https://dull.example/rss", "")
	st.AddFeed(ctx, 1, "元気なフィード", "https://ok.example/rss", "")
	feeds, _ := st.Feeds(ctx, 1)
	for range 5 {
		st.MarkFeedFailure(ctx, feeds[0].ID, "HTTP 404", now)
	}
	st.MarkFeedSuccess(ctx, feeds[1].ID, "", "", now, false)
	runID, _ := st.StartRun(ctx, store.Run{ProfileID: 1, Date: "2026-11-28", Kind: "append", Trigger: "manual", StartedAt: now})
	var cands []store.Article
	for i := range 25 {
		link := fmt.Sprintf("https://dull.example/%d", i)
		cands = append(cands, store.Article{Title: fmt.Sprint("記事", i), Link: link, NormLink: link, FeedName: "選ばれないフィード"})
	}
	st.SaveRunCandidates(ctx, runID, 1, cands, nil)

	_, body := get(t, ts, "/p/main/feeds")
	i := strings.Index(body, "見直し候補")
	if i < 0 {
		t.Fatalf("見直し候補がない: %.500s", body)
	}
	review := body[i:strings.Index(body, `class="cathead"`)]
	for _, want := range []string{"止まったフィード", "連続5回失敗", "HTTP 404", "空のフィード", "選ばれないフィード", "候補に25件"} {
		if !strings.Contains(review, want) {
			t.Errorf("見直し候補に %q がない", want)
		}
	}
	if strings.Contains(review, "元気なフィード") {
		t.Error("問題のないフィードが見直し候補に出ている")
	}
	if strings.Index(review, "止まったフィード") > strings.Index(review, "選ばれないフィード") {
		t.Error("スキップ中のフィードが先に並んでいない")
	}
}
