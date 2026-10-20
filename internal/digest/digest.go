// Package digest は要約実行（記事選定→要約→日付ページへの追記→任意でDiscord投稿）を担う。
package digest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/kienn-HCl/mekiki/internal/ai"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/discord"
	"github.com/kienn-HCl/mekiki/internal/extract"
	"github.com/kienn-HCl/mekiki/internal/feeds"
	"github.com/kienn-HCl/mekiki/internal/store"
)

const (
	KindAppend  = "append"  // 前回実行からの差分をその日のページに追記（デフォルト）
	KindRebuild = "rebuild" // その日のページを消して作り直す

	TriggerSchedule = "schedule"
	TriggerManual   = "manual"

	FailureMessage = "（要約の生成に失敗しました。記事本文をご確認ください）"

	selectTimeout   = 5 * time.Minute
	summaryTimeout  = 3 * time.Minute
	fallbackTimeout = 4 * time.Minute
	overviewTimeout = 4 * time.Minute

	summaryWorkers = 5
	pageTimeout    = 15 * time.Second
	htmlMaxChars   = 200_000
	textMaxChars   = 20_000
	textMinChars   = 300 // これ未満しか本文が取れないときは次の手段に進む
	// 1回の選定に渡す候補の上限（超過分は新しいものを優先して切り詰める）
	selectMaxArticles = 3000
	// タイトル一致dedupで参照する過去ページの範囲
	titleDedupDays = 14
)

// Runner は要約実行の本体。同時に1つしか走らせない。
type Runner struct {
	Store *store.Store
	// FetchFeeds は実行前にフィードを取得する関数（取得処理側の排他を通すため外から渡す）
	FetchFeeds func(ctx context.Context) error
	// NewBackend は設定からClaude呼び出しの経路を作る（テストで差し替える）
	NewBackend func(s config.Settings) (ai.Backend, error)

	mu      sync.Mutex
	running bool
}

var ErrBusy = errors.New("要約実行中です")

type Options struct {
	Kind    string
	Trigger string
	Slot    string // 定時実行の枠（"07:00"）
}

func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

func (r *Runner) begin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

func (r *Runner) end() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

// Run は要約実行を1回行う。結果は runs テーブルに記録される。実行中なら ErrBusy。
func (r *Runner) Run(ctx context.Context, opt Options) error {
	if !r.begin() {
		return ErrBusy
	}
	defer r.end()
	return r.run(ctx, opt)
}

// Start は要約実行をバックグラウンドで開始する。実行中なら ErrBusy。
// 実行中フラグは戻る前に立つので、直後の画面表示から実行中として見える。
func (r *Runner) Start(opt Options) error {
	if !r.begin() {
		return ErrBusy
	}
	go func() {
		defer r.end()
		if err := r.run(context.Background(), opt); err != nil {
			slog.Error("要約実行失敗", "kind", opt.Kind, "trigger", opt.Trigger, "slot", opt.Slot, "err", err)
		}
	}()
	return nil
}

func (r *Runner) run(ctx context.Context, opt Options) error {
	kv, err := r.Store.Settings(ctx)
	if err != nil {
		return err
	}
	cfg := config.Load(kv)
	now := time.Now()
	date := now.In(cfg.Location()).Format("2006-01-02")

	run := store.Run{Date: date, Kind: opt.Kind, Trigger: opt.Trigger, Slot: opt.Slot, StartedAt: now}
	run.ID, err = r.Store.StartRun(ctx, run)
	if err != nil {
		return err
	}
	tracker := ai.NewTracker()
	j := &job{r: r, cfg: cfg, date: date, run: &run, tracker: tracker}
	runErr := j.do(ctx)

	run.FinishedAt = time.Now()
	run.Usage = tracker.JSON()
	switch {
	case runErr == nil:
		if run.Status == "" {
			run.Status = "ok"
		}
	default:
		run.Status = "failed"
		run.Message = runErr.Error()
	}
	if err := r.Store.FinishRun(context.WithoutCancel(ctx), run); err != nil {
		slog.Error("実行結果の記録に失敗", "err", err)
	}
	slog.Info("要約実行終了", "run", run.ID, "status", run.Status, "message", run.Message, "usage", tracker.Line())
	if runErr != nil && cfg.DiscordWebhook != "" && cfg.DiscordAfterFail {
		msg := fmt.Sprintf("-# ⚠️ mekiki 要約実行失敗（%s）: %s", date, oneLine(runErr.Error(), 200))
		if err := discord.New(cfg.DiscordWebhook).SendContent(context.WithoutCancel(ctx), msg); err != nil {
			slog.Error("失敗通知の送信自体に失敗", "err", err)
		}
	}
	return runErr
}

type job struct {
	r       *Runner
	cfg     config.Settings
	date    string
	run     *store.Run
	tracker *ai.Tracker
	backend ai.Backend
}

func (j *job) do(ctx context.Context) error {
	st := j.r.Store
	backend, err := j.r.NewBackend(j.cfg)
	if err != nil {
		return err
	}
	j.backend = backend

	// 再作成では、その日の掲載分・選定済み印は無いものとして候補を作る。
	// ページ自体は選定・要約が成功してから消す（失敗時に元のページを失わないため）
	ignoreDate := ""
	if j.run.Kind == KindRebuild {
		ignoreDate = j.date
	}

	if j.r.FetchFeeds != nil {
		if err := j.r.FetchFeeds(ctx); err != nil {
			slog.Warn("実行前のフィード取得に失敗（取得済みの記事で続行）", "err", err)
		}
	}

	now := time.Now()
	candidates, err := st.Candidates(ctx, now.Add(-time.Duration(j.cfg.HoursBack)*time.Hour), ignoreDate)
	if err != nil {
		return err
	}
	titles, err := st.RecentPageTitles(ctx, now.AddDate(0, 0, -titleDedupDays), ignoreDate)
	if err != nil {
		return err
	}
	kept, dropped := feeds.Dedup(candidates, titles)
	if len(kept) > selectMaxArticles {
		kept = newest(kept, selectMaxArticles)
	}
	j.run.Candidates = len(kept)
	slog.Info("要約実行開始", "run", j.run.ID, "kind", j.run.Kind, "candidates", len(kept), "dedup_dropped", len(dropped))

	if len(kept) == 0 {
		if err := j.clearIfRebuild(ctx); err != nil {
			return err
		}
		if err := st.MarkConsidered(ctx, ids(dropped), j.date, j.run.ID); err != nil {
			return err
		}
		j.run.Status = "skipped"
		j.run.Message = "前回実行以降の新着記事はありません"
		return nil
	}

	var existing []store.PageItem
	if j.run.Kind != KindRebuild {
		if existing, err = st.PageItems(ctx, j.date); err != nil {
			return err
		}
	}

	picked, err := j.selectArticles(ctx, kept, existing)
	if err != nil {
		// 選定に失敗した記事は「選定済み」にしない。次回の実行で再び候補になる
		return fmt.Errorf("記事選定に失敗: %w", err)
	}
	j.run.Picked = len(picked)

	results := j.summarizeAll(ctx, picked)
	okCount := 0
	for _, res := range results {
		if res.ok {
			okCount++
		}
	}
	// 全件失敗はClaude呼び出し自体の全体障害とみなし、ページに載せず候補も消費しない
	if len(results) > 0 && okCount == 0 {
		return fmt.Errorf("要約が全件失敗しました（%d件）。AI連携の設定・認証を確認してください", len(results))
	}

	items := make([]store.PageItem, len(results))
	for i, res := range results {
		a := res.article
		items[i] = store.PageItem{
			RunID:     j.run.ID,
			Position:  len(existing) + i,
			Title:     a.Title,
			Link:      a.Link,
			NormLink:  a.NormLink,
			Category:  a.Category,
			FeedName:  a.FeedName,
			Summary:   res.summary,
			SummaryOK: res.ok,
			Stage:     res.stage,
			Image:     res.image,
		}
	}
	if err := j.clearIfRebuild(ctx); err != nil {
		return err
	}
	saveAt := time.Now()
	if len(items) > 0 {
		if err := st.AppendItems(ctx, j.date, items, saveAt); err != nil {
			return fmt.Errorf("ページへの保存に失敗: %w", err)
		}
	}
	if err := st.MarkConsidered(ctx, append(ids(kept), ids(dropped)...), j.date, j.run.ID); err != nil {
		return err
	}
	if len(items) == 0 {
		j.run.Status = "skipped"
		j.run.Message = "注目記事として選ばれたものはありませんでした"
		return nil
	}
	j.run.Message = fmt.Sprintf("%d件を追加（要約成功%d件）", len(items), okCount)

	if j.cfg.Overview {
		j.updateOverview(ctx)
	}
	if j.cfg.DiscordWebhook != "" {
		j.postDiscord(ctx, items)
	}
	return nil
}

func (j *job) clearIfRebuild(ctx context.Context) error {
	if j.run.Kind != KindRebuild {
		return nil
	}
	if err := j.r.Store.ClearPage(ctx, j.date); err != nil {
		return fmt.Errorf("ページの削除に失敗: %w", err)
	}
	return nil
}

// selectArticles は候補タイトル一覧を1回のClaude呼び出しに渡して記事を選ばせる。
func (j *job) selectArticles(ctx context.Context, candidates []store.Article, existing []store.PageItem) ([]store.Article, error) {
	prompt := buildSelectPrompt(candidates, existing, j.cfg.PickCount, j.cfg.SelectCriteria, j.cfg.UserMemory)
	resp, err := j.backend.Complete(ctx, ai.Request{
		Stage: "select", Prompt: prompt, JSONSchema: selectSchema, Timeout: selectTimeout,
	})
	j.tracker.Record("select", resp.Usage)
	if err != nil {
		return nil, err
	}
	idx, err := ParseIndexes(resp.Text, len(candidates))
	if err != nil {
		return nil, err
	}
	return PickByIndexes(candidates, idx, j.cfg.PickCount), nil
}

// ParseIndexes は選定応答をパースし、範囲内のindexだけを返す（範囲外は無視）。
// {"indexes": [...]} と素の配列 [...] の両方を受け付ける（CLI経路は形式が揺れるため）。
func ParseIndexes(text string, n int) ([]int, error) {
	var obj struct {
		Indexes []any `json:"indexes"`
	}
	var raw []any
	if err := ai.LenientJSON(text, &obj); err == nil && obj.Indexes != nil {
		raw = obj.Indexes
	} else if err := ai.LenientJSON(text, &raw); err != nil {
		return nil, err
	}
	var out []int
	for _, v := range raw {
		f, ok := v.(float64)
		if !ok || f != float64(int(f)) {
			continue
		}
		if i := int(f); i >= 0 && i < n {
			out = append(out, i)
		}
	}
	return out, nil
}

// PickByIndexes はindex順に記事を取り出す。正規化URLの重複は除き、最大limit件。
func PickByIndexes(candidates []store.Article, idx []int, limit int) []store.Article {
	var out []store.Article
	seen := map[string]bool{}
	for _, i := range idx {
		a := candidates[i]
		if seen[a.NormLink] {
			continue
		}
		seen[a.NormLink] = true
		out = append(out, a)
		if len(out) >= limit {
			break
		}
	}
	return out
}

type summaryResult struct {
	article store.Article
	summary string
	ok      bool
	stage   string // feed | page | fallback | failed
	image   string
}

func (j *job) summarizeAll(ctx context.Context, picked []store.Article) []summaryResult {
	results := make([]summaryResult, len(picked))
	sem := make(chan struct{}, summaryWorkers)
	var wg sync.WaitGroup
	for i, a := range picked {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = j.summarizeOne(ctx, a)
		}()
	}
	wg.Wait()
	return results
}

// summarizeOne は要約入力を フィード本文 → ページテキスト → WebFetch救済 の順で決めて要約し、
// 記事画像（フィード由来 → og:image）も決める。
func (j *job) summarizeOne(ctx context.Context, a store.Article) summaryResult {
	res := summaryResult{article: a, stage: "feed"}
	var pageHTML string
	pageTried := false
	fetch := func() {
		if !pageTried {
			pageTried = true
			pageHTML = fetchPage(ctx, a.Link)
		}
	}

	text := extract.HTMLToText(a.Body)
	if len([]rune(text)) < textMinChars {
		fetch()
		if pageHTML != "" {
			text = extract.HTMLToText(pageHTML)
			res.stage = "page"
		}
	}
	text = truncateRunes(text, textMaxChars)

	if len([]rune(text)) >= textMinChars {
		resp, err := j.backend.Complete(ctx, ai.Request{
			Stage: "summarize", Prompt: buildSummaryPrompt(a.Title, text, j.cfg.UserMemory),
			Timeout: summaryTimeout,
		})
		j.tracker.Record("summarize", resp.Usage)
		if err != nil {
			slog.Warn("要約失敗", "url", a.Link, "err", err)
		} else if resp.Text != "" {
			res.summary, res.ok = resp.Text, true
		}
	} else {
		// 本文がほぼ取れない → WebFetch救済（タイトルのみの要約もこのプロンプトが兼ねる）
		resp, err := j.backend.Complete(ctx, ai.Request{
			Stage: "summarize_fallback", Prompt: buildFallbackPrompt(a.Title, a.Link),
			AllowWebFetch: j.cfg.WebFetch, Timeout: fallbackTimeout,
		})
		j.tracker.Record("summarize_fallback", resp.Usage)
		if err != nil {
			slog.Warn("救済要約失敗", "url", a.Link, "err", err)
		} else if resp.Text != "" {
			res.summary, res.ok, res.stage = resp.Text, true, "fallback"
		}
	}
	if !res.ok {
		res.summary, res.stage = FailureMessage, "failed"
	}

	if a.Image != "" {
		res.image = extract.SanitizeImageURL(a.Image, a.Link)
	}
	if res.image == "" {
		// 画像のためだけのページ取得はClaudeのトークンを消費しないので行ってよい
		fetch()
		if og := extract.OGImage(pageHTML); og != "" {
			res.image = extract.SanitizeImageURL(og, a.Link)
		}
	}
	return res
}

func (j *job) updateOverview(ctx context.Context) {
	all, err := j.r.Store.PageItems(ctx, j.date)
	if err != nil || len(all) == 0 {
		return
	}
	var ok []store.PageItem
	for _, it := range all {
		if it.SummaryOK {
			ok = append(ok, it)
		}
	}
	if len(ok) == 0 {
		return
	}
	resp, err := j.backend.Complete(ctx, ai.Request{
		Stage: "overview", Prompt: buildOverviewPrompt(ok, j.cfg.UserMemory), Timeout: overviewTimeout,
	})
	j.tracker.Record("overview", resp.Usage)
	if err != nil {
		slog.Warn("今日のまとめの生成に失敗", "err", err)
		return
	}
	if err := j.r.Store.SetOverview(ctx, j.date, resp.Text, time.Now()); err != nil {
		slog.Warn("今日のまとめの保存に失敗", "err", err)
	}
}

func (j *job) postDiscord(ctx context.Context, items []store.PageItem) {
	c := discord.New(j.cfg.DiscordWebhook)
	label := "追加"
	if j.run.Kind == KindRebuild {
		label = "再作成"
	}
	header := fmt.Sprintf("## %s\n%s — %s%d件",
		j.cfg.DiscordTitle, time.Now().In(j.cfg.Location()).Format("2006/01/02 15:04 MST"), label, len(items))
	if base := strings.TrimRight(j.cfg.DiscordBaseURL, "/"); base != "" {
		header += fmt.Sprintf("\n%s/day/%s", base, j.date)
	}
	if err := c.SendContent(ctx, header); err != nil {
		slog.Error("Discordへのヘッダー送信に失敗（投稿を中止）", "err", err)
		return
	}
	embeds := make([]discord.Embed, len(items))
	for i, it := range items {
		embeds[i] = discord.NewEmbed(it.Title, it.Link, it.Summary,
			it.Category+" / "+it.FeedName, it.Image, j.cfg.DiscordColor)
	}
	posted := c.PostEmbeds(ctx, embeds, j.cfg.DiscordEmbeds)
	slog.Info("Discord投稿完了", "posted", len(posted), "total", len(embeds))
	if err := c.SendContent(ctx, "-# "+j.tracker.Line()); err != nil {
		slog.Warn("使用量サマリの送信失敗", "err", err)
	}
}

func fetchPage(ctx context.Context, link string) string {
	ctx, cancel := context.WithTimeout(ctx, pageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", feeds.UserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Info("ページ取得失敗", "url", link, "err", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		slog.Info("ページ取得失敗", "url", link, "status", resp.StatusCode)
		return ""
	}
	// 1文字最大4バイトとしてバイト数を制限してから文字数で切る
	body := io.LimitReader(resp.Body, htmlMaxChars*4)
	r, err := charset.NewReader(body, resp.Header.Get("Content-Type"))
	if err != nil {
		r = body
	}
	data, err := io.ReadAll(r)
	if err != nil && len(data) == 0 {
		return ""
	}
	return truncateRunes(string(data), htmlMaxChars)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func ids(list []store.Article) []int64 {
	out := make([]int64, len(list))
	for i, a := range list {
		out[i] = a.ID
	}
	return out
}

// newest は公開（不明なら取得）日時の新しい順にn件を残す。元の並び順は保つ。
func newest(list []store.Article, n int) []store.Article {
	type kv struct {
		i int
		t time.Time
	}
	ts := make([]kv, len(list))
	for i, a := range list {
		t := a.PublishedAt
		if t.IsZero() {
			t = a.FetchedAt
		}
		ts[i] = kv{i, t}
	}
	sorted := append([]kv(nil), ts...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].t.After(sorted[b].t) })
	keep := map[int]bool{}
	for _, e := range sorted[:n] {
		keep[e.i] = true
	}
	var out []store.Article
	for i, a := range list {
		if keep[i] {
			out = append(out, a)
		}
	}
	return out
}
