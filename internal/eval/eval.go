// Package eval は保存済みの記録（選定に渡した候補・要約に渡した本文）を使って、
// 設定を変えた選定・要約をやり直し、本番の結果と比べる再生ツール。
//
// 記事のページやフィードには一切アクセスしない。AIへの問い合わせは1件ずつ順に送り、
// 応答は eval_cache に保存して、同じ問い合わせは二度送らない。
package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kienn-HCl/mekiki/internal/ai"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/store"
)

type Options struct {
	Profile  config.Profile
	Settings config.Settings // 上書き済みの設定（経路・モデル・effort）
	Since    time.Time
	Limit    int    // select: 実行の数 / summarize: 記事の数（0 なら無制限）
	Pick     int    // select: 選ぶ件数（0 ならプロファイルの値）
	MaxChars int    // summarize: 要約に渡す本文の上限（0 なら保存されている全文）
	Judge    string // summarize: 忠実性を採点するモデル（空なら採点しない）
	Run      bool   // false なら問い合わせ内容と量を表示するだけで、AIには送らない
	MaxCalls int    // 1回の実行で送ってよい問い合わせの上限（0なら無制限。キャッシュ済みの分も数える）
	Out      io.Writer
}

type Runner struct {
	Store      *store.Store
	NewBackend func(config.Settings) (ai.Backend, error)

	backend ai.Backend
	tracker *ai.Tracker
	cached  int
}

// Override は経路・モデル・effort を上書きした設定を返す（空なら上書きしない。effort の "none" は送らない）。
func Override(s config.Settings, backend, model, effort string) config.Settings {
	clone := func(m map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	s.Models, s.Efforts = clone(s.Models), clone(s.Efforts)
	if backend != "" {
		s.Backend = backend
	}
	if model != "" {
		s = withModel(s, model)
	}
	switch effort {
	case "":
	case "none":
		s.Efforts[s.Backend] = ""
	default:
		s.Efforts[s.Backend] = effort
	}
	return s
}

func checkCalls(opt Options, calls int) error {
	if opt.Run && opt.MaxCalls > 0 && calls > opt.MaxCalls {
		return fmt.Errorf("問い合わせが上限を超えます（%d回 > %d回）。期間や件数を絞ってください", calls, opt.MaxCalls)
	}
	return nil
}

func (r *Runner) setup(opt Options) error {
	r.tracker = ai.NewTracker()
	if !opt.Run {
		return nil
	}
	b, err := r.NewBackend(opt.Settings)
	r.backend = b
	return err
}

// complete はキャッシュを見てからAIに問い合わせる。キャッシュの鍵は経路・モデル・effort・プロンプト。
func (r *Runner) complete(ctx context.Context, b ai.Backend, effort string, req ai.Request) (ai.Response, error) {
	h := sha256.Sum256([]byte(req.Stage + "\x00" + b.Name() + "\x00" + effort + "\x00" + req.Prompt))
	key := hex.EncodeToString(h[:])
	if text, usage, ok := r.Store.EvalCache(ctx, key); ok {
		r.cached++
		var u ai.Usage
		_ = json.Unmarshal([]byte(usage), &u)
		r.tracker.Record(req.Stage, &u)
		return ai.Response{Text: text, Usage: &u}, nil
	}
	resp, err := b.Complete(ctx, req)
	r.tracker.Record(req.Stage, resp.Usage)
	if err != nil {
		return resp, err
	}
	usage := []byte("{}")
	if resp.Usage != nil {
		usage, _ = json.Marshal(resp.Usage)
	}
	if err := r.Store.PutEvalCache(ctx, key, resp.Text, string(usage), time.Now()); err != nil {
		return resp, err
	}
	return resp, nil
}

// estimateTokens はプロンプトの入力トークン数の大まかな上限（日本語は1文字1トークン前後なので文字数で数える）。
func estimateTokens(prompts []string) int {
	n := 0
	for _, p := range prompts {
		n += len([]rune(p))
	}
	return n
}

func printPlan(w io.Writer, s config.Settings, calls int, prompts []string, outPerCall int) {
	in := estimateTokens(prompts)
	model := modelOf(s)
	fmt.Fprintf(w, "\n問い合わせ: %d回（経路 %s / モデル %s / effort %s）\n", calls, s.Backend, orDash(model),
		orDash(s.Effort(s.Backend)))
	fmt.Fprintf(w, "入力の目安: 最大およそ %s トークン（キャッシュ済みの分は送らない）\n", comma(in))
	if p, ok := ai.PriceOf(model); ok {
		cost := p.Cost(ai.Usage{Input: int64(in), Output: int64(calls * outPerCall)})
		fmt.Fprintf(w, "費用の目安: 最大およそ $%.3f（出力は1回 %d トークンと仮定。思考の分で増えることがある）\n", cost, outPerCall)
	}
	fmt.Fprintln(w, "実際に問い合わせるには -run を付けてください。")
}

func (r *Runner) printUsage(w io.Writer) {
	fmt.Fprintf(w, "\n%s（うちキャッシュ %d回）\n", r.tracker.Line(), r.cached)
	if cost, ok := ai.RunCost(r.tracker.JSON()); ok {
		fmt.Fprintf(w, "費用（推定、キャッシュ分を含む）: $%.3f\n", cost)
	}
}

// compareStage は本番（prod）とやり直し（このツールの集計）の1回あたりの入力・費用を並べて表示する。
func (r *Runner) compareStage(w io.Writer, stage, label string, prod ai.StageTotal) {
	var v ai.StageTotal
	v.AddStage(r.tracker.JSON(), stage)
	line := func(name string, t ai.StageTotal) string {
		if t.Calls == 0 {
			return name + ": —"
		}
		s := fmt.Sprintf("%s: 入力 %s トークン", name, comma(int(t.Input)/t.Calls))
		if t.CostKnown {
			s += fmt.Sprintf(" / $%.4f", t.Cost/float64(t.Calls))
		}
		return s
	}
	fmt.Fprintf(w, "%s1回あたり  %s  |  %s\n", label, line("本番", prod), line("やり直し", v))
}

func modelOf(s config.Settings) string {
	if s.Backend == config.BackendCLI {
		return s.CLIModel
	}
	return s.Model(s.Backend)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func comma(n int) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func pct(n, d int) string {
	if d == 0 {
		return "   —"
	}
	return fmt.Sprintf("%3.0f%%", float64(n)*100/float64(d))
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	return string(r[:n])
}
