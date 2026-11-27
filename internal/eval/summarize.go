package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kienn-HCl/mekiki/internal/ai"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/digest"
)

const judgeTemplate = `以下は記事の本文テキストと、その要約です。要約が本文に忠実かを採点してください。

- 5: 本文の要点を正しく押さえ、本文にない事実を含まない
- 4: おおむね正しいが、重要な点の抜けや軽微な不正確さがある
- 3: 要点の一部を誤る、または本文から読み取れない推測を含む
- 2: 重要な誤りがある
- 1: 本文と食い違う、または無関係

# 本文テキスト
{text}

# 要約
{summary}

# 出力形式（JSONのみ）
{"score": 1〜5の整数, "issue": "減点した理由（満点なら空文字）"}
`

var judgeSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"score": map[string]any{"type": "integer"},
		"issue": map[string]any{"type": "string"},
	},
	"required":             []string{"score", "issue"},
	"additionalProperties": false,
}

type judgement struct {
	Score int    `json:"score"`
	Issue string `json:"issue"`
}

// SummarizeReport は要約のやり直しの集計。
type SummarizeReport struct {
	Items               int
	ProdScore, VarScore float64 // 忠実性の平均点（採点したときだけ）
}

// Summarize は保存済みの本文で要約をやり直し、本番の要約と並べたレポートを report に書く。
func (r *Runner) Summarize(ctx context.Context, opt Options, report io.Writer) (*SummarizeReport, error) {
	if err := r.setup(opt); err != nil {
		return nil, err
	}
	limit := opt.Limit
	if limit <= 0 {
		limit = 1 << 30
	}
	items, err := r.Store.ItemInputs(ctx, opt.Profile.ID, opt.Since, limit)
	if err != nil {
		return nil, err
	}
	prompts := make([]string, len(items))
	inputs := make([]string, len(items))
	for i, it := range items {
		inputs[i] = truncateRunes(it.Text, opt.MaxChars)
		prompts[i] = digest.SummaryPrompt(it.Title, inputs[i], opt.Profile.UserMemory)
	}
	fmt.Fprintf(opt.Out, "要約のやり直し: プロファイル %s、本文の記録が残る記事 %d件（%s 以降）", opt.Profile.Slug,
		len(items), opt.Since.Format("2006-01-02"))
	if opt.MaxChars > 0 {
		fmt.Fprintf(opt.Out, "、本文は先頭 %d 字まで", opt.MaxChars)
	}
	fmt.Fprintln(opt.Out)
	if len(items) == 0 {
		return &SummarizeReport{}, nil
	}
	var judge ai.Backend
	if !opt.Run {
		printPlan(opt.Out, opt.Settings, len(items), prompts, 300)
		if opt.Judge != "" {
			fmt.Fprintf(opt.Out, "採点（%s）: さらに %d回（本番・やり直しの要約をそれぞれ採点）\n", opt.Judge, 2*len(items))
		}
		return nil, nil
	}
	calls := len(items)
	if opt.Judge != "" {
		calls *= 3
	}
	if err := checkCalls(opt, calls); err != nil {
		return nil, err
	}
	judgeEffort := ""
	if opt.Judge != "" {
		js := withModel(opt.Settings, opt.Judge)
		if judge, err = r.NewBackend(js); err != nil {
			return nil, fmt.Errorf("採点用の経路: %w", err)
		}
		judgeEffort = js.Effort(js.Backend)
	}

	rep := &SummarizeReport{}
	var prodSum, varSum, judged int
	fmt.Fprintf(report, "# 要約のやり直し（%s）\n\n経路 %s / モデル %s / effort %s",
		opt.Profile.Slug, opt.Settings.Backend, orDash(modelOf(opt.Settings)),
		orDash(opt.Settings.Effort(opt.Settings.Backend)))
	if opt.MaxChars > 0 {
		fmt.Fprintf(report, " / 本文の上限 %d 字", opt.MaxChars)
	}
	fmt.Fprint(report, "\n\n")
	for i, it := range items {
		resp, err := r.complete(ctx, r.backend, opt.Settings.Effort(opt.Settings.Backend), ai.Request{
			Stage: "summarize", Prompt: prompts[i], Timeout: digest.SummaryTimeout,
		})
		if err != nil {
			return rep, fmt.Errorf("記事 %d の要約: %w", it.ItemID, err)
		}
		rep.Items++
		fmt.Fprintf(report, "## %s\n\n%s · 本文 %d 字（渡したのは %d 字）\n\n", it.Title, it.Date,
			len([]rune(it.Text)), len([]rune(inputs[i])))
		fmt.Fprintf(report, "**本番**\n\n%s\n\n**やり直し**\n\n%s\n\n", it.Summary, resp.Text)
		if judge != nil {
			// 採点には、本番で要約に渡したのと同じ全文を使う
			pj, err1 := r.judge(ctx, judge, judgeEffort, it.Text, it.Summary)
			vj, err2 := r.judge(ctx, judge, judgeEffort, it.Text, resp.Text)
			if err1 != nil || err2 != nil {
				fmt.Fprintf(opt.Out, "記事 %d の採点に失敗: %v %v\n", it.ItemID, err1, err2)
			} else {
				judged++
				prodSum += pj.Score
				varSum += vj.Score
				fmt.Fprintf(report, "採点: 本番 %d%s / やり直し %d%s\n\n", pj.Score, issue(pj), vj.Score, issue(vj))
			}
		}
	}
	if judged > 0 {
		rep.ProdScore = float64(prodSum) / float64(judged)
		rep.VarScore = float64(varSum) / float64(judged)
		line := fmt.Sprintf("忠実性の平均（%s が5点満点で採点、%d件）: 本番 %.2f / やり直し %.2f",
			opt.Judge, judged, rep.ProdScore, rep.VarScore)
		fmt.Fprintln(opt.Out, line)
		fmt.Fprintf(report, "---\n\n%s\n", line)
	}
	// 本番の要約の量は、対象記事が載った実行の要約段階の平均で比べる
	var prod ai.StageTotal
	seen := map[int64]bool{}
	for _, it := range items {
		if seen[it.RunID] {
			continue
		}
		seen[it.RunID] = true
		if run, err := r.Store.Run(ctx, it.RunID); err == nil {
			prod.AddStage(run.Usage, "summarize")
		}
	}
	r.compareStage(opt.Out, "summarize", "要約", prod)
	r.printUsage(opt.Out)
	return rep, nil
}

func (r *Runner) judge(ctx context.Context, b ai.Backend, effort, text, summary string) (judgement, error) {
	prompt := strings.NewReplacer("{text}", text, "{summary}", summary).Replace(judgeTemplate)
	resp, err := r.complete(ctx, b, effort, ai.Request{
		Stage: "judge", Prompt: prompt, JSONSchema: judgeSchema, Timeout: digest.SummaryTimeout,
	})
	if err != nil {
		return judgement{}, err
	}
	var j judgement
	if err := ai.LenientJSON(resp.Text, &j); err != nil {
		return j, err
	}
	if j.Score < 1 || j.Score > 5 {
		return j, fmt.Errorf("採点が範囲外: %d", j.Score)
	}
	return j, nil
}

func issue(j judgement) string {
	if j.Issue == "" {
		return ""
	}
	b, _ := json.Marshal(j.Issue)
	return "（" + strings.Trim(string(b), `"`) + "）"
}

// withModel は今の経路のまま、モデルだけ差し替えた設定を返す。
func withModel(s config.Settings, model string) config.Settings {
	models := map[string]string{}
	for k, v := range s.Models {
		models[k] = v
	}
	s.Models = models
	if s.Backend == config.BackendCLI {
		s.CLIModel = model
	} else {
		s.Models[s.Backend] = model
	}
	return s
}
