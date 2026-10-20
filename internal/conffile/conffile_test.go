package conffile

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/store"
)

// setup は既定の設定で起動したときの状態（プロファイル main・フィード1件）を作る。
func setup(t *testing.T) (*store.Store, *Manager, string) {
	t.Helper()
	dir := t.TempDir()
	st, m := open(t, dir)
	ctx := context.Background()
	p, _ := st.ProfileBySlug(ctx, "main")
	p.UserMemory = "Rustが好き\nGoも好き"
	st.UpdateProfile(ctx, p)
	st.AddFeed(ctx, p.ID, "A", "https://a.example/rss", "技術")
	kv, _ := st.Settings(ctx)
	s := config.Load(kv)
	s.APIKeys[config.BackendAnthropic] = "sk-ant-secret"
	st.SetSettings(ctx, s.Map())
	if err := m.Save(ctx); err != nil {
		t.Fatal(err)
	}
	return st, m, dir
}

func open(t *testing.T, dir string) (*store.Store, *Manager) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, dir)
	if err := m.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st, m
}

// restart は同じディレクトリで開き直す（mekikiの再起動に相当）。
func restart(t *testing.T, st *store.Store, dir string) (*store.Store, *Manager, error) {
	t.Helper()
	st.Close()
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	m := New(st2, dir)
	return st2, m, m.Init(context.Background())
}

func read(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFreshStartCreatesDefault(t *testing.T) {
	dir := t.TempDir()
	st, _ := open(t, dir)
	list, _ := st.Profiles(context.Background())
	if len(list) != 1 || list[0].Slug != "main" {
		t.Fatalf("既定のプロファイル: %+v", list)
	}
	s := read(t, filepath.Join(dir, "config.toml"))
	if !strings.Contains(s, "slug = 'main'") || !strings.Contains(s, "[settings.ai.compat]") {
		t.Errorf("既定の設定ファイル:\n%s", s)
	}
}

func TestSaveWritesTomlAndOPML(t *testing.T) {
	_, _, dir := setup(t)
	s := read(t, filepath.Join(dir, "config.toml"))
	for _, want := range []string{"[settings]", `api_key = 'sk-ant-secret'`, "[[profiles]]",
		"interests = \"\"\"\nRustが好き\nGoも好き\"\"\""} {
		if !strings.Contains(s, want) {
			t.Errorf("config.toml に %q がない:\n%s", want, s)
		}
	}
	if strings.Contains(s, "a.example") {
		t.Error("フィードが config.toml に入っている")
	}
	opml := read(t, filepath.Join(dir, "feeds", "main.opml"))
	if !strings.Contains(opml, `xmlUrl="https://a.example/rss"`) || !strings.Contains(opml, `text="技術"`) {
		t.Errorf("OPML:\n%s", opml)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "config.toml")); fi.Mode().Perm() != 0o600 {
		t.Errorf("パーミッション: %v", fi.Mode().Perm())
	}
}

func TestEditedFilesAppliedOnRestart(t *testing.T) {
	st, _, dir := setup(t)
	ctx := context.Background()
	cfg := filepath.Join(dir, "config.toml")
	s := read(t, cfg)
	s = strings.Replace(s, "name = 'メイン'", "name = '技術記事'", 1)
	s = "# 手で書いたコメント\n" + s + `
[[profiles]]
slug = 'news'
name = 'ニュース'
select_criteria = '- 信頼できるもの'
pick_count = 10
hours_back = 24
overview = true
digest_times = ['07:00']
`
	os.WriteFile(cfg, []byte(s), 0o600)
	os.WriteFile(filepath.Join(dir, "feeds", "news.opml"), []byte(`<opml version="1.0"><body>
<outline text="国際"><outline text="N" xmlUrl="https://n.example/rss"/><outline text="Off" xmlUrl="https://off.example/rss" disabled="true"/></outline>
</body></opml>`), 0o600)

	st, m, err := restart(t, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := st.Profiles(ctx)
	if len(list) != 2 || list[0].Name != "技術記事" || list[1].Slug != "news" || list[1].DiscordEmbeds != 5 {
		t.Fatalf("profiles: %+v", list)
	}
	nf, _ := st.Feeds(ctx, list[1].ID)
	if len(nf) != 2 || nf[0].Enabled == nf[1].Enabled {
		t.Errorf("OPMLのフィード（有効/無効）: %+v", nf)
	}
	if !strings.HasPrefix(read(t, cfg), "# 手で書いたコメント") {
		t.Error("起動しただけで設定ファイルが書き直された")
	}
	// 画面から保存すると正規化した形で書き出す
	if err := m.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, cfg), "# 手で書いたコメント") {
		t.Error("保存で書き出されていない")
	}
}

func TestInvalidFileStopsStartup(t *testing.T) {
	st, _, dir := setup(t)
	ctx := context.Background()
	cfg := filepath.Join(dir, "config.toml")
	orig := read(t, cfg)
	cases := map[string]string{
		"構文エラー":    orig + "\n[[profiles]\n",
		"知らない項目":   strings.Replace(orig, "pick_count", "pick_cnt", 1),
		"不正な時刻":    strings.Replace(orig, "'07:00'", "'25:00'", 1),
		"プロファイルなし": orig[:strings.Index(orig, "[[profiles]]")],
	}
	for name, data := range cases {
		os.WriteFile(cfg, []byte(data), 0o600)
		var err error
		st, _, err = restart(t, st, dir)
		if err == nil {
			t.Errorf("%s: 起動できてしまう", name)
		}
		if list, _ := st.Profiles(ctx); len(list) != 1 || list[0].Slug != "main" {
			t.Errorf("%s: DBが変わった: %+v", name, list)
		}
	}
	os.WriteFile(cfg, []byte(strings.Replace(orig, "pick_count", "pick_cnt", 1)), 0o600)
	_, _, err := restart(t, st, dir)
	if err == nil || !strings.Contains(err.Error(), "profiles.pick_cnt") {
		t.Errorf("知らない項目のエラーに項目名がない: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "feeds", "main.opml"), []byte("<opml><body><outline text='x' xmlUrl='ftp://x'/></body></opml>"), 0o600)
	os.WriteFile(cfg, []byte(orig), 0o600)
	if _, _, err := restart(t, st, dir); err == nil {
		t.Error("不正なOPMLで起動できてしまう")
	}
}

func TestEditOnDiskBlocksSave(t *testing.T) {
	_, m, dir := setup(t)
	if err := m.CheckUnchanged(); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.toml")
	os.WriteFile(cfg, []byte(read(t, cfg)+"\n# 編集\n"), 0o600)
	if err := m.CheckUnchanged(); err != ErrEditedOnDisk {
		t.Errorf("外での編集が検出されない: %v", err)
	}
	if err := m.Save(context.Background()); err != ErrEditedOnDisk {
		t.Errorf("外で編集されたファイルを上書きした: %v", err)
	}
	if !strings.Contains(read(t, cfg), "# 編集") {
		t.Error("編集が消えた")
	}
}

func TestSlugRenameMovesOPMLAndKeepsPages(t *testing.T) {
	st, m, dir := setup(t)
	ctx := context.Background()
	p, _ := st.ProfileBySlug(ctx, "main")
	st.AppendItems(ctx, p.ID, "2026-10-20", []store.PageItem{{Title: "x", Link: "https://x", NormLink: "https://x",
		Summary: "s", SummaryOK: true, Stage: "feed"}}, time.Now(), false)
	p.Slug = "dev"
	st.UpdateProfile(ctx, p)
	if err := m.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "feeds", "main.opml")); !os.IsNotExist(err) {
		t.Error("古いOPMLが残っている")
	}
	st, _, err := restart(t, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := st.ProfileBySlug(ctx, "dev")
	if err != nil || p2.ID != p.ID {
		t.Fatalf("slug変更でIDが変わった: %+v %v", p2, err)
	}
	if f, _ := st.Feeds(ctx, p2.ID); len(f) != 1 {
		t.Error("slug変更でフィードが失われた")
	}
	if items, _ := st.PageItems(ctx, p2.ID, "2026-10-20"); len(items) != 1 {
		t.Error("slug変更でページが失われた")
	}
}

func TestRemovingProfileBacksUpDB(t *testing.T) {
	st, m, dir := setup(t)
	ctx := context.Background()
	st.CreateProfile(ctx, config.DefaultProfile("news", "ニュース"))
	m.Save(ctx)
	cfg := filepath.Join(dir, "config.toml")
	s := read(t, cfg)
	s = s[:strings.LastIndex(s[:strings.Index(s, "slug = 'news'")], "[[profiles]]")]
	os.WriteFile(cfg, []byte(s), 0o600)
	st, _, err := restart(t, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := st.Profiles(ctx); len(list) != 1 {
		t.Errorf("削除されていない: %+v", list)
	}
	if backups, _ := filepath.Glob(filepath.Join(dir, "backups", "mekiki-*.db")); len(backups) != 1 {
		t.Errorf("削除前のバックアップがない: %v", backups)
	}
}

func TestZipRoundTripAndRestore(t *testing.T) {
	st, m, _ := setup(t)
	ctx := context.Background()
	b, err := ExportBundle(ctx, st, false)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := b.Zip(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := ReadZip(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["feeds/main.opml"] == nil {
		t.Fatalf("zipの中身: %v", keys(got))
	}
	// フィードを差し替えて復元
	got["feeds/main.opml"] = bytes.Replace(got["feeds/main.opml"], []byte("a.example"), []byte("b.example"), 1)
	if err := m.Restore(ctx, got); err != nil {
		t.Fatal(err)
	}
	p, _ := st.ProfileBySlug(ctx, "main")
	if f, _ := st.Feeds(ctx, p.ID); len(f) != 1 || f[0].URL != "https://b.example/rss" {
		t.Errorf("復元後のフィード: %+v", f)
	}
	// config.toml だけの復元ではフィードは変えない
	if err := m.Restore(ctx, Bundle{"config.toml": got["config.toml"]}); err != nil {
		t.Fatal(err)
	}
	if f, _ := st.Feeds(ctx, p.ID); len(f) != 1 {
		t.Errorf("config.tomlだけの復元でフィードが消えた: %+v", f)
	}
	// 秘密情報を除いた版
	nb, _ := ExportBundle(ctx, st, true)
	if strings.Contains(string(nb["config.toml"]), "sk-ant-secret") {
		t.Error("秘密情報が残っている")
	}
}

func keys(b Bundle) []string {
	var out []string
	for k := range b {
		out = append(out, k)
	}
	return out
}
