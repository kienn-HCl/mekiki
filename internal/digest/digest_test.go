package digest

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kienn-HCl/mekiki/internal/ai"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/extract"
	"github.com/kienn-HCl/mekiki/internal/store"
)

// fakeBackend は選定で候補の先頭から pick 件を選び、要約は固定文を返す。
type fakeBackend struct {
	mu          sync.Mutex
	pick        int
	failSelect  bool
	failSummary bool
	prompts     map[string][]string
}

var indexLine = regexp.MustCompile(`(?m)^\[(\d+)\] `)

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Complete(_ context.Context, req ai.Request) (ai.Response, error) {
	f.mu.Lock()
	if f.prompts == nil {
		f.prompts = map[string][]string{}
	}
	f.prompts[req.Stage] = append(f.prompts[req.Stage], req.Prompt)
	f.mu.Unlock()
	u := &ai.Usage{Input: 100, Output: 10}
	switch req.Stage {
	case "select":
		if f.failSelect {
			return ai.Response{}, errors.New("選定失敗")
		}
		n := len(indexLine.FindAllString(req.Prompt, -1))
		var idx []string
		for i := 0; i < min(n, f.pick); i++ {
			idx = append(idx, fmt.Sprint(i))
		}
		return ai.Response{Text: `{"indexes": [` + strings.Join(idx, ",") + `]}`, Usage: u}, nil
	case "overview":
		return ai.Response{Text: "- 今日のまとめ", Usage: u}, nil
	default:
		if f.failSummary {
			return ai.Response{}, errors.New("要約失敗")
		}
		return ai.Response{Text: "要約文です。", Usage: u}, nil
	}
}

// profileID はテスト対象のプロファイル（setupで作る最初のプロファイル）
const profileID = 1

func setup(t *testing.T, backend *fakeBackend) (*Runner, *store.Store, int64) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Timezone = "UTC"
	if err := st.SetSettings(ctx, cfg.Map()); err != nil {
		t.Fatal(err)
	}
	p := config.DefaultProfile("tech", "技術")
	if id, err := st.CreateProfile(ctx, p); err != nil || id != profileID {
		t.Fatalf("CreateProfile: %d %v", id, err)
	}
	if _, err := st.AddFeed(ctx, profileID, "テストフィード", "https://feed.example/rss", "技術"); err != nil {
		t.Fatal(err)
	}
	feeds, _ := st.Feeds(ctx, profileID)
	r := &Runner{
		Store:      st,
		NewBackend: func(config.Settings) (ai.Backend, error) { return backend, nil },
	}
	setPick(t, st, 3)
	return r, st, feeds[0].ID
}

func setPick(t *testing.T, st *store.Store, n int) {
	t.Helper()
	ctx := context.Background()
	p, err := st.Profile(ctx, profileID)
	if err != nil {
		t.Fatal(err)
	}
	p.PickCount = n
	if err := st.UpdateProfile(ctx, p); err != nil {
		t.Fatal(err)
	}
}

// addArticles はn件の記事を追加する。本文・画像を持たせてネットワークに出ないようにする。
func addArticles(t *testing.T, st *store.Store, feedID int64, prefix string, n int) {
	t.Helper()
	now := time.Now()
	var list []store.Article
	for i := 0; i < n; i++ {
		link := fmt.Sprintf("https://news.example/%s/%d", prefix, i)
		list = append(list, store.Article{
			FeedID: feedID, Title: fmt.Sprintf("%s の記事タイトル その%d", prefix, i),
			Link: link, NormLink: extract.NormalizeURL(link),
			PublishedAt: now.Add(-time.Duration(i) * time.Minute), FetchedAt: now,
			Body:  "<p>" + strings.Repeat("十分な長さの本文。", 60) + "</p>",
			Image: "https://img.example/" + prefix + ".png",
		})
	}
	if _, err := st.InsertArticles(context.Background(), list); err != nil {
		t.Fatal(err)
	}
}

func today() string { return time.Now().UTC().Format("2006-01-02") }

func run(t *testing.T, r *Runner, kind string) {
	t.Helper()
	if err := r.Run(context.Background(), Options{ProfileID: profileID, Kind: kind, Trigger: TriggerManual}); err != nil {
		t.Fatalf("Run(%s): %v", kind, err)
	}
}

func TestAppendUsesOnlyNewArticles(t *testing.T) {
	fb := &fakeBackend{pick: 3}
	r, st, feedID := setup(t, fb)
	ctx := context.Background()

	addArticles(t, st, feedID, "朝", 5)
	run(t, r, KindAppend)
	items, _ := st.PageItems(ctx, profileID, today())
	if len(items) != 3 {
		t.Fatalf("1回目: %d件, want 3", len(items))
	}

	// 新着なし → 何も追加されず skipped
	run(t, r, KindAppend)
	items, _ = st.PageItems(ctx, profileID, today())
	if len(items) != 3 {
		t.Fatalf("新着なしの2回目: %d件, want 3", len(items))
	}
	runs, _ := st.RunsOn(ctx, profileID, today())
	if runs[0].Status != "skipped" {
		t.Errorf("新着なしの実行は skipped のはず: %+v", runs[0])
	}

	// 新着2件 → 選定に渡るのは新着の2件だけ（1回目で選ばれなかった2件は再び候補にならない）
	addArticles(t, st, feedID, "昼", 2)
	run(t, r, KindAppend)
	items, _ = st.PageItems(ctx, profileID, today())
	if len(items) != 5 {
		t.Fatalf("追記後: %d件, want 5", len(items))
	}
	last := fb.prompts["select"][len(fb.prompts["select"])-1]
	if n := len(indexLine.FindAllString(last, -1)); n != 2 {
		t.Errorf("追記の選定候補数 = %d, want 2（前回の候補が混ざっている）:\n%s", n, last)
	}
	if !strings.Contains(last, "本日すでにページに掲載済みの記事") {
		t.Error("掲載済み記事が選定プロンプトに渡されていない")
	}
	// 表示は新しい実行が先
	if !strings.HasPrefix(items[0].Title, "昼") {
		t.Errorf("最新の追記分が先頭に来ていない: %s", items[0].Title)
	}
	page, ok, _ := st.Page(ctx, profileID, today())
	if !ok || page.Overview != "- 今日のまとめ" {
		t.Errorf("今日のまとめが保存されていない: %+v", page)
	}
}

func TestRebuildReplacesPage(t *testing.T) {
	fb := &fakeBackend{pick: 2}
	r, st, feedID := setup(t, fb)
	ctx := context.Background()
	setPick(t, st, 2)

	addArticles(t, st, feedID, "朝", 4)
	run(t, r, KindAppend)
	addArticles(t, st, feedID, "昼", 2)
	run(t, r, KindAppend)
	if items, _ := st.PageItems(ctx, profileID, today()); len(items) != 4 {
		t.Fatalf("再作成前: %d件, want 4", len(items))
	}

	fb.pick = 3
	setPick(t, st, 3)
	run(t, r, KindRebuild)
	items, _ := st.PageItems(ctx, profileID, today())
	if len(items) != 3 {
		t.Fatalf("再作成後: %d件, want 3", len(items))
	}
	// 再作成では今日の全候補（6件）が選定に渡る
	last := fb.prompts["select"][len(fb.prompts["select"])-1]
	if n := len(indexLine.FindAllString(last, -1)); n != 6 {
		t.Errorf("再作成の候補数 = %d, want 6", n)
	}
	if strings.Contains(last, "本日すでにページに掲載済みの記事") {
		t.Error("再作成なのに掲載済み記事が渡されている")
	}
}

func TestSelectFailureKeepsCandidatesAndPage(t *testing.T) {
	fb := &fakeBackend{pick: 2}
	r, st, feedID := setup(t, fb)
	ctx := context.Background()

	addArticles(t, st, feedID, "朝", 3)
	run(t, r, KindAppend)

	// 再作成中に選定が失敗しても、元のページは消えない
	fb.failSelect = true
	if err := r.Run(ctx, Options{ProfileID: profileID, Kind: KindRebuild, Trigger: TriggerManual}); err == nil {
		t.Fatal("選定失敗がエラーになっていない")
	}
	if items, _ := st.PageItems(ctx, profileID, today()); len(items) != 2 {
		t.Fatalf("失敗した再作成でページが変わった: %d件", len(items))
	}

	// 選定失敗した追記の候補は消費されず、次回に回る
	addArticles(t, st, feedID, "昼", 2)
	if err := r.Run(ctx, Options{ProfileID: profileID, Kind: KindAppend, Trigger: TriggerManual}); err == nil {
		t.Fatal("選定失敗がエラーになっていない")
	}
	fb.failSelect = false
	fb.pick = 10
	setPick(t, st, 10)
	run(t, r, KindAppend)
	if items, _ := st.PageItems(ctx, profileID, today()); len(items) != 4 {
		t.Fatalf("失敗後の再実行: %d件, want 4", len(items))
	}
}

func TestAllSummariesFailedIsRunFailure(t *testing.T) {
	fb := &fakeBackend{pick: 2, failSummary: true}
	r, st, feedID := setup(t, fb)
	ctx := context.Background()
	addArticles(t, st, feedID, "朝", 3)
	if err := r.Run(ctx, Options{ProfileID: profileID, Kind: KindAppend, Trigger: TriggerManual}); err == nil {
		t.Fatal("要約全滅がエラーになっていない")
	}
	if _, ok, _ := st.Page(ctx, profileID, today()); ok {
		t.Error("要約全滅なのにページが作られた")
	}
	// 候補は消費されない
	fb.failSummary = false
	run(t, r, KindAppend)
	if items, _ := st.PageItems(ctx, profileID, today()); len(items) != 2 {
		t.Fatalf("復旧後: %d件, want 2", len(items))
	}
}

func TestAlreadyPostedURLExcluded(t *testing.T) {
	fb := &fakeBackend{pick: 5}
	r, st, feedID := setup(t, fb)
	ctx := context.Background()
	addArticles(t, st, feedID, "朝", 1)
	run(t, r, KindAppend)

	// 別フィードに同じ記事（トラッキング付きURL）が流れてきても再掲しない
	st.AddFeed(ctx, profileID, "別フィード", "https://other.example/rss", "技術")
	list, _ := st.Feeds(ctx, profileID)
	other := list[len(list)-1].ID
	link := "https://news.example/朝/0?utm_source=other"
	st.InsertArticles(ctx, []store.Article{{
		FeedID: other, Title: "別タイトルだが同じ記事", Link: link, NormLink: extract.NormalizeURL(link),
		PublishedAt: time.Now(), FetchedAt: time.Now(), Body: strings.Repeat("本文", 200),
		Image: "https://img.example/x.png",
	}})
	run(t, r, KindAppend)
	if items, _ := st.PageItems(ctx, profileID, today()); len(items) != 1 {
		t.Fatalf("同じ記事が再掲された: %d件", len(items))
	}
}

func TestParseIndexes(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{`{"indexes": [0, 2, 5]}`, []int{0, 2, 5}},
		{"[1, 2]", []int{1, 2}},
		{"```json\n[3, 1]\n```", []int{3, 1}},
		{`選びました: {"indexes": [4, 99, -1, 1.5, "2"]}`, []int{4}},
	}
	for _, c := range cases {
		got, err := ParseIndexes(c.in, 10)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%q: got %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := ParseIndexes("選べませんでした", 10); err == nil {
		t.Error("JSONでない応答がエラーにならない")
	}
}

func TestProfilesAreIndependent(t *testing.T) {
	fb := &fakeBackend{pick: 5}
	r, st, techFeed := setup(t, fb)
	ctx := context.Background()
	setPick(t, st, 5)

	news := config.DefaultProfile("news", "ニュース")
	news.SelectCriteria = "- ニュース用の基準"
	news.UserMemory = "政治と経済に関心"
	newsID, err := st.CreateProfile(ctx, news)
	if err != nil {
		t.Fatal(err)
	}
	// 同じフィードURLを両方のプロファイルに登録できる
	if ok, _ := st.AddFeed(ctx, newsID, "テストフィード", "https://feed.example/rss", "ニュース"); !ok {
		t.Fatal("別プロファイルに同じURLを登録できない")
	}
	nf, _ := st.Feeds(ctx, newsID)
	addArticles(t, st, techFeed, "技術", 2)
	addArticles(t, st, nf[0].ID, "政治", 3)

	run(t, r, KindAppend) // tech
	if err := r.Run(ctx, Options{ProfileID: newsID, Kind: KindAppend, Trigger: TriggerManual}); err != nil {
		t.Fatal(err)
	}
	techItems, _ := st.PageItems(ctx, profileID, today())
	newsItems, _ := st.PageItems(ctx, newsID, today())
	if len(techItems) != 2 || len(newsItems) != 3 {
		t.Fatalf("tech=%d news=%d, want 2 and 3", len(techItems), len(newsItems))
	}
	for _, it := range newsItems {
		if !strings.HasPrefix(it.Title, "政治") {
			t.Errorf("ニュースのページに他プロファイルの記事: %s", it.Title)
		}
	}
	last := fb.prompts["select"][len(fb.prompts["select"])-1]
	if !strings.Contains(last, "ニュース用の基準") || !strings.Contains(last, "政治と経済に関心") {
		t.Error("プロファイルの選定基準・興味が選定プロンプトに入っていない")
	}
	// 再作成は自分のプロファイルのページだけを置き換える
	run(t, r, KindRebuild)
	if items, _ := st.PageItems(ctx, newsID, today()); len(items) != 3 {
		t.Errorf("techの再作成でnewsのページが変わった: %d件", len(items))
	}
	// プロファイル削除で配下がすべて消える
	if err := st.DeleteProfile(ctx, newsID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Page(ctx, newsID, today()); ok {
		t.Error("削除したプロファイルのページが残っている")
	}
	if f, _ := st.Feeds(ctx, newsID); len(f) != 0 {
		t.Error("削除したプロファイルのフィードが残っている")
	}
}
