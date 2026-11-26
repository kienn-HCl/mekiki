package eval

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/kienn-HCl/mekiki/internal/ai"
	"github.com/kienn-HCl/mekiki/internal/digest"
	"github.com/kienn-HCl/mekiki/internal/store"
)

// pickStats は1つの選び方（本番 or やり直し）の成績。
type pickStats struct {
	Picked int // 選んだ件数
	Hits   int // そのうち開いた・保存した記事
	Found  int // 候補中の「役に立った」記事のうち選べた件数（再現率の分子）
}

// SelectReport は選定のやり直しの集計。
type SelectReport struct {
	Runs      int
	Positives int // 候補中の開いた・保存した記事（再現率の分母）
	Prod, Var pickStats
	Overlap   int // 本番とやり直しで共通して選ばれた件数
	Union     int
}

// Select は保存済みの候補で選定をやり直し、本番の選定と比べる。選定基準・興味は今のプロファイル設定を使う。
func (r *Runner) Select(ctx context.Context, opt Options) (*SelectReport, error) {
	if err := r.setup(opt); err != nil {
		return nil, err
	}
	runs, err := r.Store.EvalRuns(ctx, opt.Profile.ID, opt.Since)
	if err != nil {
		return nil, err
	}
	if opt.Limit > 0 && len(runs) > opt.Limit {
		runs = runs[len(runs)-opt.Limit:]
	}
	signals, err := r.Store.Signals(ctx, opt.Profile.ID)
	if err != nil {
		return nil, err
	}
	pick := opt.Pick
	if pick <= 0 {
		pick = opt.Profile.PickCount
	}

	type job struct {
		run    store.Run
		cands  []store.Candidate
		arts   []store.Article
		prompt string
	}
	var jobs []job
	var prompts []string
	for _, run := range runs {
		cands, err := r.Store.RunCandidates(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		var already []store.PageItem
		if run.Kind != digest.KindRebuild {
			if already, err = r.Store.ItemsBeforeRun(ctx, opt.Profile.ID, run.Date, run.ID); err != nil {
				return nil, err
			}
		}
		arts := make([]store.Article, len(cands))
		for i, c := range cands {
			arts[i] = store.Article{Title: c.Title, Link: c.Link, NormLink: c.NormLink, Category: c.Category,
				FeedName: c.FeedName, PublishedAt: c.PublishedAt}
		}
		p := digest.SelectPrompt(arts, already, pick, opt.Profile.SelectCriteria, opt.Profile.UserMemory)
		jobs = append(jobs, job{run, cands, arts, p})
		prompts = append(prompts, p)
	}
	fmt.Fprintf(opt.Out, "選定のやり直し: プロファイル %s、候補の記録が残る実行 %d回（%s 以降）\n",
		opt.Profile.Slug, len(jobs), opt.Since.Format("2006-01-02"))
	if len(jobs) == 0 {
		return &SelectReport{}, nil
	}
	if !opt.Run {
		printPlan(opt.Out, opt.Settings, len(jobs), prompts, 200)
		return nil, nil
	}

	var prod ai.StageTotal
	for _, j := range jobs {
		prod.AddStage(j.run.Usage, "select")
	}
	rep := &SelectReport{}
	tw := tabwriter.NewWriter(opt.Out, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "実行\t日付\t候補\t役立\t本番 選定/的中\tやり直し 選定/的中\t共通\t")
	for _, j := range jobs {
		resp, err := r.complete(ctx, r.backend, opt.Settings.Effort(opt.Settings.Backend), ai.Request{
			Stage: "select", Prompt: j.prompt, JSONSchema: digest.SelectSchema, Timeout: digest.SelectTimeout,
		})
		if err != nil {
			return rep, fmt.Errorf("実行 %d の選定: %w", j.run.ID, err)
		}
		idx, err := digest.ParseIndexes(resp.Text, len(j.arts))
		if err != nil {
			return rep, fmt.Errorf("実行 %d の選定応答: %w", j.run.ID, err)
		}
		varPicked := map[int]bool{}
		for _, i := range digest.PickIndexes(j.arts, idx, pick) {
			varPicked[i] = true
		}
		var prod, vari pickStats
		pos, overlap, union := 0, 0, 0
		for i, c := range j.cands {
			good := signals[c.NormLink].Positive()
			p, v := c.PickedRank >= 0, varPicked[i]
			if good {
				pos++
			}
			count := func(s *pickStats, picked bool) {
				if picked {
					s.Picked++
					if good {
						s.Hits++
						s.Found++
					}
				}
			}
			count(&prod, p)
			count(&vari, v)
			if p && v {
				overlap++
			}
			if p || v {
				union++
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%d / %d\t%d / %d\t%d\t\n", j.run.ID, j.run.Date, len(j.cands), pos,
			prod.Picked, prod.Hits, vari.Picked, vari.Hits, overlap)
		rep.Runs++
		rep.Positives += pos
		rep.Overlap += overlap
		rep.Union += union
		add(&rep.Prod, prod)
		add(&rep.Var, vari)
	}
	tw.Flush()

	fmt.Fprintf(opt.Out, "\n         選定  的中  適合率  再現率\n")
	for _, row := range []struct {
		name string
		s    pickStats
	}{{"本番    ", rep.Prod}, {"やり直し", rep.Var}} {
		fmt.Fprintf(opt.Out, "%s %5d %5d   %s   %s\n", row.name, row.s.Picked, row.s.Hits,
			pct(row.s.Hits, row.s.Picked), pct(row.s.Found, rep.Positives))
	}
	fmt.Fprintf(opt.Out, "本番との一致（Jaccard）: %s（%d / %d）\n", pct(rep.Overlap, rep.Union), rep.Overlap, rep.Union)
	fmt.Fprintln(opt.Out, "的中＝開いた・保存した記事。本番で選ばれた記事は画面に出ているぶん開かれやすいので、適合率は本番に有利に出る。")
	r.compareStage(opt.Out, "select", "選定", prod)
	r.printUsage(opt.Out)
	return rep, nil
}

func add(dst *pickStats, s pickStats) {
	dst.Picked += s.Picked
	dst.Hits += s.Hits
	dst.Found += s.Found
}
