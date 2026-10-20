// Package store はSQLiteによる永続化（フィード・記事・日付ページ・実行履歴・設定）を担う。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS feeds (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	title         TEXT NOT NULL,
	url           TEXT NOT NULL UNIQUE,
	category      TEXT NOT NULL DEFAULT '',
	enabled       INTEGER NOT NULL DEFAULT 1,
	etag          TEXT NOT NULL DEFAULT '',
	last_modified TEXT NOT NULL DEFAULT '',
	failures      INTEGER NOT NULL DEFAULT 0,
	last_attempt  INTEGER,
	last_success  INTEGER,
	last_error    TEXT NOT NULL DEFAULT '',
	position      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS articles (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	feed_id      INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
	title        TEXT NOT NULL,
	link         TEXT NOT NULL,
	norm_link    TEXT NOT NULL,
	published_at INTEGER,
	fetched_at   INTEGER NOT NULL,
	body         TEXT NOT NULL DEFAULT '',
	image        TEXT NOT NULL DEFAULT '',
	read         INTEGER NOT NULL DEFAULT 0,
	UNIQUE(feed_id, norm_link)
);
CREATE INDEX IF NOT EXISTS idx_articles_fetched ON articles(fetched_at);
CREATE INDEX IF NOT EXISTS idx_articles_norm ON articles(norm_link);

-- 選定に一度でも渡した記事。差分（前回実行以降に増えた記事）の判定に使う
CREATE TABLE IF NOT EXISTS considered (
	article_id INTEGER PRIMARY KEY REFERENCES articles(id) ON DELETE CASCADE,
	date       TEXT NOT NULL,
	run_id     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_considered_date ON considered(date);

CREATE TABLE IF NOT EXISTS pages (
	date       TEXT PRIMARY KEY,
	overview   TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

-- ページに載った記事。記事本体が保持期間切れで消えても残るよう、表示用の値は複製して持つ
CREATE TABLE IF NOT EXISTS page_items (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	date       TEXT NOT NULL REFERENCES pages(date) ON DELETE CASCADE,
	run_id     INTEGER NOT NULL,
	position   INTEGER NOT NULL,
	title      TEXT NOT NULL,
	link       TEXT NOT NULL,
	norm_link  TEXT NOT NULL,
	category   TEXT NOT NULL DEFAULT '',
	feed_name  TEXT NOT NULL DEFAULT '',
	summary    TEXT NOT NULL,
	summary_ok INTEGER NOT NULL,
	stage      TEXT NOT NULL,
	image      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_items_date ON page_items(date);
CREATE INDEX IF NOT EXISTS idx_items_norm ON page_items(norm_link);

CREATE TABLE IF NOT EXISTS runs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	date        TEXT NOT NULL,
	kind        TEXT NOT NULL,          -- append | rebuild
	trigger     TEXT NOT NULL,          -- schedule | manual
	slot        TEXT NOT NULL DEFAULT '', -- 定時実行の枠（例 "07:00"）
	started_at  INTEGER NOT NULL,
	finished_at INTEGER,
	status      TEXT NOT NULL,          -- running | ok | failed | skipped
	message     TEXT NOT NULL DEFAULT '',
	candidates  INTEGER NOT NULL DEFAULT 0,
	picked      INTEGER NOT NULL DEFAULT 0,
	usage       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_date ON runs(date);
`

type Store struct {
	db *sql.DB
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "mekiki.db")
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLiteは書き込みが直列なので接続を絞ってロック競合を避ける
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("スキーマ作成に失敗: %w", err)
	}
	_ = os.Chmod(path, 0o600) // APIキー等を含むため
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func unix(t time.Time) int64 { return t.Unix() }

func fromUnix(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0)
}

func nullUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

// ---- settings ----

func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

func (s *Store) SetSettings(ctx context.Context, kv map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- feeds ----

type Feed struct {
	ID           int64
	Title        string
	URL          string
	Category     string
	Enabled      bool
	ETag         string
	LastModified string
	Failures     int
	LastAttempt  time.Time
	LastSuccess  time.Time
	LastError    string
	ArticleCount int
	UnreadCount  int
}

func (s *Store) Feeds(ctx context.Context) ([]Feed, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, f.title, f.url, f.category, f.enabled, f.etag, f.last_modified,
		       f.failures, f.last_attempt, f.last_success, f.last_error,
		       (SELECT COUNT(*) FROM articles a WHERE a.feed_id = f.id),
		       (SELECT COUNT(*) FROM articles a WHERE a.feed_id = f.id AND a.read = 0)
		FROM feeds f ORDER BY f.category, f.position, f.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feed
	for rows.Next() {
		var f Feed
		var la, ls sql.NullInt64
		if err := rows.Scan(&f.ID, &f.Title, &f.URL, &f.Category, &f.Enabled, &f.ETag,
			&f.LastModified, &f.Failures, &la, &ls, &f.LastError,
			&f.ArticleCount, &f.UnreadCount); err != nil {
			return nil, err
		}
		f.LastAttempt, f.LastSuccess = fromUnix(la), fromUnix(ls)
		out = append(out, f)
	}
	return out, rows.Err()
}

// AddFeed はフィードを登録する。同じURLが既にあれば何もしない（false を返す）。
func (s *Store) AddFeed(ctx context.Context, title, url, category string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO feeds(title, url, category, position)
		VALUES(?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM feeds))
		ON CONFLICT(url) DO NOTHING`, title, url, category)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) UpdateFeed(ctx context.Context, id int64, title, category string, enabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE feeds SET title = ?, category = ?, enabled = ? WHERE id = ?`,
		title, category, enabled, id)
	return err
}

func (s *Store) DeleteFeed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM feeds WHERE id = ?`, id)
	return err
}

func (s *Store) ResetFeedFailures(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE feeds SET failures = 0`)
	return err
}

func (s *Store) MarkFeedSuccess(ctx context.Context, id int64, etag, lastModified string, now time.Time, keepValidators bool) error {
	if keepValidators {
		_, err := s.db.ExecContext(ctx, `
			UPDATE feeds SET failures = 0, last_attempt = ?, last_success = ?, last_error = ''
			WHERE id = ?`, unix(now), unix(now), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE feeds SET etag = ?, last_modified = ?, failures = 0,
		       last_attempt = ?, last_success = ?, last_error = ''
		WHERE id = ?`, etag, lastModified, unix(now), unix(now), id)
	return err
}

func (s *Store) MarkFeedFailure(ctx context.Context, id int64, msg string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feeds SET failures = failures + 1, last_attempt = ?, last_error = ?
		WHERE id = ?`, unix(now), msg, id)
	return err
}

// ---- articles ----

type Article struct {
	ID          int64
	FeedID      int64
	FeedName    string
	Category    string
	Title       string
	Link        string
	NormLink    string
	PublishedAt time.Time // ゼロ値 = 不明
	FetchedAt   time.Time
	Body        string
	Image       string
	Read        bool
}

// InsertArticles は新着記事を追加し、新規に入った件数を返す。既存（同一フィード・同一正規化URL）は無視。
func (s *Store) InsertArticles(ctx context.Context, articles []Article) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO articles(feed_id, title, link, norm_link, published_at, fetched_at, body, image)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(feed_id, norm_link) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	added := 0
	for _, a := range articles {
		res, err := stmt.ExecContext(ctx, a.FeedID, a.Title, a.Link, a.NormLink,
			nullUnix(a.PublishedAt), unix(a.FetchedAt), a.Body, a.Image)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, tx.Commit()
}

const articleCols = `a.id, a.feed_id, f.title, f.category, a.title, a.link, a.norm_link,
	a.published_at, a.fetched_at, a.body, a.image, a.read`

func scanArticles(rows *sql.Rows) ([]Article, error) {
	defer rows.Close()
	var out []Article
	for rows.Next() {
		var a Article
		var pub, fetched sql.NullInt64
		if err := rows.Scan(&a.ID, &a.FeedID, &a.FeedName, &a.Category, &a.Title, &a.Link,
			&a.NormLink, &pub, &fetched, &a.Body, &a.Image, &a.Read); err != nil {
			return nil, err
		}
		a.PublishedAt, a.FetchedAt = fromUnix(pub), fromUnix(fetched)
		out = append(out, a)
	}
	return out, rows.Err()
}

type ArticleFilter struct {
	FeedID     int64
	Category   string
	UnreadOnly bool
	Limit      int
	Offset     int
}

// Articles はRSSリーダー画面用の記事一覧（新しい順）。
func (s *Store) Articles(ctx context.Context, f ArticleFilter) ([]Article, error) {
	var where []string
	var args []any
	if f.FeedID > 0 {
		where = append(where, "a.feed_id = ?")
		args = append(args, f.FeedID)
	}
	if f.Category != "" {
		where = append(where, "f.category = ?")
		args = append(args, f.Category)
	}
	if f.UnreadOnly {
		where = append(where, "a.read = 0")
	}
	q := `SELECT ` + articleCols + ` FROM articles a JOIN feeds f ON f.id = a.feed_id`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += ` ORDER BY COALESCE(a.published_at, a.fetched_at) DESC, a.id DESC LIMIT ? OFFSET ?`
	args = append(args, f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanArticles(rows)
}

func (s *Store) Article(ctx context.Context, id int64) (Article, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+articleCols+` FROM articles a JOIN feeds f ON f.id = a.feed_id WHERE a.id = ?`, id)
	if err != nil {
		return Article{}, err
	}
	list, err := scanArticles(rows)
	if err != nil {
		return Article{}, err
	}
	if len(list) == 0 {
		return Article{}, sql.ErrNoRows
	}
	return list[0], nil
}

func (s *Store) MarkRead(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE articles SET read = 1 WHERE id = ?`, id)
	return err
}

func (s *Store) MarkAllRead(ctx context.Context, feedID int64) error {
	if feedID > 0 {
		_, err := s.db.ExecContext(ctx, `UPDATE articles SET read = 1 WHERE feed_id = ?`, feedID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE articles SET read = 1`)
	return err
}

// Candidates は要約実行の候補（＝前回までの実行で選定に渡していない記事）を返す。
//   - 公開日時が since 以降（公開日時が不明な記事は取得日時で判定）
//   - 有効なフィードのもの
//   - まだ選定に渡していない（considered にない）
//   - 過去のページに載っていない（正規化URLで判定）
//
// ignoreDate を指定すると、その日の選定済み印・掲載分は無いものとして扱う（ページ再作成用）。
// 並びはカテゴリ・フィードの登録順→新しい順。
func (s *Store) Candidates(ctx context.Context, since time.Time, ignoreDate string) ([]Article, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+articleCols+`
		FROM articles a JOIN feeds f ON f.id = a.feed_id
		WHERE f.enabled = 1
		  AND COALESCE(a.published_at, a.fetched_at) >= ?
		  AND a.fetched_at >= ?
		  AND NOT EXISTS (SELECT 1 FROM considered c WHERE c.article_id = a.id AND c.date != ?)
		  AND NOT EXISTS (SELECT 1 FROM page_items p WHERE p.norm_link = a.norm_link AND p.date != ?)
		ORDER BY f.category, f.position, f.id, COALESCE(a.published_at, a.fetched_at) DESC`,
		unix(since), unix(since), ignoreDate, ignoreDate)
	if err != nil {
		return nil, err
	}
	return scanArticles(rows)
}

// RecentPageTitles は直近のページに載った記事タイトル（タイトル一致dedup用）。
func (s *Store) RecentPageTitles(ctx context.Context, since time.Time, ignoreDate string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT title FROM page_items WHERE created_at >= ? AND date != ?`, unix(since), ignoreDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		m[strings.TrimSpace(t)] = true
	}
	return m, rows.Err()
}

func (s *Store) MarkConsidered(ctx context.Context, ids []int64, date string, runID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO considered(article_id, date, run_id) VALUES(?, ?, ?)
		 ON CONFLICT(article_id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id, date, runID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneArticles は保持期間を過ぎた記事を削除する（ページに載った記事の表示内容は page_items 側に残る）。
func (s *Store) PruneArticles(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM articles WHERE fetched_at < ?`, unix(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- pages ----

type Page struct {
	Date      string
	Overview  string
	CreatedAt time.Time
	UpdatedAt time.Time
	ItemCount int
}

type PageItem struct {
	ID        int64
	Date      string
	RunID     int64
	Position  int
	Title     string
	Link      string
	NormLink  string
	Category  string
	FeedName  string
	Summary   string
	SummaryOK bool
	Stage     string
	Image     string
	CreatedAt time.Time
}

func (s *Store) Page(ctx context.Context, date string) (Page, bool, error) {
	var p Page
	var c, u int64
	err := s.db.QueryRowContext(ctx, `
		SELECT date, overview, created_at, updated_at,
		       (SELECT COUNT(*) FROM page_items i WHERE i.date = pages.date)
		FROM pages WHERE date = ?`, date).Scan(&p.Date, &p.Overview, &c, &u, &p.ItemCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Page{}, false, nil
	}
	if err != nil {
		return Page{}, false, err
	}
	p.CreatedAt, p.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return p, true, nil
}

func (s *Store) Pages(ctx context.Context) ([]Page, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT date, overview, created_at, updated_at,
		       (SELECT COUNT(*) FROM page_items i WHERE i.date = pages.date)
		FROM pages ORDER BY date DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		var p Page
		var c, u int64
		if err := rows.Scan(&p.Date, &p.Overview, &c, &u, &p.ItemCount); err != nil {
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// AdjacentDates はdateの前後にあるページの日付を返す（なければ空文字列）。
func (s *Store) AdjacentDates(ctx context.Context, date string) (prev, next string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(date), '') FROM pages WHERE date < ?`, date).Scan(&prev)
	if err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MIN(date), '') FROM pages WHERE date > ?`, date).Scan(&next)
	return
}

func (s *Store) PageItems(ctx context.Context, date string) ([]PageItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, date, run_id, position, title, link, norm_link, category, feed_name,
		       summary, summary_ok, stage, image, created_at
		FROM page_items WHERE date = ? ORDER BY run_id DESC, position`, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PageItem
	for rows.Next() {
		var it PageItem
		var c int64
		if err := rows.Scan(&it.ID, &it.Date, &it.RunID, &it.Position, &it.Title, &it.Link,
			&it.NormLink, &it.Category, &it.FeedName, &it.Summary, &it.SummaryOK, &it.Stage,
			&it.Image, &c); err != nil {
			return nil, err
		}
		it.CreatedAt = time.Unix(c, 0)
		out = append(out, it)
	}
	return out, rows.Err()
}

// AppendItems はページ（なければ作成）に記事を追記する。
func (s *Store) AppendItems(ctx context.Context, date string, items []PageItem, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pages(date, created_at, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(date) DO UPDATE SET updated_at = excluded.updated_at`,
		date, unix(now), unix(now)); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO page_items(date, run_id, position, title, link, norm_link, category,
		                       feed_name, summary, summary_ok, stage, image, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, it := range items {
		if _, err := stmt.ExecContext(ctx, date, it.RunID, it.Position, it.Title, it.Link,
			it.NormLink, it.Category, it.FeedName, it.Summary, it.SummaryOK, it.Stage,
			it.Image, unix(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetOverview(ctx context.Context, date, overview string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pages SET overview = ?, updated_at = ? WHERE date = ?`, overview, unix(now), date)
	return err
}

// ClearPage はその日のページを消し、その日に選定へ渡した記事の「選定済み」印も外す
// （再作成時に同じ記事を再び候補にできるようにするため）。
func (s *Store) ClearPage(ctx context.Context, date string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM page_items WHERE date = ?`,
		`DELETE FROM pages WHERE date = ?`,
		`DELETE FROM considered WHERE date = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, date); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- runs ----

type Run struct {
	ID         int64
	Date       string
	Kind       string
	Trigger    string
	Slot       string
	StartedAt  time.Time
	FinishedAt time.Time
	Status     string
	Message    string
	Candidates int
	Picked     int
	Usage      string
}

func (s *Store) StartRun(ctx context.Context, r Run) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO runs(date, kind, trigger, slot, started_at, status)
		VALUES(?, ?, ?, ?, ?, 'running')`, r.Date, r.Kind, r.Trigger, r.Slot, unix(r.StartedAt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishRun(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET finished_at = ?, status = ?, message = ?, candidates = ?, picked = ?, usage = ?
		WHERE id = ?`, unix(r.FinishedAt), r.Status, r.Message, r.Candidates, r.Picked, r.Usage, r.ID)
	return err
}

// FailStaleRuns は前回プロセスが途中で落ちて running のまま残った実行を失敗扱いにする。
func (s *Store) FailStaleRuns(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = 'failed', finished_at = ?, message = '実行中にサーバが停止しました'
		WHERE status = 'running'`, unix(now))
	return err
}

func scanRuns(rows *sql.Rows) ([]Run, error) {
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var st, fin sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Date, &r.Kind, &r.Trigger, &r.Slot, &st, &fin, &r.Status,
			&r.Message, &r.Candidates, &r.Picked, &r.Usage); err != nil {
			return nil, err
		}
		r.StartedAt, r.FinishedAt = fromUnix(st), fromUnix(fin)
		out = append(out, r)
	}
	return out, rows.Err()
}

const runCols = `id, date, kind, trigger, slot, started_at, finished_at, status, message,
	candidates, picked, usage`

func (s *Store) Runs(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+` FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

func (s *Store) RunsOn(ctx context.Context, date string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+` FROM runs WHERE date = ? ORDER BY id DESC`, date)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// SlotDone はその日・その定時枠の実行が既に行われたか。
func (s *Store) SlotDone(ctx context.Context, date, slot string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE date = ? AND slot = ? AND trigger = 'schedule'`,
		date, slot).Scan(&n)
	return n > 0, err
}
