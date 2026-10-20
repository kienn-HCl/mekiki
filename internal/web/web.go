// Package web はWeb画面（日付ページ・RSSリーダー・フィード管理・設定）のHTTPハンドラ。
//
// プロファイルごとの画面は /p/{slug}/... 、サーバ全体の設定は /settings、プロファイル一覧は /profiles。
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
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
	User     string // Basic認証のユーザー名
	Password string // 空ならBasic認証なし
	tmpl     map[string]*template.Template
	loc      atomic.Pointer[time.Location] // 時刻表示に使う設定のタイムゾーン
}

func New(a *app.App, user, password string) (*Server, error) {
	s := &Server{App: a, User: user, Password: password, tmpl: map[string]*template.Template{}}
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
	pages := []string{"day", "archive", "articles", "feeds", "settings", "profile", "profiles", "runs"}
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
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /go/{id}", s.goArticle)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /settings", s.settingsPage)
	mux.HandleFunc("POST /settings", s.actionSaveSettings)
	mux.HandleFunc("POST /settings/test", s.actionTestAI)
	mux.HandleFunc("GET /profiles", s.profilesPage)
	mux.HandleFunc("POST /profiles", s.actionCreateProfile)
	mux.HandleFunc("POST /actions/fetch", s.actionFetch)

	p := func(pattern string, h profileHandler) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" /p/{slug}"+path, s.withProfile(h))
	}
	p("GET /{$}", s.today)
	p("GET /day/{date}", s.day)
	p("GET /archive", s.archive)
	p("GET /articles", s.articles)
	p("GET /feeds", s.feedsPage)
	p("GET /feeds/export.opml", s.exportOPML)
	p("GET /runs", s.runs)
	p("GET /settings", s.profilePage)
	p("POST /settings", s.actionSaveProfile)
	p("POST /delete", s.actionDeleteProfile)
	p("POST /actions/digest", s.actionDigest)
	p("POST /actions/rebuild", s.actionRebuild)
	p("POST /articles/read", s.actionMarkRead)
	p("POST /feeds/add", s.actionAddFeed)
	p("POST /feeds/import", s.actionImportOPML)
	p("POST /feeds/{id}/update", s.actionUpdateFeed)
	p("POST /feeds/{id}/delete", s.actionDeleteFeed)
	p("POST /feeds/reset", s.actionResetFailures)
	return s.auth(sameOrigin(mux))
}

type profileHandler func(w http.ResponseWriter, r *http.Request, p config.Profile)

func (s *Server) withProfile(h profileHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.App.Store.ProfileBySlug(r.Context(), r.PathValue("slug"))
		if errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, "/profiles?err="+url.QueryEscape("プロファイルが見つかりません"), http.StatusSeeOther)
			return
		}
		if err != nil {
			serverError(w, err)
			return
		}
		h(w, r, p)
	}
}

func prefix(p config.Profile) string { return "/p/" + p.Slug }

// home は最初のプロファイルの今日のページへ飛ばす。
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.Store.Profiles(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if len(list) == 0 {
		http.Redirect(w, r, "/profiles", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, prefix(list[0])+"/", http.StatusSeeOther)
}

// ---- ミドルウェア ----

func (s *Server) auth(next http.Handler) http.Handler {
	if s.Password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		// 両方を必ず比較して、どちらが違ったかを応答時間から推測させない
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.User)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.Password)) == 1
		if !ok || !userOK || !passOK {
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
	Title    string
	Nav      string
	Flash    string
	Error    string
	Today    string
	Status   app.Status
	Profile  config.Profile   // 表示中のプロファイル（全体画面ではゼロ値）
	Profiles []config.Profile // 切り替え用
	Prefix   string           // /p/{slug}
	Busy     string           // 実行中ジョブの説明
}

func (s *Server) base(r *http.Request, p config.Profile, nav, title string) base {
	ctx := r.Context()
	cfg, _ := s.App.Settings(ctx)
	s.loc.Store(cfg.Location())
	profiles, _ := s.App.Store.Profiles(ctx)
	b := base{
		Title:    title,
		Nav:      nav,
		Flash:    r.URL.Query().Get("msg"),
		Error:    r.URL.Query().Get("err"),
		Today:    time.Now().In(cfg.Location()).Format("2006-01-02"),
		Status:   s.App.Status(),
		Profile:  p,
		Profiles: profiles,
	}
	if p.ID != 0 {
		b.Prefix = prefix(p)
	}
	switch {
	case b.Status.Digesting:
		name := ""
		for _, pr := range profiles {
			if pr.ID == b.Status.DigestingProfile {
				name = "「" + pr.Name + "」の"
			}
		}
		b.Busy = name + "要約実行中です。完了すると自動で再読み込みします…"
	case b.Status.Fetching:
		b.Busy = "フィードを取得中です…"
	}
	return b
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
	HasFeeds   bool
}

func (s *Server) today(w http.ResponseWriter, r *http.Request, p config.Profile) {
	s.renderDay(w, r, p, "")
}

func (s *Server) day(w http.ResponseWriter, r *http.Request, p config.Profile) {
	date := r.PathValue("date")
	if _, err := time.Parse("2006-01-02", date); err != nil {
		http.NotFound(w, r)
		return
	}
	s.renderDay(w, r, p, date)
}

func (s *Server) renderDay(w http.ResponseWriter, r *http.Request, p config.Profile, date string) {
	ctx := r.Context()
	b := s.base(r, p, "day", "")
	if date == "" {
		date = b.Today
	}
	b.Title = p.Name + " " + fmtDate(date)
	if date != b.Today {
		b.Nav = "archive"
	}
	st := s.App.Store
	page, exists, err := st.Page(ctx, p.ID, date)
	if err != nil {
		serverError(w, err)
		return
	}
	items, err := st.PageItems(ctx, p.ID, date)
	if err != nil {
		serverError(w, err)
		return
	}
	runs, err := st.RunsOn(ctx, p.ID, date)
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
	prev, next, err := st.AdjacentDates(ctx, p.ID, date)
	if err != nil {
		serverError(w, err)
		return
	}
	if date == b.Today {
		next = ""
	} else if next == "" && date < b.Today {
		next = b.Today
	}
	feedList, _ := st.Feeds(ctx, p.ID)
	s.render(w, "day", dayData{
		base: b, Date: date, IsToday: date == b.Today, Page: page, Exists: exists,
		Groups: groups, Prev: prev, Next: next, Runs: runs, HasFeeds: len(feedList) > 0,
	})
}

func (s *Server) archive(w http.ResponseWriter, r *http.Request, p config.Profile) {
	pages, err := s.App.Store.Pages(r.Context(), p.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	s.render(w, "archive", struct {
		base
		Pages []store.Page
	}{s.base(r, p, "archive", p.Name+" アーカイブ"), pages})
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

func (s *Server) articles(w http.ResponseWriter, r *http.Request, p config.Profile) {
	ctx := r.Context()
	q := r.URL.Query()
	feedID, _ := strconv.ParseInt(q.Get("feed"), 10, 64)
	category := q.Get("category")
	unread := q.Get("unread") == "1"
	pageNo, _ := strconv.Atoi(q.Get("page"))
	pageNo = max(pageNo, 1)

	list, err := s.App.Store.Articles(ctx, store.ArticleFilter{
		ProfileID: p.ID, FeedID: feedID, Category: category, UnreadOnly: unread,
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
	allFeeds, err := s.App.Store.Feeds(ctx, p.ID)
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
	link := func(n int) string {
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
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		if len(v) == 0 {
			return prefix(p) + "/articles"
		}
		return prefix(p) + "/articles?" + v.Encode()
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
		base: s.base(r, p, "articles", title), Articles: list, Groups: groupFeeds(allFeeds),
		FeedID: feedID, Category: category, Unread: unread, Page: pageNo,
		ToggleURL: prefix(p) + "/articles?" + toggle.Encode(), CurrentURL: link(pageNo),
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

func (s *Server) actionMarkRead(w http.ResponseWriter, r *http.Request, p config.Profile) {
	feedID, _ := strconv.ParseInt(r.FormValue("feed"), 10, 64)
	if err := s.App.Store.MarkAllRead(r.Context(), p.ID, feedID); err != nil {
		serverError(w, err)
		return
	}
	back(w, r, prefix(p)+"/articles", "msg", "既読にしました")
}

// ---- フィード管理 ----

func (s *Server) feedsPage(w http.ResponseWriter, r *http.Request, p config.Profile) {
	list, err := s.App.Store.Feeds(r.Context(), p.ID)
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
	}{s.base(r, p, "feeds", p.Name+" フィード管理"), groupFeeds(list), len(list), cats, cfg.MaxFailures})
}

func (s *Server) actionAddFeed(w http.ResponseWriter, r *http.Request, p config.Profile) {
	fallback := prefix(p) + "/feeds"
	feedURL := strings.TrimSpace(r.FormValue("url"))
	title := strings.TrimSpace(r.FormValue("title"))
	category := strings.TrimSpace(r.FormValue("category"))
	u, err := url.Parse(feedURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		back(w, r, fallback, "err", "フィードURLが不正です")
		return
	}
	if title == "" {
		title = probeFeedTitle(r.Context(), feedURL)
		if title == "" {
			title = u.Host
		}
	}
	added, err := s.App.Store.AddFeed(r.Context(), p.ID, title, feedURL, category)
	if err != nil {
		serverError(w, err)
		return
	}
	if !added {
		back(w, r, fallback, "err", "そのURLはこのプロファイルに登録済みです")
		return
	}
	back(w, r, fallback, "msg", "「"+title+"」を追加しました。次回の取得から記事が集まります")
}

// probeFeedTitle はタイトル未入力のフィード追加時に、フィード自体からタイトルを取る。
func probeFeedTitle(ctx context.Context, feedURL string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fp := gofeed.NewParser()
	fp.UserAgent = feeds.UserAgent
	f, err := fp.ParseURLWithContext(feedURL, ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(extract.HTMLToText(f.Title))
}

func (s *Server) actionImportOPML(w http.ResponseWriter, r *http.Request, p config.Profile) {
	fallback := prefix(p) + "/feeds"
	file, _, err := r.FormFile("opml")
	if err != nil {
		back(w, r, fallback, "err", "OPMLファイルを選択してください")
		return
	}
	defer file.Close()
	list, err := feeds.ParseOPML(io.LimitReader(file, 5<<20))
	if err != nil {
		back(w, r, fallback, "err", err.Error())
		return
	}
	added, err := importFeeds(r.Context(), s.App.Store, p.ID, list)
	if err != nil {
		serverError(w, err)
		return
	}
	back(w, r, fallback, "msg", fmt.Sprintf("OPMLから%d件を追加しました（登録済み%d件はスキップ）", added, len(list)-added))
}

func importFeeds(ctx context.Context, st *store.Store, profileID int64, list []feeds.OPMLFeed) (int, error) {
	added := 0
	for _, f := range list {
		if f.URL == "" {
			continue
		}
		ok, err := st.AddFeed(ctx, profileID, f.Title, f.URL, f.Category)
		if err != nil {
			return added, err
		}
		if ok {
			added++
		}
	}
	return added, nil
}

func (s *Server) exportOPML(w http.ResponseWriter, r *http.Request, p config.Profile) {
	list, err := s.App.Store.Feeds(r.Context(), p.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="mekiki-%s.opml"`, p.Slug))
	if err := feeds.WriteOPML(w, list); err != nil {
		slog.Error("OPML書き出し失敗", "err", err)
	}
}

func (s *Server) actionUpdateFeed(w http.ResponseWriter, r *http.Request, p config.Profile) {
	fallback := prefix(p) + "/feeds"
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		back(w, r, fallback, "err", "フィード名を入力してください")
		return
	}
	err := s.App.Store.UpdateFeed(r.Context(), p.ID, id, title,
		strings.TrimSpace(r.FormValue("category")), r.FormValue("enabled") == "1")
	if err != nil {
		serverError(w, err)
		return
	}
	back(w, r, fallback, "msg", "「"+title+"」を更新しました")
}

func (s *Server) actionDeleteFeed(w http.ResponseWriter, r *http.Request, p config.Profile) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.App.Store.DeleteFeed(r.Context(), p.ID, id); err != nil {
		serverError(w, err)
		return
	}
	back(w, r, prefix(p)+"/feeds", "msg", "フィードを削除しました")
}

func (s *Server) actionResetFailures(w http.ResponseWriter, r *http.Request, p config.Profile) {
	if err := s.App.Store.ResetFeedFailures(r.Context(), p.ID); err != nil {
		serverError(w, err)
		return
	}
	back(w, r, prefix(p)+"/feeds", "msg", "このプロファイルのフィードの失敗カウントをリセットしました")
}

// ---- ジョブ ----

func (s *Server) actionFetch(w http.ResponseWriter, r *http.Request) {
	if err := s.App.StartFetch(); err != nil {
		back(w, r, "/", "err", err.Error())
		return
	}
	back(w, r, "/", "msg", "フィードの取得を開始しました（全プロファイル）")
}

func (s *Server) actionDigest(w http.ResponseWriter, r *http.Request, p config.Profile) {
	if err := s.App.StartDigest(p.ID, digest.KindAppend); err != nil {
		back(w, r, prefix(p)+"/", "err", err.Error())
		return
	}
	back(w, r, prefix(p)+"/", "msg", "要約実行を開始しました（前回からの新着分を今日のページに追記します）")
}

func (s *Server) actionRebuild(w http.ResponseWriter, r *http.Request, p config.Profile) {
	if err := s.App.StartDigest(p.ID, digest.KindRebuild); err != nil {
		back(w, r, prefix(p)+"/", "err", err.Error())
		return
	}
	back(w, r, prefix(p)+"/", "msg", "今日のページの再作成を開始しました")
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

func (s *Server) runs(w http.ResponseWriter, r *http.Request, p config.Profile) {
	list, err := s.App.Store.Runs(r.Context(), p.ID, 100)
	if err != nil {
		serverError(w, err)
		return
	}
	s.render(w, "runs", struct {
		base
		Runs []store.Run
	}{s.base(r, p, "runs", p.Name+" 実行履歴"), list})
}

// ---- プロファイル ----

func (s *Server) profilesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b := s.base(r, config.Profile{}, "profiles", "プロファイル")
	type row struct {
		config.Profile
		Feeds int
		Pages int
	}
	var rows []row
	for _, p := range b.Profiles {
		fl, _ := s.App.Store.Feeds(ctx, p.ID)
		pl, _ := s.App.Store.Pages(ctx, p.ID)
		rows = append(rows, row{p, len(fl), len(pl)})
	}
	s.render(w, "profiles", struct {
		base
		Rows []row
	}{b, rows})
}

func (s *Server) actionCreateProfile(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(strings.TrimSpace(r.FormValue("slug")))
	name := strings.TrimSpace(r.FormValue("name"))
	p := config.DefaultProfile(slug, name)
	if from := r.FormValue("copy_from"); from != "" {
		// 既存プロファイルの設定（選定基準・興味・時刻・Discord以外）を引き継ぐ
		if src, err := s.App.Store.ProfileBySlug(r.Context(), from); err == nil {
			p.SelectCriteria, p.UserMemory = src.SelectCriteria, src.UserMemory
			p.PickCount, p.HoursBack, p.Overview = src.PickCount, src.HoursBack, src.Overview
			p.DigestTimes = src.DigestTimes
		}
	}
	if err := p.Validate(); err != nil {
		back(w, r, "/profiles", "err", err.Error())
		return
	}
	if _, err := s.App.Store.CreateProfile(r.Context(), p); err != nil {
		back(w, r, "/profiles", "err", err.Error())
		return
	}
	http.Redirect(w, r, prefix(p)+"/settings?msg="+url.QueryEscape(
		"プロファイル「"+p.Name+"」を作成しました。選定基準などを設定し、フィードを追加してください"), http.StatusSeeOther)
}

func (s *Server) profilePage(w http.ResponseWriter, r *http.Request, p config.Profile) {
	s.render(w, "profile", struct {
		base
		P           config.Profile
		DigestTimes string
	}{s.base(r, p, "profile", p.Name+" プロファイル設定"), p, strings.Join(p.DigestTimes, ", ")})
}

func (s *Server) actionSaveProfile(w http.ResponseWriter, r *http.Request, p config.Profile) {
	fallback := prefix(p) + "/settings"
	n := func(key string, def int) int {
		v, err := strconv.Atoi(strings.TrimSpace(r.FormValue(key)))
		if err != nil {
			return def
		}
		return v
	}
	times, err := config.ParseTimes(r.FormValue("digest_times"))
	if err != nil {
		back(w, r, fallback, "err", err.Error())
		return
	}
	next := p
	next.Slug = strings.ToLower(strings.TrimSpace(r.FormValue("slug")))
	next.Name = strings.TrimSpace(r.FormValue("name"))
	next.SelectCriteria = strings.TrimSpace(r.FormValue("select_criteria"))
	next.UserMemory = strings.TrimSpace(r.FormValue("user_memory"))
	next.PickCount = n("pick_count", p.PickCount)
	next.HoursBack = n("hours_back", p.HoursBack)
	next.Overview = r.FormValue("overview") == "1"
	next.DigestTimes = times
	next.DiscordEmbeds = n("discord_embeds", p.DiscordEmbeds)
	next.DiscordTitle = strings.TrimSpace(r.FormValue("discord_title"))
	if c := strings.TrimPrefix(strings.TrimSpace(r.FormValue("discord_color")), "#"); c != "" {
		if v, err := strconv.ParseInt(c, 16, 32); err == nil {
			next.DiscordColor = int(v)
		}
	}
	next.DiscordBaseURL = strings.TrimSpace(r.FormValue("discord_base_url"))
	next.DiscordAfterFail = r.FormValue("discord_after_fail") == "1"
	if v := strings.TrimSpace(r.FormValue("discord_webhook")); v != "" {
		next.DiscordWebhook = v
	}
	if r.FormValue("discord_clear") == "1" {
		next.DiscordWebhook = ""
	}
	if err := next.Validate(); err != nil {
		back(w, r, fallback, "err", err.Error())
		return
	}
	if err := s.App.Store.UpdateProfile(r.Context(), next); err != nil {
		back(w, r, fallback, "err", err.Error())
		return
	}
	http.Redirect(w, r, prefix(next)+"/settings?msg="+url.QueryEscape("プロファイル設定を保存しました"), http.StatusSeeOther)
}

func (s *Server) actionDeleteProfile(w http.ResponseWriter, r *http.Request, p config.Profile) {
	list, err := s.App.Store.Profiles(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if len(list) <= 1 {
		back(w, r, prefix(p)+"/settings", "err", "最後のプロファイルは削除できません")
		return
	}
	if r.FormValue("confirm") != p.Slug {
		back(w, r, prefix(p)+"/settings", "err", "削除するには確認欄にプロファイルIDを入力してください")
		return
	}
	if id, running := s.App.Digest.Running(); running && id == p.ID {
		back(w, r, prefix(p)+"/settings", "err", "要約実行中のため削除できません")
		return
	}
	if err := s.App.Store.DeleteProfile(r.Context(), p.ID); err != nil {
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/profiles?msg="+url.QueryEscape("プロファイル「"+p.Name+"」を削除しました"), http.StatusSeeOther)
}

// ---- 全体設定 ----

type providerView struct {
	config.Provider
	Model     string
	Effort    string
	UseFetch  bool
	KeySource string // 画面で設定済み / 環境変数 XXX / 未設定
	KeyHint   string
}

type settingsData struct {
	base
	S         config.Settings
	Providers []providerView
	Timezones []tzOption
}

type tzOption struct{ Name, Label string }

// timezoneOptions はタイムゾーンの選択肢を現在のUTCとの時差付きで返す。
// 一覧にないタイムゾーンが設定されていれば先頭に足す。
func timezoneOptions(current string) []tzOption {
	names := config.Timezones
	if !slices.Contains(names, current) {
		names = append([]string{current}, names...)
	}
	now := time.Now()
	var out []tzOption
	for _, n := range names {
		loc, err := time.LoadLocation(n)
		if err != nil {
			continue
		}
		_, off := now.In(loc).Zone()
		sign := "+"
		if off < 0 {
			sign, off = "-", -off
		}
		out = append(out, tzOption{n, fmt.Sprintf("%s（UTC%s%02d:%02d）", n, sign, off/3600, off%3600/60)})
	}
	return out
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.App.Settings(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	d := settingsData{base: s.base(r, config.Profile{}, "settings", "全体設定"), S: cfg,
		Timezones: timezoneOptions(cfg.Timezone)}
	for _, p := range config.Providers {
		v := providerView{Provider: p, Model: cfg.Models[p.ID], Effort: cfg.Effort(p.ID), UseFetch: cfg.UseWebFetch(p.ID)}
		switch {
		case cfg.APIKeys[p.ID] != "":
			v.KeySource, v.KeyHint = "画面で設定済み", maskKey(cfg.APIKeys[p.ID])
		case cfg.APIKey(p.ID) != "":
			v.KeySource, v.KeyHint = "環境変数 "+p.KeyEnv, maskKey(cfg.APIKey(p.ID))
		default:
			v.KeySource = "未設定"
		}
		d.Providers = append(d.Providers, v)
	}
	d.S.APIKeys = nil
	s.render(w, "settings", d)
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "****"
	}
	return k[:6] + "…" + k[len(k)-4:]
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
	next := cur
	next.Backend = r.FormValue("backend")
	next.APIKeys = map[string]string{}
	next.Models = map[string]string{}
	for _, p := range config.Providers {
		next.APIKeys[p.ID] = cur.APIKeys[p.ID]
		if v := strings.TrimSpace(r.FormValue("api_key." + p.ID)); v != "" {
			next.APIKeys[p.ID] = v
		}
		if r.FormValue("api_key_clear."+p.ID) == "1" {
			next.APIKeys[p.ID] = ""
		}
		next.Models[p.ID] = strings.TrimSpace(r.FormValue("model." + p.ID))
	}
	next.CompatURL = strings.TrimSpace(r.FormValue("compat_url"))

	next.CLIPath = strings.TrimSpace(r.FormValue("cli_path"))
	next.CLIModel = strings.TrimSpace(r.FormValue("cli_model"))
	next.Efforts = map[string]string{}
	for _, p := range config.Providers {
		next.Efforts[p.ID] = r.FormValue("effort." + p.ID)
	}
	next.WebFetch = map[string]bool{}
	for _, id := range config.WebFetchBackends() {
		next.WebFetch[id] = r.FormValue("web_fetch."+id) == "1"
	}
	next.Timezone = strings.TrimSpace(r.FormValue("timezone"))
	next.FetchInterval = n("fetch_interval", cur.FetchInterval)
	next.ArticlesPerFeed = n("articles_per_feed", cur.ArticlesPerFeed)
	next.MaxFailures = n("max_failures", cur.MaxFailures)
	next.FailureRetryHours = n("failure_retry_hours", cur.FailureRetryHours)
	next.RetentionDays = n("retention_days", cur.RetentionDays)
	if next.CLIPath == "" {
		next.CLIPath = "claude"
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

// actionTestAI は保存済みの設定でAIに短い問い合わせをして疎通を確認する。
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
		item, ok := strings.CutPrefix(line, "- ")
		if !ok {
			item, ok = strings.CutPrefix(line, "・")
		}
		if ok {
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
