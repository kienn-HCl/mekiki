package conffile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/store"
)

func setup(t *testing.T) (*store.Store, *Manager, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	p := config.DefaultProfile("tech", "Tech")
	p.UserMemory = "Rustが好き\nGoも好き"
	id, _ := st.CreateProfile(ctx, p)
	st.AddFeed(ctx, id, "A", "https://a.example/rss", "技術")
	kv := config.Defaults()
	kv.APIKeys[config.BackendAnthropic] = "sk-ant-secret"
	st.SetSettings(ctx, kv.Map())
	path := filepath.Join(dir, "config.toml")
	m := New(st, path)
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return st, m, path
}

func read(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInitWritesReadableFile(t *testing.T) {
	_, _, path := setup(t)
	s := read(t, path)
	for _, want := range []string{
		"# mekiki の設定ファイル", "[settings]", `backend = 'anthropic'`, `api_key = 'sk-ant-secret'`,
		"[[profiles]]", `slug = 'tech'`, "interests = \"\"\"\nRustが好き\nGoも好き\"\"\"", "[settings.ai.compat]", "url = ''", "[[profiles.feeds]]",
		`url = 'https://a.example/rss'`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("設定ファイルに %q がない:\n%s", want, s)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("パーミッション: %v", fi.Mode().Perm())
	}
}

func TestEditFileAppliesToDB(t *testing.T) {
	st, m, path := setup(t)
	ctx := context.Background()
	s := read(t, path)
	s = strings.Replace(s, `name = 'Tech'`, `name = '技術記事'`, 1)
	s = strings.Replace(s, "digest_times = ['07:00']", "digest_times = ['07:00', '19:00']", 1)
	s += `
[[profiles.feeds]]
title = 'B'
url = 'https://b.example/rss'
category = '技術'
enabled = true

[[profiles]]
slug = 'news'
name = 'ニュース'
select_criteria = '- 信頼できるもの'
pick_count = 10
hours_back = 24
overview = true
digest_times = []
`
	if err := m.apply(ctx, []byte(s), "test"); err != nil {
		t.Fatal(err)
	}
	list, _ := st.Profiles(ctx)
	if len(list) != 2 || list[0].Name != "技術記事" || len(list[0].DigestTimes) != 2 || list[1].Slug != "news" {
		t.Fatalf("profiles: %+v", list)
	}
	if list[1].DiscordEmbeds != 5 {
		t.Errorf("省略したembed数の既定: %d", list[1].DiscordEmbeds)
	}
	feeds, _ := st.Feeds(ctx, list[0].ID)
	if len(feeds) != 2 {
		t.Errorf("フィード追加: %+v", feeds)
	}
	// 書き出し直しても内容が保たれる
	if err := m.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, path), `slug = 'news'`) {
		t.Error("追加したプロファイルが書き出されない")
	}
}

func TestInvalidFileKeepsState(t *testing.T) {
	st, m, path := setup(t)
	ctx := context.Background()
	orig := read(t, path)
	cases := map[string]string{
		"構文エラー":      orig + "\n[[profiles]\n",
		"知らない項目":     strings.Replace(orig, "pick_count", "pick_cnt", 1),
		"不正な時刻":      strings.Replace(orig, "'07:00'", "'25:00'", 1),
		"プロファイルなし":   orig[:strings.Index(orig, "[[profiles]]")],
		"不正なフィードURL": strings.Replace(orig, "https://a.example/rss", "ftp://x", 1),
	}
	for name, data := range cases {
		if err := m.apply(ctx, []byte(data), "test"); err == nil {
			t.Errorf("%s: エラーにならない", name)
		} else if m.LastError() == "" {
			t.Errorf("%s: エラーが保持されない", name)
		}
		list, _ := st.Profiles(ctx)
		if len(list) != 1 || list[0].Slug != "tech" {
			t.Errorf("%s: DBが変わった: %+v", name, list)
		}
	}
	if _, err := Decode([]byte(strings.Replace(orig, "pick_count", "pick_cnt", 1))); err == nil ||
		!strings.Contains(err.Error(), "profiles.pick_cnt") {
		t.Errorf("知らない項目のエラーに項目名がない: %v", err)
	}
	if err := m.apply(ctx, []byte(orig), "test"); err != nil || m.LastError() != "" {
		t.Errorf("正しいファイルに戻してもエラーが消えない: %v %q", err, m.LastError())
	}
}

func TestRemovingProfileBacksUpDB(t *testing.T) {
	st, m, path := setup(t)
	ctx := context.Background()
	id2, _ := st.CreateProfile(ctx, config.DefaultProfile("news", "ニュース"))
	st.AppendItems(ctx, id2, "2026-10-20", []store.PageItem{{Title: "x", Link: "https://x", NormLink: "https://x",
		Summary: "s", SummaryOK: true, Stage: "feed"}}, time.Now(), false)
	m.Save(ctx)
	s := read(t, path)
	s = s[:strings.LastIndex(s[:strings.Index(s, "slug = 'news'")], "[[profiles]]")]
	if err := m.apply(ctx, []byte(s), "test"); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.Profiles(ctx); len(list) != 1 {
		t.Errorf("削除されていない: %+v", list)
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "backups", "mekiki-*.db"))
	if len(backups) != 1 {
		t.Errorf("削除前のバックアップがない: %v", backups)
	}
}

func TestSlugRenameKeepsPages(t *testing.T) {
	st, m, path := setup(t)
	ctx := context.Background()
	list, _ := st.Profiles(ctx)
	st.AppendItems(ctx, list[0].ID, "2026-10-20", []store.PageItem{{Title: "x", Link: "https://x", NormLink: "https://x",
		Summary: "s", SummaryOK: true, Stage: "feed"}}, time.Now(), false)
	s := strings.Replace(read(t, path), `slug = 'tech'`, `slug = 'dev'`, 1)
	if err := m.apply(ctx, []byte(s), "test"); err != nil {
		t.Fatal(err)
	}
	p, err := st.ProfileBySlug(ctx, "dev")
	if err != nil || p.ID != list[0].ID {
		t.Fatalf("slug変更でIDが変わった: %+v %v", p, err)
	}
	if items, _ := st.PageItems(ctx, p.ID, "2026-10-20"); len(items) != 1 {
		t.Error("slug変更でページが失われた")
	}
}

func TestWatchPicksUpEdits(t *testing.T) {
	st, m, path := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Watch(ctx)
	s := strings.Replace(read(t, path), "pick_count = 20", "pick_count = 7", 1)
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(path, future, future) // 同じ秒内の書き込みでも更新時刻が変わるように
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if p, _ := st.ProfileBySlug(context.Background(), "tech"); p.PickCount == 7 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Error("ファイルの編集が反映されない")
}

func TestStripSecrets(t *testing.T) {
	st, _, _ := setup(t)
	ctx := context.Background()
	list, _ := st.Profiles(ctx)
	list[0].DiscordWebhook = "https://discord.com/api/webhooks/1/x"
	st.UpdateProfile(ctx, list[0])
	f, _ := Export(ctx, st)
	f.StripSecrets()
	data, _ := Encode(f)
	if strings.Contains(string(data), "sk-ant-secret") || strings.Contains(string(data), "webhooks/1/x") {
		t.Error("秘密情報が残っている")
	}
}

func TestWatchClearsErrorWhenFixed(t *testing.T) {
	_, m, path := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Watch(ctx)
	orig := read(t, path)
	write := func(s string, at time.Time) {
		os.WriteFile(path, []byte(s), 0o600)
		os.Chtimes(path, at, at)
	}
	wait := func(cond func() bool) bool {
		for d := time.Now().Add(8 * time.Second); time.Now().Before(d); time.Sleep(200 * time.Millisecond) {
			if cond() {
				return true
			}
		}
		return false
	}
	write(strings.Replace(orig, "pick_count", "pick_cnt", 1), time.Now().Add(2*time.Second))
	if !wait(func() bool { return m.LastError() != "" }) {
		t.Fatal("書き間違いが検出されない")
	}
	write(orig, time.Now().Add(4*time.Second)) // 元に戻す（最後に反映した内容と同じ）
	if !wait(func() bool { return m.LastError() == "" }) {
		t.Error("直してもエラーが消えない")
	}
}
