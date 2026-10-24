// Package store はSQLiteによる永続化（フィード・記事・日付ページ・実行履歴・設定）を担う。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kienn-HCl/mekiki/internal/config"
)

const schemaVersion = 2

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- プロファイル（技術記事用・ニュース用など）。フィード・ページ・実行履歴はプロファイルごと
CREATE TABLE IF NOT EXISTS profiles (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	slug               TEXT NOT NULL UNIQUE,
	name               TEXT NOT NULL,
	position           INTEGER NOT NULL DEFAULT 0,
	select_criteria    TEXT NOT NULL,
	user_memory        TEXT NOT NULL DEFAULT '',
	pick_count         INTEGER NOT NULL,
	hours_back         INTEGER NOT NULL,
	overview           INTEGER NOT NULL,
	digest_times       TEXT NOT NULL DEFAULT '',
	discord_webhook    TEXT NOT NULL DEFAULT '',
	discord_embeds     INTEGER NOT NULL DEFAULT 5,
	discord_title      TEXT NOT NULL DEFAULT '',
	discord_color      INTEGER NOT NULL DEFAULT 0,
	discord_base_url   TEXT NOT NULL DEFAULT '',
	discord_after_fail INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS feeds (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	profile_id    INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	title         TEXT NOT NULL,
	url           TEXT NOT NULL,
	category      TEXT NOT NULL DEFAULT '',
	enabled       INTEGER NOT NULL DEFAULT 1,
	etag          TEXT NOT NULL DEFAULT '',
	last_modified TEXT NOT NULL DEFAULT '',
	failures      INTEGER NOT NULL DEFAULT 0,
	last_attempt  INTEGER,
	last_success  INTEGER,
	last_error    TEXT NOT NULL DEFAULT '',
	position      INTEGER NOT NULL DEFAULT 0,
	UNIQUE(profile_id, url)
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
	profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	date       TEXT NOT NULL,
	overview   TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(profile_id, date)
);

-- ページに載った記事。記事本体が保持期間切れで消えても残るよう、表示用の値は複製して持つ
CREATE TABLE IF NOT EXISTS page_items (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	profile_id INTEGER NOT NULL,
	date       TEXT NOT NULL,
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
	created_at INTEGER NOT NULL,
	FOREIGN KEY(profile_id, date) REFERENCES pages(profile_id, date) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_items_date ON page_items(profile_id, date);
CREATE INDEX IF NOT EXISTS idx_items_norm ON page_items(profile_id, norm_link);

CREATE TABLE IF NOT EXISTS runs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	profile_id  INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
	date        TEXT NOT NULL,
	kind        TEXT NOT NULL,            -- append | rebuild
	trigger     TEXT NOT NULL,            -- schedule | manual
	slot        TEXT NOT NULL DEFAULT '', -- 定時実行の枠（例 "07:00"）
	started_at  INTEGER NOT NULL,
	finished_at INTEGER,
	status      TEXT NOT NULL,            -- running | ok | failed | skipped
	message     TEXT NOT NULL DEFAULT '',
	candidates  INTEGER NOT NULL DEFAULT 0,
	picked      INTEGER NOT NULL DEFAULT 0,
	usage       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_date ON runs(profile_id, date);
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
	// SQLiteは書き込みが直列なので接続を1本に絞ってロック競合を避ける（PRAGMAも接続単位で効く）
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600) // APIキー等を含むため
	return s, nil
}

func (s *Store) initSchema() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != schemaVersion {
		return fmt.Errorf("対応していないデータベースの版です（%d）", version)
	}
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("スキーマ作成に失敗: %w", err)
	}
	_, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
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

// ---- settings（サーバ全体） ----

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

// ---- profiles ----

var ErrNotFound = errors.New("見つかりません")

const profileCols = `id, slug, name, position, select_criteria, user_memory, pick_count, hours_back,
	overview, digest_times, discord_webhook, discord_embeds, discord_title, discord_color,
	discord_base_url, discord_after_fail`

type scanner interface{ Scan(...any) error }

func scanProfile(r scanner) (config.Profile, error) {
	var p config.Profile
	var times string
	err := r.Scan(&p.ID, &p.Slug, &p.Name, &p.Position, &p.SelectCriteria, &p.UserMemory,
		&p.PickCount, &p.HoursBack, &p.Overview, &times, &p.DiscordWebhook, &p.DiscordEmbeds,
		&p.DiscordTitle, &p.DiscordColor, &p.DiscordBaseURL, &p.DiscordAfterFail)
	if err != nil {
		return p, err
	}
	p.DigestTimes, _ = config.ParseTimes(times)
	return p, nil
}

func (s *Store) Profiles(ctx context.Context) ([]config.Profile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+profileCols+` FROM profiles ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []config.Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Profile(ctx context.Context, id int64) (config.Profile, error) {
	p, err := scanProfile(s.db.QueryRowContext(ctx, `SELECT `+profileCols+` FROM profiles WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func (s *Store) ProfileBySlug(ctx context.Context, slug string) (config.Profile, error) {
	p, err := scanProfile(s.db.QueryRowContext(ctx, `SELECT `+profileCols+` FROM profiles WHERE slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertProfile(db execer, p config.Profile) (int64, error) {
	res, err := db.Exec(`
		INSERT INTO profiles(slug, name, position, select_criteria, user_memory, pick_count, hours_back,
		                     overview, digest_times, discord_webhook, discord_embeds, discord_title,
		                     discord_color, discord_base_url, discord_after_fail)
		VALUES(?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM profiles), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Slug, p.Name, p.SelectCriteria, p.UserMemory, p.PickCount, p.HoursBack, p.Overview,
		strings.Join(p.DigestTimes, ","), p.DiscordWebhook, p.DiscordEmbeds, p.DiscordTitle,
		p.DiscordColor, p.DiscordBaseURL, p.DiscordAfterFail)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("プロファイルID %q は使われています", p.Slug)
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) CreateProfile(ctx context.Context, p config.Profile) (int64, error) {
	return insertProfile(s.db, p)
}

func (s *Store) UpdateProfile(ctx context.Context, p config.Profile) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE profiles SET slug = ?, name = ?, select_criteria = ?, user_memory = ?, pick_count = ?,
		       hours_back = ?, overview = ?, digest_times = ?, discord_webhook = ?, discord_embeds = ?,
		       discord_title = ?, discord_color = ?, discord_base_url = ?, discord_after_fail = ?
		WHERE id = ?`,
		p.Slug, p.Name, p.SelectCriteria, p.UserMemory, p.PickCount, p.HoursBack, p.Overview,
		strings.Join(p.DigestTimes, ","), p.DiscordWebhook, p.DiscordEmbeds, p.DiscordTitle,
		p.DiscordColor, p.DiscordBaseURL, p.DiscordAfterFail, p.ID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("プロファイルID %q は使われています", p.Slug)
	}
	return err
}

// MoveProfile はプロファイルの並び順を変える。to は移動先の位置（0始まり。範囲外は端に丸める）。
// 先頭のプロファイルが、トップページを開いたときに表示される既定のプロファイルになる。
func (s *Store) MoveProfile(ctx context.Context, id int64, to int) error {
	list, err := s.Profiles(ctx)
	if err != nil {
		return err
	}
	from := slices.IndexFunc(list, func(p config.Profile) bool { return p.ID == id })
	if from < 0 {
		return ErrNotFound
	}
	to = max(0, min(to, len(list)-1))
	p := list[from]
	list = slices.Delete(list, from, from+1)
	list = slices.Insert(list, to, p)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, p := range list {
		if _, err := tx.ExecContext(ctx, `UPDATE profiles SET position = ? WHERE id = ?`, i+1, p.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetProfileOrder はプロファイルの並びを ids の順にする（含まれないものは後ろに回る）。
func (s *Store) SetProfileOrder(ctx context.Context, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE profiles SET position = position + 100000`); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE profiles SET position = ? WHERE id = ?`, i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FeedDef は設定ファイルに書くフィードの定義（取得状態はDB側にだけ持つ）。
type FeedDef struct {
	Title    string
	URL      string
	Category string
	Enabled  bool
}

// SyncFeeds はプロファイルのフィード一覧を defs に合わせる。URLで突き合わせ、
// あるものは名前・カテゴリ・有効/無効・並びを更新、ないものは追加、defs にないものは削除する
// （削除したフィードの記事も消える。取得状態・記事はURLが同じ限り引き継がれる）。
func (s *Store) SyncFeeds(ctx context.Context, profileID int64, defs []FeedDef) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, url FROM feeds WHERE profile_id = ?`, profileID)
	if err != nil {
		return err
	}
	existing := map[string]int64{}
	for rows.Next() {
		var id int64
		var u string
		if err := rows.Scan(&id, &u); err != nil {
			rows.Close()
			return err
		}
		existing[u] = id
	}
	rows.Close()
	for i, d := range defs {
		if id, ok := existing[d.URL]; ok {
			if _, err := tx.ExecContext(ctx,
				`UPDATE feeds SET title = ?, category = ?, enabled = ?, position = ? WHERE id = ?`,
				d.Title, d.Category, d.Enabled, i+1, id); err != nil {
				return err
			}
			delete(existing, d.URL)
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO feeds(profile_id, title, url, category, enabled, position) VALUES(?, ?, ?, ?, ?, ?)`,
			profileID, d.Title, d.URL, d.Category, d.Enabled, i+1); err != nil {
			return err
		}
	}
	for _, id := range existing {
		if _, err := tx.ExecContext(ctx, `DELETE FROM feeds WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BackupTo はDB全体の一貫したコピーを path に書き出す（稼働中でも取れる）。
func (s *Store) BackupTo(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// DeleteProfile はプロファイルとそのフィード・記事・ページ・履歴をすべて消す。
func (s *Store) DeleteProfile(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM profiles WHERE id = ?`, id)
	return err
}

// EnsureProfile はプロファイルが1つもなければ既定のものを作る。
func (s *Store) EnsureProfile(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM profiles`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.CreateProfile(ctx, config.DefaultProfile("main", "メイン"))
	return err
}

// ---- feeds ----

type Feed struct {
	ID           int64
	ProfileID    int64
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

// Feeds はフィード一覧。profileID が0なら全プロファイル。
func (s *Store) Feeds(ctx context.Context, profileID int64) ([]Feed, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, f.profile_id, f.title, f.url, f.category, f.enabled, f.etag, f.last_modified,
		       f.failures, f.last_attempt, f.last_success, f.last_error,
		       (SELECT COUNT(*) FROM articles a WHERE a.feed_id = f.id),
		       (SELECT COUNT(*) FROM articles a WHERE a.feed_id = f.id AND a.read = 0)
		FROM feeds f WHERE (? = 0 OR f.profile_id = ?)
		ORDER BY f.category, f.position, f.id`, profileID, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feed
	for rows.Next() {
		var f Feed
		var la, ls sql.NullInt64
		if err := rows.Scan(&f.ID, &f.ProfileID, &f.Title, &f.URL, &f.Category, &f.Enabled, &f.ETag,
			&f.LastModified, &f.Failures, &la, &ls, &f.LastError,
			&f.ArticleCount, &f.UnreadCount); err != nil {
			return nil, err
		}
		f.LastAttempt, f.LastSuccess = fromUnix(la), fromUnix(ls)
		out = append(out, f)
	}
	return out, rows.Err()
}

// AddFeed はフィードを登録する。プロファイル内に同じURLが既にあれば何もしない（false を返す）。
func (s *Store) AddFeed(ctx context.Context, profileID int64, title, url, category string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO feeds(profile_id, title, url, category, position)
		VALUES(?, ?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM feeds))
		ON CONFLICT(profile_id, url) DO NOTHING`, profileID, title, url, category)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) UpdateFeed(ctx context.Context, profileID, id int64, title, category string, enabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE feeds SET title = ?, category = ?, enabled = ? WHERE id = ? AND profile_id = ?`,
		title, category, enabled, id, profileID)
	return err
}

func (s *Store) DeleteFeed(ctx context.Context, profileID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM feeds WHERE id = ? AND profile_id = ?`, id, profileID)
	return err
}

func (s *Store) ResetFeedFailures(ctx context.Context, profileID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE feeds SET failures = 0 WHERE (? = 0 OR profile_id = ?)`, profileID, profileID)
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
	ProfileID  int64
	FeedID     int64
	Category   string
	UnreadOnly bool
	Limit      int
	Offset     int
}

// Articles はRSSリーダー画面用の記事一覧（新しい順）。
func (s *Store) Articles(ctx context.Context, f ArticleFilter) ([]Article, error) {
	where := []string{"f.profile_id = ?"}
	args := []any{f.ProfileID}
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
	q := `SELECT ` + articleCols + ` FROM articles a JOIN feeds f ON f.id = a.feed_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY COALESCE(a.published_at, a.fetched_at) DESC, a.id DESC LIMIT ? OFFSET ?`
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
		return Article{}, ErrNotFound
	}
	return list[0], nil
}

func (s *Store) MarkRead(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE articles SET read = 1 WHERE id = ?`, id)
	return err
}

// MarkAllRead はプロファイルの記事（feedID>0ならそのフィードの記事）をすべて既読にする。
func (s *Store) MarkAllRead(ctx context.Context, profileID, feedID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE articles SET read = 1
		WHERE feed_id IN (SELECT id FROM feeds WHERE profile_id = ? AND (? = 0 OR id = ?))`,
		profileID, feedID, feedID)
	return err
}

// Candidates は要約実行の候補（＝前回までの実行で選定に渡していない記事）を返す。
//   - プロファイルの有効なフィードのもの
//   - 公開日時が since 以降（公開日時が不明な記事は取得日時で判定）
//   - まだ選定に渡していない（considered にない）
//   - そのプロファイルの過去のページに載っていない（正規化URLで判定）
//
// ignoreDate を指定すると、その日の選定済み印・掲載分は無いものとして扱う（ページ再作成用）。
// 並びはカテゴリ・フィードの登録順→新しい順。
func (s *Store) Candidates(ctx context.Context, profileID int64, since time.Time, ignoreDate string) ([]Article, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+articleCols+`
		FROM articles a JOIN feeds f ON f.id = a.feed_id
		WHERE f.profile_id = ? AND f.enabled = 1
		  AND COALESCE(a.published_at, a.fetched_at) >= ?
		  AND a.fetched_at >= ?
		  AND NOT EXISTS (SELECT 1 FROM considered c WHERE c.article_id = a.id AND c.date != ?)
		  AND NOT EXISTS (SELECT 1 FROM page_items p
		                  WHERE p.profile_id = f.profile_id AND p.norm_link = a.norm_link AND p.date != ?)
		ORDER BY f.category, f.position, f.id, COALESCE(a.published_at, a.fetched_at) DESC`,
		profileID, unix(since), unix(since), ignoreDate, ignoreDate)
	if err != nil {
		return nil, err
	}
	return scanArticles(rows)
}

// RecentPageTitles はプロファイルの直近のページに載った記事タイトル（タイトル一致dedup用）。
func (s *Store) RecentPageTitles(ctx context.Context, profileID int64, since time.Time, ignoreDate string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT title FROM page_items WHERE profile_id = ? AND created_at >= ? AND date != ?`,
		profileID, unix(since), ignoreDate)
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
	ProfileID int64
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

const pageCols = `profile_id, date, overview, created_at, updated_at,
	(SELECT COUNT(*) FROM page_items i WHERE i.profile_id = pages.profile_id AND i.date = pages.date)`

func scanPage(r scanner) (Page, error) {
	var p Page
	var c, u int64
	err := r.Scan(&p.ProfileID, &p.Date, &p.Overview, &c, &u, &p.ItemCount)
	p.CreatedAt, p.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return p, err
}

func (s *Store) Page(ctx context.Context, profileID int64, date string) (Page, bool, error) {
	p, err := scanPage(s.db.QueryRowContext(ctx,
		`SELECT `+pageCols+` FROM pages WHERE profile_id = ? AND date = ?`, profileID, date))
	if errors.Is(err, sql.ErrNoRows) {
		return Page{}, false, nil
	}
	if err != nil {
		return Page{}, false, err
	}
	return p, true, nil
}

func (s *Store) Pages(ctx context.Context, profileID int64) ([]Page, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pageCols+` FROM pages WHERE profile_id = ? ORDER BY date DESC`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AdjacentDates はdateの前後にあるページの日付を返す（なければ空文字列）。
func (s *Store) AdjacentDates(ctx context.Context, profileID int64, date string) (prev, next string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(date), '') FROM pages WHERE profile_id = ? AND date < ?`, profileID, date).Scan(&prev)
	if err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MIN(date), '') FROM pages WHERE profile_id = ? AND date > ?`, profileID, date).Scan(&next)
	return
}

func (s *Store) PageItems(ctx context.Context, profileID int64, date string) ([]PageItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, date, run_id, position, title, link, norm_link, category, feed_name,
		       summary, summary_ok, stage, image, created_at
		FROM page_items WHERE profile_id = ? AND date = ? ORDER BY run_id DESC, position`, profileID, date)
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

// AppendItems はページ（なければ作成）に記事を追記する。replace なら先にその日のページを消して置き換える
// （再作成用。削除と追加を1トランザクションで行い、途中で失敗しても元のページが残るようにする）。
func (s *Store) AppendItems(ctx context.Context, profileID int64, date string, items []PageItem, now time.Time, replace bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if replace {
		if err := clearPage(ctx, tx, profileID, date); err != nil {
			return err
		}
	}
	if len(items) == 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pages(profile_id, date, created_at, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(profile_id, date) DO UPDATE SET updated_at = excluded.updated_at`,
		profileID, date, unix(now), unix(now)); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO page_items(profile_id, date, run_id, position, title, link, norm_link, category,
		                       feed_name, summary, summary_ok, stage, image, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, it := range items {
		if _, err := stmt.ExecContext(ctx, profileID, date, it.RunID, it.Position, it.Title, it.Link,
			it.NormLink, it.Category, it.FeedName, it.Summary, it.SummaryOK, it.Stage,
			it.Image, unix(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetOverview(ctx context.Context, profileID int64, date, overview string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pages SET overview = ?, updated_at = ? WHERE profile_id = ? AND date = ?`,
		overview, unix(now), profileID, date)
	return err
}

// clearPage はその日のページを消し、その日に選定へ渡した記事の「選定済み」印も外す
// （再作成時に同じ記事を再び候補にできるようにするため）。
func clearPage(ctx context.Context, tx *sql.Tx, profileID int64, date string) error {
	for _, q := range []string{
		`DELETE FROM page_items WHERE profile_id = ? AND date = ?`,
		`DELETE FROM pages WHERE profile_id = ? AND date = ?`,
		`DELETE FROM considered WHERE article_id IN (
			SELECT a.id FROM articles a JOIN feeds f ON f.id = a.feed_id WHERE f.profile_id = ?
		) AND date = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, profileID, date); err != nil {
			return err
		}
	}
	return nil
}

// ---- runs ----

type Run struct {
	ID         int64
	ProfileID  int64
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
		INSERT INTO runs(profile_id, date, kind, trigger, slot, started_at, status)
		VALUES(?, ?, ?, ?, ?, ?, 'running')`,
		r.ProfileID, r.Date, r.Kind, r.Trigger, r.Slot, unix(r.StartedAt))
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
		if err := rows.Scan(&r.ID, &r.ProfileID, &r.Date, &r.Kind, &r.Trigger, &r.Slot, &st, &fin,
			&r.Status, &r.Message, &r.Candidates, &r.Picked, &r.Usage); err != nil {
			return nil, err
		}
		r.StartedAt, r.FinishedAt = fromUnix(st), fromUnix(fin)
		out = append(out, r)
	}
	return out, rows.Err()
}

const runCols = `id, profile_id, date, kind, trigger, slot, started_at, finished_at, status, message,
	candidates, picked, usage`

func (s *Store) Runs(ctx context.Context, profileID int64, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+` FROM runs WHERE profile_id = ? ORDER BY id DESC LIMIT ?`, profileID, limit)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

func (s *Store) RunsOn(ctx context.Context, profileID int64, date string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+` FROM runs WHERE profile_id = ? AND date = ? ORDER BY id DESC`, profileID, date)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// SlotDone はそのプロファイル・その日・その定時枠の実行が既に行われたか。
func (s *Store) SlotDone(ctx context.Context, profileID int64, date, slot string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE profile_id = ? AND date = ? AND slot = ? AND trigger = 'schedule'`,
		profileID, date, slot).Scan(&n)
	return n > 0, err
}
