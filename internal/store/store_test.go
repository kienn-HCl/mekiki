package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/kienn-HCl/mekiki/internal/config"
)

// v1Schema はプロファイル導入前（最初のWebアプリ版）のスキーマ。移行テスト用
const v1Schema = `
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

func TestMigrateV1(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "mekiki.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		v1Schema,
		`INSERT INTO settings(key, value) VALUES
			('backend', 'api'), ('api_key', 'sk-ant-old'), ('model', 'claude-opus-5-5'),
			('select_criteria', '- 旧基準'), ('user_memory', '旧メモ'), ('pick_count', '7'),
			('digest_times', '06:00,19:00'), ('discord_webhook', 'https://discord.example/hook'),
			('timezone', 'UTC')`,
		`INSERT INTO feeds(id, title, url, category) VALUES(5, 'F', 'https://f.example/rss', '技術')`,
		`INSERT INTO articles(id, feed_id, title, link, norm_link, fetched_at) VALUES(9, 5, 'A', 'https://f.example/a', 'https://f.example/a', 100)`,
		`INSERT INTO considered(article_id, date, run_id) VALUES(9, '2026-10-20', 3)`,
		`INSERT INTO pages(date, overview, created_at, updated_at) VALUES('2026-10-20', '- まとめ', 1, 2)`,
		`INSERT INTO runs(id, date, kind, trigger, slot, started_at, status) VALUES(3, '2026-10-20', 'append', 'schedule', '06:00', 1, 'ok')`,
		`INSERT INTO page_items(date, run_id, position, title, link, norm_link, summary, summary_ok, stage, created_at)
			VALUES('2026-10-20', 3, 0, 'A', 'https://f.example/a', 'https://f.example/a', '要約', 1, 'feed', 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	db.Close()

	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	profiles, _ := st.Profiles(ctx)
	if len(profiles) != 1 || profiles[0].Slug != "main" {
		t.Fatalf("profiles = %+v", profiles)
	}
	p := profiles[0]
	if p.SelectCriteria != "- 旧基準" || p.UserMemory != "旧メモ" || p.PickCount != 7 ||
		len(p.DigestTimes) != 2 || p.DiscordWebhook != "https://discord.example/hook" {
		t.Errorf("旧設定がプロファイルに移っていない: %+v", p)
	}
	kv, _ := st.Settings(ctx)
	s := config.Load(kv)
	if s.Backend != config.BackendAnthropic || s.APIKeys[config.BackendAnthropic] != "sk-ant-old" || s.Timezone != "UTC" {
		t.Errorf("全体設定: %+v", s)
	}
	if _, ok := kv["select_criteria"]; ok {
		t.Error("プロファイルに移した旧キーが settings に残っている")
	}
	feeds, _ := st.Feeds(ctx, p.ID)
	if len(feeds) != 1 || feeds[0].ID != 5 || feeds[0].ArticleCount != 1 {
		t.Errorf("feeds = %+v", feeds)
	}
	items, _ := st.PageItems(ctx, p.ID, "2026-10-20")
	page, ok, _ := st.Page(ctx, p.ID, "2026-10-20")
	if len(items) != 1 || !ok || page.Overview != "- まとめ" {
		t.Errorf("ページが移っていない: items=%d page=%+v", len(items), page)
	}
	if done, _ := st.SlotDone(ctx, p.ID, "2026-10-20", "06:00"); !done {
		t.Error("実行履歴が移っていない")
	}
	// 外部キーが新しいfeedsを指している（フィード削除で記事も消える）
	if err := st.DeleteFeed(ctx, p.ID, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Article(ctx, 9); err != ErrNotFound {
		t.Errorf("フィード削除で記事が消えない（外部キーが壊れている）: %v", err)
	}
	// 再オープンしても移行し直さない
	st.Close()
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if list, _ := st2.Profiles(ctx); len(list) != 1 {
		t.Errorf("再オープンでプロファイルが増えた: %d", len(list))
	}
}

func TestFreshDBAndCandidatesPerProfile(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.EnsureProfile(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateProfile(ctx, config.DefaultProfile("b", "B"))
	if err != nil {
		t.Fatal(err)
	}
	st.AddFeed(ctx, 1, "A", "https://same.example/rss", "")
	st.AddFeed(ctx, b, "B", "https://same.example/rss", "")
	fa, _ := st.Feeds(ctx, 1)
	fb, _ := st.Feeds(ctx, b)
	now := time.Now()
	for _, f := range []Feed{fa[0], fb[0]} {
		st.InsertArticles(ctx, []Article{{FeedID: f.ID, Title: "同じ記事", Link: "https://same.example/1",
			NormLink: "https://same.example/1", PublishedAt: now, FetchedAt: now}})
	}
	// プロファイルAのページに載っても、Bの候補からは消えない
	st.AppendItems(ctx, 1, "2026-10-20", []PageItem{{Title: "同じ記事", Link: "https://same.example/1",
		NormLink: "https://same.example/1", Summary: "s", SummaryOK: true, Stage: "feed"}}, now, false)
	ca, _ := st.Candidates(ctx, 1, now.Add(-time.Hour), "")
	cb, _ := st.Candidates(ctx, b, now.Add(-time.Hour), "")
	if len(ca) != 0 || len(cb) != 1 {
		t.Errorf("候補: A=%d B=%d, want 0 and 1", len(ca), len(cb))
	}
}
