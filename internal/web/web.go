// Package web はWeb画面（日付ページ・RSSリーダー・フィード管理・設定）のHTTPハンドラ。
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/kienn-HCl/mekiki/internal/ai"
	"github.com/kienn-HCl/mekiki/internal/app"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/digest"
	"github.com/kienn-HCl/mekiki/internal/extract"
	"github.com/kienn-HCl/mekiki/internal/feeds"
	"github.com/kienn-HCl/mekiki/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	App      *app.App
	Password string // 空ならBasic認証なし
	tmpl     map[string]*template.Template
	loc      atomic.Pointer[time.Location] // 時刻表示に使う設定のタイムゾーン
}

func New(a *app.App, password string) (*Server, error) {
	s := &Server{App: a, Password: password, tmpl: map[string]*template.Template{}}
	s.loc.Store(config.Defaults().Location())
	funcs := template.FuncMap{
		"fmtTime":  s.fmtTime,
		"fmtDate":  fmtDate,
		"ago":      s.ago,
		"snippet":  snippet,
		"overview": renderOverview,
		"usage":    ai.ParseUsageLine,
		"list":     func(v ...string) []string { return v },
		"stageLabel": func(s string) string {
			return map[string]string{
				"feed": "フィード本文", "page": "ページ本文", "fallback": "WebFetch/タイトル", "failed": "失敗",
			}[s]
		},
		"kindLabel": func(s string) string {
			return map[string]string{digest.KindAppend: "追記", digest.KindRebuild: "再作成"}[s]
		},
		"triggerLabel": func(s string) string {
			return map[string]string{digest.TriggerSchedule: "定時", digest.TriggerManual: "手動"}[s]
		},
	}
	pages := []string{"day", "archive", "articles", "feeds", "settings", "runs"}
	for _, p := range pages {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets,
			"templates/layout.html", "templates/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("テンプレート %s: %w", p, err)
		}
		s.tmpl[p] = t
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(assets))
	mux.HandleFunc("GET /{$}", s.today)
	mux.HandleFunc("GET /day/{date}", s.day)
	mux.HandleFunc("GET /archive", s.archive)
	mux.HandleFunc("GET /articles", s.articles)
	mux.HandleFunc("GET /go/{id}", s.goArticle)
	mux.HandleFunc("GET /feeds", s.feedsPage)
	mux.HandleFunc("GET /feeds/export.opml", s.exportOPML)
	mux.HandleFunc("GET /settings", s.settingsPage)
	mux.HandleFunc("GET /runs", s.runs)
	mux.HandleFunc("GET /api/status", s.status)

	mux.HandleFunc("POST /actions/fetch", s.actionFetch)
	mux.HandleFunc("POST /actions/digest", s.actionDigest)
	mux.HandleFunc("POST /actions/rebuild", s.actionRebuild)
	mux.HandleFunc("POST /articles/read", s.actionMarkRead)
	mux.HandleFunc("POST /feeds/add", s.actionAddFeed)
	mux.HandleFunc("POST /feeds/import", s.actionImportOPML)
	mux.HandleFunc("POST /feeds/{id}/update", s.actionUpdateFeed)
	mux.HandleFunc("POST /feeds/{id}/delete", s.actionDeleteFeed)
	mux.HandleFunc("POST /feeds/reset", s.actionResetFailures)
	mux.HandleFunc("POST /settings", s.actionSaveSettings)
	mux.HandleFunc("POST /settings/test", s.actionTestAI)
	return s.auth(sameOrigin(mux))
}

// ---- ミドルウェア ----

func (s *Server) auth(next http.Handler) http.Handler {
	if s.Password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="mekiki", charset="UTF-8"`)
			http.Error(w, "認証が必要です", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin は別サイトからのPOST（CSRF）を拒否する。
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			src := r.Header.Get("Origin")
			if src == "" {
				src = r.Header.Get("Referer")
			}
			if src != "" {
				u, err := url.Parse(src)
				if err != nil || u.Host != r.Host {
					http.Error(w, "不正なリクエスト元です", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 共通 ----

type base struct {
	Title  string
	Nav    string
	Flash  string
	Error  string
	Today  string
	Status app.Status
}

func (s *Server) base(r *http.Request, nav, title string) base {
	cfg, _ := s.App.Settings(r.Context())
	s.loc.Store(cfg.Location())
	return base{
		Title:  title,
		Nav:    nav,
		Flash:  r.URL.Query().Get("msg"),
		Error:  r.URL.Query().Get("err"),
		Today:  time.Now().In(cfg.Location()).Format("2006-01-02"),
		Status: s.App.Status(),
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl[name].Execute(&buf, data); err != nil {
		slog.Error("テンプレート描画失敗", "page", name, "err", err)
		http.Error(w, "描画に失敗しました", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("リクエスト処理失敗", "err", err)
	http.Error(w, "サーバーエラー: "+err.Error(), http.StatusInternalServerError)
}

// back はフォーム送信後の戻り先（同一サイト内のパスのみ）にメッセージ付きでリダイレクトする。
func back(w http.ResponseWriter, r *http.Request, fallback, key, msg string) {
	to := r.FormValue("back")
	if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		to = fallback
	}
	u, _ := url.Parse(to)
	q := u.Query()
	q.Del("msg")
	q.Del("err")
	if msg != "" {
		q.Set(key, msg)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// ---- 日付ページ ----

type runGroup struct {
	Run   store.Run
	Items []store.PageItem
}

type dayData struct {
	base
	Date       string
	IsToday    bool
	Page       store.Page
	Exists     bool
	Groups     []runGroup
	Prev, Next string
	Runs       []store.Run
	Settings   config.Settings
}

func (s *Server) today(w http.ResponseWriter, r *http.Request) {
	s.renderDay(w, r, "")
}

func (s *Server) day(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if _, err := time.Parse("2006-01-02", date); err != nil {
		http.NotFound(w, r)
		return
	}
	s.renderDay(w, r, date)
}

func (s *Server) renderDay(w http.ResponseWriter, r *http.Request, date string) {
	ctx := r.Context()
	b := s.base(r, "day", "")
	if date == "" {
		date = b.Today
	}
	b.Title = fmtDate(date)
	if date != b.Today {
		b.Nav = "archive"
	}
	st := s.App.Store
	page, exists, err := st.Page(ctx, date)
	if err != nil {
		serverError(w, err)
		return
	}
	items, err := st.PageItems(ctx, date)
	if err != nil {
		serverError(w, err)
		return
	}
	runs, err := st.RunsOn(ctx, date)
	if err != nil {
		serverError(w, err)
		return
	}
	runByID := map[int64]store.Run{}
	for _, run := range runs {
		runByID[run.ID] = run
	}
	var groups []runGroup
	for _, it := range items {
		if len(groups) == 0 || groups[len(groups)-1].Run.ID != it.RunID {
			run, ok := runByID[it.RunID]
			if !ok {
				run = store.Run{ID: it.RunID, StartedAt: it.CreatedAt}
			}
			groups = append(groups, runGroup{Run: run})
		}
		g := &groups[len(groups)-1]
		g.Items = append(g.Items, it)
	}
	prev, next, err := st.AdjacentDates(ctx, date)
	if err != nil {
		serverError(w, err)
		return
	}
	if date == b.Today {
		next = ""
	} else if next == "" && date < b.Today {
		next = b.Today
	}
	cfg, _ := s.App.Settings(ctx)
	s.render(w, "day", dayData{
		base: b, Date: date, IsToday: date == b.Today, Page: page, Exists: exists,
		Groups: groups, Prev: prev, Next: next, Runs: runs, Settings: cfg,
	})
}

func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	pages, err := s.App.Store.Pages(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	s.render(w, "archive", struct {
		base
		Pages []store.Page
	}{s.base(r, "archive", "アーカイブ"), pages})
}

// ---- RSSリーダー ----

const articlesPerPage = 50

type feedGroup struct {
	Category string
	Feeds    []store.Feed
	Unread   int
}

func groupFeeds(list []store.Feed) []feedGroup {
	var out []feedGroup
	for _, f := range list {
		if len(out) == 0 || out[len(out)-1].Category != f.Category {
			out = append(out, feedGroup{Category: f.Category})
		}
		g := &out[len(out)-1]
		g.Feeds = append(g.Feeds, f)
		g.Unread += f.UnreadCount
	}
	return out
}

func (s *Server) articles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	feedID, _ := strconv.ParseInt(q.Get("feed"), 10, 64)
	category := q.Get("category")
	unread := q.Get("unread") == "1"
	pageNo, _ := strconv.Atoi(q.Get("page"))
	pageNo = max(pageNo, 1)

	list, err := s.App.Store.Articles(ctx, store.ArticleFilter{
		FeedID: feedID, Category: category, UnreadOnly: unread,
		Limit: articlesPerPage + 1, Offset: (pageNo - 1) * articlesPerPage,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	hasNext := len(list) > articlesPerPage
	if hasNext {
		list = list[:articlesPerPage]
	}
	allFeeds, err := s.App.Store.Feeds(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	title := "RSS"
	for _, f := range allFeeds {
		if f.ID == feedID {
			title = f.Title
		}
	}
	if category != "" {
		title = category
	}
	link := func(p int) string {
		v := url.Values{}
		if feedID > 0 {
			v.Set("feed", strconv.FormatInt(feedID, 10))
		}
		if category != "" {
			v.Set("category", category)
		}
		if unread {
			v.Set("unread", "1")
		}
		if p > 1 {
			v.Set("page", strconv.Itoa(p))
		}
		if len(v) == 0 {
			return "/articles"
		}
		return "/articles?" + v.Encode()
	}
	toggle := url.Values{}
	if feedID > 0 {
		toggle.Set("feed", strconv.FormatInt(feedID, 10))
	}
	if category != "" {
		toggle.Set("category", category)
	}
	if !unread {
		toggle.Set("unread", "1")
	}
	data := struct {
		base
		Articles   []store.Article
		Groups     []feedGroup
		FeedID     int64
		Category   string
		Unread     bool
		Page       int
		PrevURL    string
		NextURL    string
		ToggleURL  string
		CurrentURL string
	}{
		base: s.base(r, "articles", title), Articles: list, Groups: groupFeeds(allFeeds),
		FeedID: feedID, Category: category, Unread: unread, Page: pageNo,
		ToggleURL: "/articles?" + toggle.Encode(), CurrentURL: link(pageNo),
	}
	if pageNo > 1 {
		data.PrevURL = link(pageNo - 1)
	}
	if hasNext {
		data.NextURL = link(pageNo + 1)
	}
	s.render(w, "articles", data)
}

// goArticle は記事を既読にしてから元記事へ飛ばす。
func (s *Server) goArticle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.App.Store.Article(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = s.App.Store.MarkRead(r.Context(), id)
	u, err := url.Parse(a.Link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "記事URLが不正です", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, a.Link, http.StatusFound)
}

func (s *Server) actionMarkRead(w http.ResponseWriter, r *http.Request) {
	feedID, _ := strconv.ParseInt(r.FormValue("feed"), 10, 64)
	if err := s.App.Store.MarkAllRead(r.Context(), feedID); err != nil {
		serverError(w, err)
		return
	}
	back(w, r, "/articles", "msg", "既読にしました")
}

// ---- フィード管理 ----

func (s *Server) feedsPage(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.Store.Feeds(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	cfg, _ := s.App.Settings(r.Context())
	var cats []string
	seen := map[string]bool{}
	for _, f := range list {
		if !seen[f.Category] {
			seen[f.Category] = true
			cats = append(cats, f.Category)
		}
	}
	s.render(w, "feeds", struct {
		base
		Groups      []feedGroup
		Count       int
		Categories  []string
		MaxFailures int
	}{s.base(r, "feeds", "フィード管理"), groupFeeds(list), len(list), cats, cfg.MaxFailures})
}

func (s *Server) actionAddFeed(w http.ResponseWriter, r *http.Request) {
	feedURL := strings.TrimSpace(r.FormValue("url"))
	title := strings.TrimSpace(r.FormValue("title"))
	category := strings.TrimSpace(r.FormValue("category"))
	u, err := url.Parse(feedURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		back(w, r, "/feeds", "err", "フィードURLが不正です")
		return
	}
	if title == "" {
		title = probeFeedTitle(r.Context(), feedURL)
		if title == "" {
			title = u.Host
		}
	}
	added, err := s.App.Store.AddFeed(r.Context(), title, feedURL, category)
	if err != nil {
		serverError(w, err)
		return
	}
	if !added {
		back(w, r, "/feeds", "err", "そのURLは登録済みです")
		return
	}
	back(w, r, "/feeds", "msg", "「"+title+"」を追加しました。次回の取得から記事が集まります")
}

// probeFeedTitle はタイトル未入力のフィード追加時に、フィード自体からタイトルを取る。
func probeFeedTitle(ctx context.Context, feedURL string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p := gofeed.NewParser()
	p.UserAgent = feeds.UserAgent
	f, err := p.ParseURLWithContext(feedURL, ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(extract.HTMLToText(f.Title))
}

func (s *Server) actionImportOPML(w http.ResponseWriter, r *http.Request) {
	file, _, err := r.FormFile("opml")
	if err != nil {
		back(w, r, "/feeds", "err", "OPMLファイルを選択してください")
		return
	}
	defer file.Close()
	list, err := feeds.ParseOPML(io.LimitReader(file, 5<<20))
	if err != nil {
		back(w, r, "/feeds", "err", err.Error())
		return
	}
	added := 0
	for _, f := range list {
		if f.URL == "" {
			continue
		}
		ok, err := s.App.Store.AddFeed(r.Context(), f.Title, f.URL, f.Category)
		if err != nil {
			serverError(w, err)
			return
		}
		if ok {
			added++
		}
	}
	back(w, r, "/feeds", "msg", fmt.Sprintf("OPMLから%d件を追加しました（登録済み%d件はスキップ）", added, len(list)-added))
}

func (s *Server) exportOPML(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.Store.Feeds(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="mekiki.opml"`)
	if err := feeds.WriteOPML(w, list); err != nil {
		slog.Error("OPML書き出し失敗", "err", err)
	}
}

func (s *Server) actionUpdateFeed(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		back(w, r, "/feeds", "err", "フィード名を入力してください")
		return
	}
	err := s.App.Store.UpdateFeed(r.Context(), id, title,
		strings.TrimSpace(r.FormValue("category")), r.FormValue("enabled") == "1")
	if err != nil {
		serverError(w, err)
		return
	}
	back(w, r, "/feeds", "msg", "「"+title+"」を更新しました")
}

func (s *Server) actionDeleteFeed(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.App.Store.DeleteFeed(r.Context(), id); err != nil {
		serverError(w, err)
		return
	}
	back(w, r, "/feeds", "msg", "フィードを削除しました")
}

func (s *Server) actionResetFailures(w http.ResponseWriter, r *http.Request) {
	if err := s.App.Store.ResetFeedFailures(r.Context()); err != nil {
		serverError(w, err)
		return
	}
	back(w, r, "/feeds", "msg", "全フィードの失敗カウントをリセットしました")
}

// ---- ジョブ ----

func (s *Server) actionFetch(w http.ResponseWriter, r *http.Request) {
	if err := s.App.StartFetch(); err != nil {
		back(w, r, "/articles", "err", err.Error())
		return
	}
	back(w, r, "/articles", "msg", "フィードの取得を開始しました")
}

func (s *Server) actionDigest(w http.ResponseWriter, r *http.Request) {
	if err := s.App.StartDigest(digest.KindAppend); err != nil {
		back(w, r, "/", "err", err.Error())
		return
	}
	back(w, r, "/", "msg", "要約実行を開始しました（前回からの新着分を今日のページに追記します）")
}

func (s *Server) actionRebuild(w http.ResponseWriter, r *http.Request) {
	if err := s.App.StartDigest(digest.KindRebuild); err != nil {
		back(w, r, "/", "err", err.Error())
		return
	}
	back(w, r, "/", "msg", "今日のページの再作成を開始しました")
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	st := s.App.Status()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"fetching":  st.Fetching,
		"digesting": st.Digesting,
		"busy":      st.Fetching || st.Digesting,
	})
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.Store.Runs(r.Context(), 100)
	if err != nil {
		serverError(w, err)
		return
	}
	s.render(w, "runs", struct {
		base
		Runs []store.Run
	}{s.base(r, "runs", "実行履歴"), list})
}

// ---- 設定 ----

type settingsData struct {
	base
	S            config.Settings
	DigestTimes  string
	KeySource    string // 画面設定 / 環境変数 / 未設定
	KeyHint      string
	DefaultModel string
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.App.Settings(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	d := settingsData{
		base: s.base(r, "settings", "設定"), S: cfg,
		DigestTimes: strings.Join(cfg.DigestTimes, ", "), DefaultModel: config.DefaultModel,
	}
	switch {
	case cfg.APIKey != "":
		d.KeySource, d.KeyHint = "画面で設定済み", maskKey(cfg.APIKey)
	case cfg.EffectiveAPIKey() != "":
		d.KeySource, d.KeyHint = "環境変数 ANTHROPIC_API_KEY", maskKey(cfg.EffectiveAPIKey())
	default:
		d.KeySource = "未設定"
	}
	d.S.APIKey = ""
	s.render(w, "settings", d)
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "****"
	}
	return k[:7] + "…" + k[len(k)-4:]
}

func (s *Server) actionSaveSettings(w http.ResponseWriter, r *http.Request) {
	cur, err := s.App.Settings(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	n := func(key string, def int) int {
		v, err := strconv.Atoi(strings.TrimSpace(r.FormValue(key)))
		if err != nil {
			return def
		}
		return v
	}
	times, err := config.ParseTimes(r.FormValue("digest_times"))
	if err != nil {
		back(w, r, "/settings", "err", err.Error())
		return
	}
	color := cur.DiscordColor
	if c := strings.TrimPrefix(strings.TrimSpace(r.FormValue("discord_color")), "#"); c != "" {
		if v, err := strconv.ParseInt(c, 16, 32); err == nil {
			color = int(v)
		}
	}
	next := cur
	next.Backend = r.FormValue("backend")
	next.Model = strings.TrimSpace(r.FormValue("model"))
	next.Effort = r.FormValue("effort")
	next.CLIPath = strings.TrimSpace(r.FormValue("cli_path"))
	next.CLIModel = strings.TrimSpace(r.FormValue("cli_model"))
	next.WebFetch = r.FormValue("web_fetch") == "1"
	next.SelectCriteria = strings.TrimSpace(r.FormValue("select_criteria"))
	next.UserMemory = strings.TrimSpace(r.FormValue("user_memory"))
	next.PickCount = n("pick_count", cur.PickCount)
	next.HoursBack = n("hours_back", cur.HoursBack)
	next.Overview = r.FormValue("overview") == "1"
	next.Timezone = strings.TrimSpace(r.FormValue("timezone"))
	next.FetchInterval = n("fetch_interval", cur.FetchInterval)
	next.DigestTimes = times
	next.ArticlesPerFeed = n("articles_per_feed", cur.ArticlesPerFeed)
	next.MaxFailures = n("max_failures", cur.MaxFailures)
	next.FailureRetryHours = n("failure_retry_hours", cur.FailureRetryHours)
	next.RetentionDays = n("retention_days", cur.RetentionDays)
	next.DiscordEmbeds = n("discord_embeds", cur.DiscordEmbeds)
	next.DiscordTitle = strings.TrimSpace(r.FormValue("discord_title"))
	next.DiscordColor = color
	next.DiscordBaseURL = strings.TrimSpace(r.FormValue("discord_base_url"))
	next.DiscordAfterFail = r.FormValue("discord_after_fail") == "1"
	if v := strings.TrimSpace(r.FormValue("discord_webhook")); v != "" {
		next.DiscordWebhook = v
	}
	if r.FormValue("discord_clear") == "1" {
		next.DiscordWebhook = ""
	}
	if v := strings.TrimSpace(r.FormValue("api_key")); v != "" {
		next.APIKey = v
	}
	if r.FormValue("api_key_clear") == "1" {
		next.APIKey = ""
	}
	if next.CLIPath == "" {
		next.CLIPath = "claude"
	}
	if next.DiscordWebhook != "" && !strings.HasPrefix(next.DiscordWebhook, "https://") {
		back(w, r, "/settings", "err", "Discord Webhook URLは https:// で始まるURLを指定してください")
		return
	}
	if err := next.Validate(); err != nil {
		back(w, r, "/settings", "err", err.Error())
		return
	}
	if err := s.App.Store.SetSettings(r.Context(), next.Map()); err != nil {
		serverError(w, err)
		return
	}
	back(w, r, "/settings", "msg", "設定を保存しました")
}

// actionTestAI は保存済みの設定でClaudeに短い問い合わせをして疎通を確認する。
func (s *Server) actionTestAI(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.App.Settings(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	backend, err := app.NewBackend(cfg)
	if err != nil {
		back(w, r, "/settings", "err", "接続テスト失敗: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	resp, err := backend.Complete(ctx, ai.Request{
		Stage: "test", Prompt: "接続テストです。「OK」とだけ返してください。", Timeout: 2 * time.Minute,
	})
	if err != nil {
		back(w, r, "/settings", "err", "接続テスト失敗（"+backend.Name()+"）: "+err.Error())
		return
	}
	back(w, r, "/settings", "msg", fmt.Sprintf("接続テスト成功（%s）: %s", backend.Name(), snippet(resp.Text, 100)))
}

// ---- テンプレート関数 ----

var weekdays = []string{"日", "月", "火", "水", "木", "金", "土"}

func fmtDate(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return fmt.Sprintf("%d年%d月%d日(%s)", t.Year(), t.Month(), t.Day(), weekdays[t.Weekday()])
}

func (s *Server) fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(s.loc.Load()).Format("2006/01/02 15:04")
}

func (s *Server) ago(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "たった今"
	case d < time.Hour:
		return fmt.Sprintf("%d分前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d時間前", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d日前", int(d.Hours()/24))
	}
	return t.In(s.loc.Load()).Format("2006/01/02")
}

func snippet(s string, n int) string {
	text := strings.Join(strings.Fields(extract.HTMLToText(s)), " ")
	r := []rune(text)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return text
}

// renderOverview は「今日のまとめ」の箇条書きテキストをHTMLにする（エスケープ済み）。
func renderOverview(text string) template.HTML {
	var b strings.Builder
	inList := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if item, ok := strings.CutPrefix(line, "- "); ok || strings.HasPrefix(line, "・") {
			if !ok {
				item = strings.TrimPrefix(line, "・")
			}
			if !inList {
				b.WriteString("<ul>")
				inList = true
			}
			b.WriteString("<li>" + template.HTMLEscapeString(stripBold(item)) + "</li>")
			continue
		}
		if inList {
			b.WriteString("</ul>")
			inList = false
		}
		b.WriteString("<p>" + template.HTMLEscapeString(stripBold(line)) + "</p>")
	}
	if inList {
		b.WriteString("</ul>")
	}
	return template.HTML(b.String())
}

func stripBold(s string) string { return strings.ReplaceAll(s, "**", "") }
