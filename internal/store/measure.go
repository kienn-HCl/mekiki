package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// ---- 候補の写し・要約の入力（計測・再生ツール用） ----

type Candidate struct {
	Position    int
	Title       string
	Link        string
	NormLink    string
	Category    string
	FeedName    string
	PublishedAt time.Time
	PickedRank  int // 選ばれなければ -1
}

// SaveRunCandidates は選定に渡した候補と、選ばれた順位（picked は候補の index を順位順に並べたもの）を保存する。
func (s *Store) SaveRunCandidates(ctx context.Context, runID, profileID int64, cands []Article, picked []int) error {
	rank := map[int]int{}
	for r, i := range picked {
		if _, ok := rank[i]; !ok {
			rank[i] = r
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO run_candidates(run_id, position, profile_id, title, link, norm_link, category, feed_name,
		                           published_at, picked_rank)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, a := range cands {
		var pr any
		if r, ok := rank[i]; ok {
			pr = r
		}
		if _, err := stmt.ExecContext(ctx, runID, i, profileID, a.Title, a.Link, a.NormLink, a.Category,
			a.FeedName, nullUnix(a.PublishedAt), pr); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RunCandidates(ctx context.Context, runID int64) ([]Candidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT position, title, link, norm_link, category, feed_name, published_at, COALESCE(picked_rank, -1)
		FROM run_candidates WHERE run_id = ? ORDER BY position`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		var pub sql.NullInt64
		if err := rows.Scan(&c.Position, &c.Title, &c.Link, &c.NormLink, &c.Category, &c.FeedName, &pub,
			&c.PickedRank); err != nil {
			return nil, err
		}
		c.PublishedAt = fromUnix(pub)
		out = append(out, c)
	}
	return out, rows.Err()
}

// EvalRuns は候補の写しが残っている、since 以降に始まった実行を古い順に返す。
func (s *Store) EvalRuns(ctx context.Context, profileID int64, since time.Time) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs
		WHERE profile_id = ? AND started_at >= ? AND EXISTS (SELECT 1 FROM run_candidates c WHERE c.run_id = runs.id)
		ORDER BY id`, profileID, unix(since))
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// ItemsBeforeRun はその日のページに、その実行より前の実行で載った記事（選定プロンプトの掲載済み一覧の再現用）。
func (s *Store) ItemsBeforeRun(ctx context.Context, profileID int64, date string, runID int64) ([]PageItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT title, norm_link FROM page_items
		WHERE profile_id = ? AND date = ? AND run_id < ? ORDER BY run_id, position`, profileID, date, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PageItem
	for rows.Next() {
		var it PageItem
		if err := rows.Scan(&it.Title, &it.NormLink); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

type ItemInput struct {
	ItemID  int64
	RunID   int64
	Date    string
	Title   string
	Stage   string
	Summary string
	Text    string
}

// ItemInputs は since 以降に載った記事のうち、要約に渡した本文が残っているものを新しい順に最大 limit 件返す。
func (s *Store) ItemInputs(ctx context.Context, profileID int64, since time.Time, limit int) ([]ItemInput, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.run_id, i.date, i.title, i.stage, i.summary, t.text
		FROM page_items i JOIN item_inputs t ON t.item_id = i.id
		WHERE i.profile_id = ? AND i.created_at >= ? AND i.summary_ok = 1
		ORDER BY i.id DESC LIMIT ?`, profileID, unix(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemInput
	for rows.Next() {
		var it ItemInput
		if err := rows.Scan(&it.ItemID, &it.RunID, &it.Date, &it.Title, &it.Stage, &it.Summary, &it.Text); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ---- ページ取得のキャッシュ ----

// CachedPage は maxAge 以内に取得したページのHTMLを返す（取得失敗も記録してあれば ok=true で空文字列）。
func (s *Store) CachedPage(ctx context.Context, url string, maxAge time.Duration, now time.Time) (html string, ok bool) {
	err := s.db.QueryRowContext(ctx, `SELECT html FROM page_cache WHERE url = ? AND fetched_at >= ?`,
		url, unix(now.Add(-maxAge))).Scan(&html)
	return html, err == nil
}

func (s *Store) PutPageCache(ctx context.Context, url, html string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO page_cache(url, fetched_at, html) VALUES(?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET fetched_at = excluded.fetched_at, html = excluded.html`, url, unix(now), html)
	return err
}

// PruneSnapshots は before より古い実行の候補の写し・要約の入力と、cacheBefore より古いページキャッシュを消す。
func (s *Store) PruneSnapshots(ctx context.Context, before, cacheBefore time.Time) error {
	for _, q := range []struct {
		sql string
		t   time.Time
	}{
		{`DELETE FROM run_candidates WHERE run_id IN (SELECT id FROM runs WHERE started_at < ?)`, before},
		{`DELETE FROM item_inputs WHERE item_id IN (SELECT id FROM page_items WHERE created_at < ?)`, before},
		{`DELETE FROM page_cache WHERE fetched_at < ?`, cacheBefore},
	} {
		if _, err := s.db.ExecContext(ctx, q.sql, unix(q.t)); err != nil {
			return err
		}
	}
	return nil
}

// ---- 開いた記録 ----

const (
	OpenFromPage   = "page"
	OpenFromReader = "reader"
	OpenFromSaved  = "saved"
)

func (s *Store) RecordOpen(ctx context.Context, profileID int64, normLink, source string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO opens(profile_id, norm_link, source, first_at, last_at, count) VALUES(?, ?, ?, ?, ?, 1)
		ON CONFLICT(profile_id, norm_link, source) DO UPDATE SET last_at = excluded.last_at, count = count + 1`,
		profileID, normLink, source, unix(now), unix(now))
	return err
}

func (s *Store) PageItem(ctx context.Context, profileID, id int64) (PageItem, error) {
	var it PageItem
	var c int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, date, run_id, position, title, link, norm_link, category, feed_name,
		       summary, summary_ok, stage, image, created_at
		FROM page_items WHERE profile_id = ? AND id = ?`, profileID, id).Scan(&it.ID, &it.Date, &it.RunID,
		&it.Position, &it.Title, &it.Link, &it.NormLink, &it.Category, &it.FeedName, &it.Summary, &it.SummaryOK,
		&it.Stage, &it.Image, &c)
	if err == sql.ErrNoRows {
		return it, ErrNotFound
	}
	it.CreatedAt = time.Unix(c, 0)
	return it, err
}

// ---- お気に入り・後で読む ----

const (
	SavedFavorite = "favorite"
	SavedLater    = "later"
)

type Saved struct {
	ID        int64
	ProfileID int64
	Kind      string
	NormLink  string
	Title     string
	Link      string
	Category  string
	FeedName  string
	Summary   string
	Image     string
	CreatedAt time.Time
}

// SavedMark は記事ごとの保存状態（画面のボタン表示用）。
type SavedMark struct{ Favorite, Later bool }

// ToggleSaved は保存済みなら外し、なければ保存する。保存した状態になったら true。
func (s *Store) ToggleSaved(ctx context.Context, v Saved, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM saved WHERE profile_id = ? AND kind = ? AND norm_link = ?`,
		v.ProfileID, v.Kind, v.NormLink)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, nil
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO saved(profile_id, kind, norm_link, title, link, category, feed_name, summary, image, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, v.ProfileID, v.Kind, v.NormLink, v.Title, v.Link, v.Category,
		v.FeedName, v.Summary, v.Image, unix(now))
	return err == nil, err
}

const savedCols = `id, profile_id, kind, norm_link, title, link, category, feed_name, summary, image, created_at`

func scanSaved(rows *sql.Rows) ([]Saved, error) {
	defer rows.Close()
	var out []Saved
	for rows.Next() {
		var v Saved
		var c int64
		if err := rows.Scan(&v.ID, &v.ProfileID, &v.Kind, &v.NormLink, &v.Title, &v.Link, &v.Category,
			&v.FeedName, &v.Summary, &v.Image, &c); err != nil {
			return nil, err
		}
		v.CreatedAt = time.Unix(c, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) SavedList(ctx context.Context, profileID int64, kind string) ([]Saved, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+savedCols+` FROM saved
		WHERE profile_id = ? AND kind = ? ORDER BY created_at DESC, id DESC`, profileID, kind)
	if err != nil {
		return nil, err
	}
	return scanSaved(rows)
}

func (s *Store) SavedItem(ctx context.Context, profileID, id int64) (Saved, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+savedCols+` FROM saved WHERE profile_id = ? AND id = ?`,
		profileID, id)
	if err != nil {
		return Saved{}, err
	}
	list, err := scanSaved(rows)
	if err != nil {
		return Saved{}, err
	}
	if len(list) == 0 {
		return Saved{}, ErrNotFound
	}
	return list[0], nil
}

func (s *Store) SavedMarks(ctx context.Context, profileID int64) (map[string]SavedMark, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT norm_link, kind FROM saved WHERE profile_id = ?`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]SavedMark{}
	for rows.Next() {
		var link, kind string
		if err := rows.Scan(&link, &kind); err != nil {
			return nil, err
		}
		mk := m[link]
		switch kind {
		case SavedFavorite:
			mk.Favorite = true
		case SavedLater:
			mk.Later = true
		}
		m[link] = mk
	}
	return m, rows.Err()
}

func (s *Store) SavedCounts(ctx context.Context, profileID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM saved WHERE profile_id = ? GROUP BY kind`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		m[k] = n
	}
	return m, rows.Err()
}

// ---- 計測 ----

// Signal は記事への反応。開いた・保存したものを「役に立った」とみなす。
type Signal struct{ Opened, Favorite, Later bool }

func (g Signal) Positive() bool { return g.Opened || g.Favorite || g.Later }

// Signals はプロファイルで開いた・保存した記事を正規化URLごとに返す。
func (s *Store) Signals(ctx context.Context, profileID int64) (map[string]Signal, error) {
	m := map[string]Signal{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT norm_link, 'open' FROM opens WHERE profile_id = ?
		UNION ALL SELECT norm_link, kind FROM saved WHERE profile_id = ?`, profileID, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var link, kind string
		if err := rows.Scan(&link, &kind); err != nil {
			return nil, err
		}
		g := m[link]
		switch kind {
		case "open":
			g.Opened = true
		case SavedFavorite:
			g.Favorite = true
		case SavedLater:
			g.Later = true
		}
		m[link] = g
	}
	return m, rows.Err()
}

// RunReaction は実行で載った記事への反応の件数。Useful は開いたか保存した記事。
type RunReaction struct {
	Items  int `json:"items"`
	Opened int `json:"opened"`
	Saved  int `json:"saved"`
	Useful int `json:"useful"`
}

// RunReactions は runIDs の実行ごとに、載った記事・開いた記事・保存した記事の件数を返す。
func (s *Store) RunReactions(ctx context.Context, profileID int64, runIDs []int64) (map[int64]RunReaction, error) {
	out := map[int64]RunReaction{}
	if len(runIDs) == 0 {
		return out, nil
	}
	args := []any{profileID}
	for _, id := range runIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.run_id, COUNT(*),
		       SUM(EXISTS (SELECT 1 FROM opens o WHERE o.profile_id = i.profile_id AND o.norm_link = i.norm_link)),
		       SUM(EXISTS (SELECT 1 FROM saved v WHERE v.profile_id = i.profile_id AND v.norm_link = i.norm_link)),
		       SUM(EXISTS (SELECT 1 FROM opens o WHERE o.profile_id = i.profile_id AND o.norm_link = i.norm_link)
		           OR EXISTS (SELECT 1 FROM saved v WHERE v.profile_id = i.profile_id AND v.norm_link = i.norm_link))
		FROM page_items i WHERE i.profile_id = ? AND i.run_id IN (?`+strings.Repeat(",?", len(runIDs)-1)+`)
		GROUP BY i.run_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r RunReaction
		if err := rows.Scan(&id, &r.Items, &r.Opened, &r.Saved, &r.Useful); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// Missed は since 以降に、選定で選ばれなかった候補のうち保存された件数と、RSSリーダーで開かれた件数を返す
// （日付ページに一度も載っていないもの。取りこぼしの目安）。
func (s *Store) Missed(ctx context.Context, profileID int64, since time.Time) (saved, opened int, err error) {
	const notPicked = `EXISTS (SELECT 1 FROM run_candidates c WHERE c.profile_id = x.profile_id AND c.norm_link = x.norm_link)
		AND NOT EXISTS (SELECT 1 FROM page_items i WHERE i.profile_id = x.profile_id AND i.norm_link = x.norm_link)`
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT norm_link) FROM saved x
		WHERE x.profile_id = ? AND x.created_at >= ? AND `+notPicked, profileID, unix(since)).Scan(&saved)
	if err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT norm_link) FROM opens x
		WHERE x.profile_id = ? AND x.source = 'reader' AND x.first_at >= ? AND `+notPicked,
		profileID, unix(since)).Scan(&opened)
	return
}

// ---- 再生ツールの応答キャッシュ ----

func (s *Store) EvalCache(ctx context.Context, key string) (text, usage string, ok bool) {
	err := s.db.QueryRowContext(ctx, `SELECT text, usage FROM eval_cache WHERE key = ?`, key).Scan(&text, &usage)
	return text, usage, err == nil
}

func (s *Store) PutEvalCache(ctx context.Context, key, text, usage string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO eval_cache(key, text, usage, created_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET text = excluded.text, usage = excluded.usage, created_at = excluded.created_at`,
		key, text, usage, unix(now))
	return err
}

func (s *Store) Run(ctx context.Context, id int64) (Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id)
	if err != nil {
		return Run{}, err
	}
	list, err := scanRuns(rows)
	if err != nil {
		return Run{}, err
	}
	if len(list) == 0 {
		return Run{}, ErrNotFound
	}
	return list[0], nil
}

// ItemStages は since 以降に載った記事の要約元（feed / page / fallback / failed）ごとの件数。
func (s *Store) ItemStages(ctx context.Context, profileID int64, since time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stage, COUNT(*) FROM page_items
		WHERE profile_id = ? AND created_at >= ? GROUP BY stage`, profileID, unix(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		m[k] = n
	}
	return m, rows.Err()
}

// InputLengths は since 以降に要約へ渡した本文の文字数（記録が残っているもの）。
func (s *Store) InputLengths(ctx context.Context, profileID int64, since time.Time) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT length(t.text) FROM item_inputs t JOIN page_items i ON i.id = t.item_id
		WHERE i.profile_id = ? AND i.created_at >= ? ORDER BY 1`, profileID, unix(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// FeedPick はフィードごとの、選定に渡した候補の数と選ばれた数。
type FeedPick struct{ Candidates, Picked int }

// FeedPicks は since 以降に始まった実行の候補を、フィード名ごとに数える。
func (s *Store) FeedPicks(ctx context.Context, profileID int64, since time.Time) (map[string]FeedPick, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.feed_name, COUNT(*), COUNT(c.picked_rank) FROM run_candidates c JOIN runs r ON r.id = c.run_id
		WHERE c.profile_id = ? AND r.started_at >= ? GROUP BY c.feed_name`, profileID, unix(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]FeedPick{}
	for rows.Next() {
		var name string
		var p FeedPick
		if err := rows.Scan(&name, &p.Candidates, &p.Picked); err != nil {
			return nil, err
		}
		m[name] = p
	}
	return m, rows.Err()
}
