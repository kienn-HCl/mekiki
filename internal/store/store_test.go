package store

import (
	"context"
	"testing"
	"time"

	"github.com/kienn-HCl/mekiki/internal/config"
)

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
