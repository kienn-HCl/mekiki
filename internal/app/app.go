// Package app はフィード取得・要約実行のジョブ管理と定期実行（スケジューラ）を担う。
package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/kienn-HCl/mekiki/internal/ai"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/digest"
	"github.com/kienn-HCl/mekiki/internal/feeds"
	"github.com/kienn-HCl/mekiki/internal/store"
)

var ErrFetchBusy = errors.New("フィード取得中です")

type App struct {
	Store  *store.Store
	Digest *digest.Runner

	fetchMu  sync.Mutex // フィード取得の排他
	stateMu  sync.Mutex
	fetching bool
	last     FetchStatus

	notReadyWarned bool
}

type FetchStatus struct {
	At       time.Time
	Feeds    int
	Failed   int
	Skipped  int
	Added    int
	Duration time.Duration
	Err      string
}

func New(st *store.Store) *App {
	a := &App{Store: st}
	a.Digest = &digest.Runner{
		Store:      st,
		FetchFeeds: a.fetchWait,
		NewBackend: NewBackend,
	}
	return a
}

// NewBackend は設定に応じてClaude呼び出しの経路を作る。
func NewBackend(s config.Settings) (ai.Backend, error) {
	if s.Backend == config.BackendCLI {
		return ai.NewCLI(s.CLIPath, s.CLIModel)
	}
	return ai.NewAPI(s.EffectiveAPIKey(), s.Model, s.Effort)
}

func (a *App) Settings(ctx context.Context) (config.Settings, error) {
	kv, err := a.Store.Settings(ctx)
	if err != nil {
		return config.Settings{}, err
	}
	return config.Load(kv), nil
}

// FetchNow はフィード取得を同期実行する。取得中なら ErrFetchBusy。
func (a *App) FetchNow(ctx context.Context) (feeds.Result, error) {
	if !a.fetchMu.TryLock() {
		return feeds.Result{}, ErrFetchBusy
	}
	defer a.fetchMu.Unlock()
	return a.doFetch(ctx)
}

// fetchWait は取得中なら終わるのを待ってから取得する（要約実行前の取得用）。
func (a *App) fetchWait(ctx context.Context) error {
	a.fetchMu.Lock()
	defer a.fetchMu.Unlock()
	_, err := a.doFetch(ctx)
	return err
}

// StartFetch はフィード取得をバックグラウンドで開始する。取得中なら ErrFetchBusy。
func (a *App) StartFetch() error {
	if !a.fetchMu.TryLock() {
		return ErrFetchBusy
	}
	a.setFetching(true)
	go func() {
		defer a.fetchMu.Unlock()
		if _, err := a.doFetch(context.Background()); err != nil {
			slog.Error("フィード取得失敗", "err", err)
		}
	}()
	return nil
}

// doFetch は呼び出し側が fetchMu を持っている前提で取得する。
func (a *App) doFetch(ctx context.Context) (feeds.Result, error) {
	a.setFetching(true)
	defer a.setFetching(false)

	cfg, err := a.Settings(ctx)
	if err != nil {
		return feeds.Result{}, err
	}
	now := time.Now()
	res, err := feeds.FetchAll(ctx, a.Store, feeds.Options{
		ArticlesPerFeed:   cfg.ArticlesPerFeed,
		MaxFailures:       cfg.MaxFailures,
		FailureRetryHours: cfg.FailureRetryHours,
	}, now)
	st := FetchStatus{
		At: now, Feeds: res.Feeds, Failed: res.Failed, Skipped: len(res.Skipped),
		Added: res.Added, Duration: res.Duration,
	}
	if err != nil {
		st.Err = err.Error()
	}
	a.stateMu.Lock()
	a.last = st
	a.stateMu.Unlock()
	slog.Info("フィード取得完了", "feeds", res.Feeds, "failed", res.Failed,
		"skipped", len(res.Skipped), "added", res.Added, "took", res.Duration.Round(time.Millisecond))

	if n, err := a.Store.PruneArticles(ctx, now.AddDate(0, 0, -cfg.RetentionDays)); err == nil && n > 0 {
		slog.Info("保持期間切れの記事を削除", "count", n)
	}
	return res, err
}

func (a *App) setFetching(v bool) {
	a.stateMu.Lock()
	a.fetching = v
	a.stateMu.Unlock()
}

type Status struct {
	Fetching  bool
	Digesting bool
	LastFetch FetchStatus
}

func (a *App) Status() Status {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return Status{Fetching: a.fetching, Digesting: a.Digest.Running(), LastFetch: a.last}
}

// StartDigest は手動の要約実行をバックグラウンドで開始する。実行中なら digest.ErrBusy。
func (a *App) StartDigest(kind string) error {
	return a.Digest.Start(digest.Options{Kind: kind, Trigger: digest.TriggerManual})
}

// RunScheduler は定期取得と定時の要約実行を行う。ctxがキャンセルされるまで戻らない。
func (a *App) RunScheduler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		a.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) tick(ctx context.Context) {
	cfg, err := a.Settings(ctx)
	if err != nil {
		slog.Error("設定の読み込みに失敗", "err", err)
		return
	}
	now := time.Now()

	// 定時の要約実行。サーバ停止などで時刻を過ぎていた場合は、その日の直近の枠だけを1回実行する
	if slot, date := DueSlot(cfg.DigestTimes, now.In(cfg.Location())); slot != "" && !a.Digest.Running() {
		done, err := a.Store.SlotDone(ctx, date, slot)
		if err == nil && !done && a.readyForDigest(ctx, cfg) {
			slog.Info("定時の要約実行を開始", "slot", slot)
			if err := a.Digest.Start(digest.Options{
				Kind: digest.KindAppend, Trigger: digest.TriggerSchedule, Slot: slot,
			}); err == nil {
				return // 要約実行は内部でフィード取得も行う
			}
		}
	}

	if cfg.FetchInterval > 0 {
		last := a.Status().LastFetch.At
		if last.IsZero() || now.Sub(last) >= time.Duration(cfg.FetchInterval)*time.Minute {
			_ = a.StartFetch() // 取得中ならスキップ
		}
	}
}

// readyForDigest は定時実行してよい状態か（フィードがあり、AI連携が設定済み）。
// 初回起動直後など未設定のうちは、失敗する実行を記録せずに待つ。設定が済めば次の確認で実行される。
func (a *App) readyForDigest(ctx context.Context, cfg config.Settings) bool {
	list, err := a.Store.Feeds(ctx)
	if err != nil || len(list) == 0 {
		return false
	}
	if _, err := NewBackend(cfg); err != nil {
		a.stateMu.Lock()
		warned := a.notReadyWarned
		a.notReadyWarned = true
		a.stateMu.Unlock()
		if !warned {
			slog.Warn("AI連携が未設定のため定時の要約実行を保留中", "err", err)
		}
		return false
	}
	a.stateMu.Lock()
	a.notReadyWarned = false
	a.stateMu.Unlock()
	return true
}

// DueSlot は現在時刻（設定のタイムゾーン）までに来ている当日の要約枠のうち最も遅いものを返す。
func DueSlot(times []string, now time.Time) (slot, date string) {
	date = now.Format("2006-01-02")
	hm := now.Format("15:04")
	for _, t := range times { // timesは昇順
		if t <= hm {
			slot = t
		}
	}
	return slot, date
}
