package eval

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kienn-HCl/mekiki/internal/ai"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/store"
)

type fakeBackend struct{ calls *atomic.Int32 }

func (f fakeBackend) Name() string { return "fake:m" }

func (f fakeBackend) Complete(_ context.Context, req ai.Request) (ai.Response, error) {
	f.calls.Add(1)
	u := &ai.Usage{Input: int64(len(req.Prompt)), Output: 10, Model: "claude-haiku-5-5"}
	switch req.Stage {
	case "select":
		return ai.Response{Text: `{"indexes": [1, 2]}`, Usage: u}, nil
	case "judge":
		return ai.Response{Text: `{"score": 4, "issue": "やや省略"}`, Usage: u}, nil
	}
	return ai.Response{Text: "やり直しの要約", Usage: u}, nil
}

func setup(t *testing.T) (*store.Store, config.Profile) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	st.EnsureProfile(ctx)
	p, _ := st.ProfileBySlug(ctx, "main")
	now := time.Now()
	runID, _ := st.StartRun(ctx, store.Run{ProfileID: p.ID, Date: "2026-11-20", Kind: "append", Trigger: "manual", StartedAt: now})
	st.FinishRun(ctx, store.Run{ID: runID, Status: "ok", FinishedAt: now,
		Usage: `{"summarize":{"calls":1,"input":1000,"output":100,"model":"claude-opus-5-5"},"select":{"calls":1,"input":500,"output":20,"model":"claude-opus-5-5"}}`})
	var cands []store.Article
	for _, s := range []string{"a", "b", "c"} {
		cands = append(cands, store.Article{Title: "記事" + s, Link: "https://ex.com/" + s, NormLink: "https://ex.com/" + s})
	}
	// 本番は a を選び、利用者は a を開いて b を保存した
	st.SaveRunCandidates(ctx, runID, p.ID, cands, []int{0})
	st.AppendItems(ctx, p.ID, "2026-11-20", []store.PageItem{{RunID: runID, Title: "記事a", Link: "https://ex.com/a",
		NormLink: "https://ex.com/a", Summary: "本番の要約", SummaryOK: true, Stage: "feed", Input: strings.Repeat("本文", 200)}}, now, false)
	st.RecordOpen(ctx, p.ID, "https://ex.com/a", store.OpenFromPage, now)
	st.ToggleSaved(ctx, store.Saved{ProfileID: p.ID, Kind: store.SavedLater, NormLink: "https://ex.com/b", Title: "記事b",
		Link: "https://ex.com/b"}, now)
	return st, p
}

func runner(st *store.Store, calls *atomic.Int32) *Runner {
	return &Runner{Store: st, NewBackend: func(config.Settings) (ai.Backend, error) { return fakeBackend{calls}, nil }}
}

func TestSelect(t *testing.T) {
	st, p := setup(t)
	var calls atomic.Int32
	opt := Options{Profile: p, Settings: config.Defaults(), Since: time.Now().Add(-time.Hour), Out: &bytes.Buffer{}}

	if rep, err := runner(st, &calls).Select(context.Background(), opt); err != nil || rep != nil || calls.Load() != 0 {
		t.Fatalf("-run なしでAIに問い合わせた: calls=%d err=%v", calls.Load(), err)
	}
	opt.Run = true
	rep, err := runner(st, &calls).Select(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	// 役に立った記事は a・b。本番は a だけ選び、やり直しは b・c を選んだ
	if rep.Positives != 2 || rep.Prod.Hits != 1 || rep.Var.Picked != 2 || rep.Var.Hits != 1 || rep.Overlap != 0 || rep.Union != 3 {
		t.Errorf("集計: %+v", rep)
	}
	if _, err := runner(st, &calls).Select(context.Background(), opt); err != nil || calls.Load() != 1 {
		t.Errorf("同じ問い合わせを二度送った: calls=%d err=%v", calls.Load(), err)
	}
}

func TestSummarize(t *testing.T) {
	st, p := setup(t)
	var calls atomic.Int32
	out := &bytes.Buffer{}
	opt := Options{Profile: p, Settings: config.Defaults(), Since: time.Now().Add(-time.Hour), MaxChars: 50,
		Judge: "claude-sonnet-5-5", Run: true, Out: out}
	var report bytes.Buffer
	rep, err := runner(st, &calls).Summarize(context.Background(), opt, &report)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 1 || rep.VarScore != 4 || calls.Load() != 3 {
		t.Errorf("集計: %+v calls=%d", rep, calls.Load())
	}
	for _, want := range []string{"本番の要約", "やり直しの要約", "渡したのは 50 字", "採点: 本番 4（やや省略）"} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("レポートに %q がない:\n%s", want, report.String())
		}
	}
	if !strings.Contains(out.String(), "要約1回あたり  本番: 入力 1,000 トークン / $0.0060") {
		t.Errorf("費用の比較:\n%s", out.String())
	}
}
