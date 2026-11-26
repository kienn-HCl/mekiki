package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/extract"
	"github.com/kienn-HCl/mekiki/internal/store"
)

// ---- 元記事へのリダイレクト（開いた記録を取る） ----

func redirectOut(w http.ResponseWriter, r *http.Request, link string) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "記事URLが不正です", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, link, http.StatusFound)
}

func (s *Server) goItem(w http.ResponseWriter, r *http.Request, p config.Profile) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	it, err := s.App.Store.PageItem(r.Context(), p.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = s.App.Store.RecordOpen(r.Context(), p.ID, it.NormLink, store.OpenFromPage, time.Now())
	redirectOut(w, r, it.Link)
}

func (s *Server) goSaved(w http.ResponseWriter, r *http.Request, p config.Profile) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	v, err := s.App.Store.SavedItem(r.Context(), p.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = s.App.Store.RecordOpen(r.Context(), p.ID, v.NormLink, store.OpenFromSaved, time.Now())
	redirectOut(w, r, v.Link)
}

func (s *Server) goReaderArticle(w http.ResponseWriter, r *http.Request, p config.Profile) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.App.Store.Article(r.Context(), id)
	if err != nil || !s.feedInProfile(r, p, a.FeedID) {
		http.NotFound(w, r)
		return
	}
	_ = s.App.Store.MarkRead(r.Context(), id)
	_ = s.App.Store.RecordOpen(r.Context(), p.ID, a.NormLink, store.OpenFromReader, time.Now())
	redirectOut(w, r, a.Link)
}

func (s *Server) feedInProfile(r *http.Request, p config.Profile, feedID int64) bool {
	feeds, err := s.App.Store.Feeds(r.Context(), p.ID)
	if err != nil {
		return false
	}
	for _, f := range feeds {
		if f.ID == feedID {
			return true
		}
	}
	return false
}

// ---- お気に入り・後で読む ----

// actionToggleSaved は日付ページの記事（item）・RSSリーダーの記事（article）・保存一覧の項目（saved）を
// お気に入り / 後で読む に入れる、または外す。fetch からは JSON で状態を返す。
func (s *Server) actionToggleSaved(w http.ResponseWriter, r *http.Request, p config.Profile) {
	ctx := r.Context()
	kind := r.FormValue("kind")
	if kind != store.SavedFavorite && kind != store.SavedLater {
		http.Error(w, "kind が不正です", http.StatusBadRequest)
		return
	}
	v := store.Saved{ProfileID: p.ID, Kind: kind}
	id := func(name string) int64 {
		n, _ := strconv.ParseInt(r.FormValue(name), 10, 64)
		return n
	}
	var err error
	switch {
	case r.FormValue("item") != "":
		var it store.PageItem
		if it, err = s.App.Store.PageItem(ctx, p.ID, id("item")); err == nil {
			v.NormLink, v.Title, v.Link, v.Category, v.FeedName, v.Image = it.NormLink, it.Title, it.Link,
				it.Category, it.FeedName, it.Image
			if it.SummaryOK {
				v.Summary = it.Summary
			}
		}
	case r.FormValue("article") != "":
		var a store.Article
		if a, err = s.App.Store.Article(ctx, id("article")); err == nil && !s.feedInProfile(r, p, a.FeedID) {
			err = store.ErrNotFound
		}
		if err == nil {
			v.NormLink, v.Title, v.Link, v.Category, v.FeedName = a.NormLink, a.Title, a.Link, a.Category, a.FeedName
			v.Summary = snippet(a.Body, 180)
			if a.Image != "" {
				v.Image = extract.SanitizeImageURL(a.Image, a.Link)
			}
		}
	case r.FormValue("saved") != "":
		var src store.Saved
		if src, err = s.App.Store.SavedItem(ctx, p.ID, id("saved")); err == nil {
			src.ID, src.Kind = 0, kind
			v = src
		}
	default:
		err = store.ErrNotFound
	}
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	on, err := s.App.Store.ToggleSaved(ctx, v, time.Now())
	if err != nil {
		serverError(w, err)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"on": on})
		return
	}
	fallback := prefix(p) + "/"
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Host == r.Host && strings.HasPrefix(ref.Path, "/") {
		fallback = ref.RequestURI()
	}
	back(w, r, fallback, "", "")
}

func (s *Server) savedPage(w http.ResponseWriter, r *http.Request, p config.Profile) {
	ctx := r.Context()
	kind := r.URL.Query().Get("kind")
	if kind != store.SavedFavorite {
		kind = store.SavedLater
	}
	list, err := s.App.Store.SavedList(ctx, p.ID, kind)
	if err != nil {
		serverError(w, err)
		return
	}
	marks, _ := s.App.Store.SavedMarks(ctx, p.ID)
	counts, _ := s.App.Store.SavedCounts(ctx, p.ID)
	title := "後で読む"
	if kind == store.SavedFavorite {
		title = "お気に入り"
	}
	s.render(w, "saved", struct {
		base
		Kind   string
		Items  []store.Saved
		Marks  map[string]store.SavedMark
		Counts map[string]int
	}{s.base(r, p, "saved", p.Name+" "+title), kind, list, marks, counts})
}
