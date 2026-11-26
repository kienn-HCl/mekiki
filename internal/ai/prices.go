package ai

import (
	"encoding/json"
	"strings"
)

// Price は100万トークンあたりの米ドル単価（Anthropic API の定価、2026-11時点）。
// キャッシュ書き込み（5分）は入力の1.25倍として計算する。
type Price struct{ Input, Output, CacheRead float64 }

var prices = map[string]Price{
	"claude-fable-5-2":  {8, 40, 0.20},
	"claude-fable-5-1":  {10, 50, 1.00},
	"claude-fable-5":    {10, 50, 1.00},
	"claude-opus-5-5":   {4, 20, 0.20},
	"claude-opus-5":     {5, 25, 0.50},
	"claude-opus-4-8":   {5, 25, 0.50},
	"claude-opus-4-7":   {5, 25, 0.50},
	"claude-opus-4-6":   {5, 25, 0.50},
	"claude-sonnet-5-5": {2, 10, 0.10},
	"claude-sonnet-5":   {2, 10, 0.20},
	"claude-sonnet-4-6": {3, 15, 0.30},
	"claude-haiku-5-5":  {0.10, 0.50, 0.01},
	"claude-haiku-4-5":  {1, 5, 0.10},
}

// PriceOf はモデルの単価を返す。OpenRouter 形式の "anthropic/..." も受け付ける。
func PriceOf(model string) (Price, bool) {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	p, ok := prices[model]
	return p, ok
}

// Cost は使用量の推定金額（米ドル）。
func (p Price) Cost(u Usage) float64 {
	return (float64(u.Input)*p.Input + float64(u.Output)*p.Output +
		float64(u.CacheRead)*p.CacheRead + float64(u.CacheCreation)*p.Input*1.25) / 1e6
}

// StageTotal は段階ごとの使用量の合計（複数の実行をまたいで足し込む）。
type StageTotal struct {
	Calls     int
	Input     int64
	Cost      float64
	CostKnown bool
}

// AddStage は runs.usage のJSONから stage の使用量を t に足す。
func (t *StageTotal) AddStage(usageJSON, stage string) {
	var stages map[string]*StageUsage
	if json.Unmarshal([]byte(usageJSON), &stages) != nil {
		return
	}
	s, ok := stages[stage]
	if !ok {
		return
	}
	if t.Calls == 0 {
		t.CostKnown = true
	}
	t.Calls += s.Calls
	t.Input += s.Input + s.CacheRead + s.CacheCreation
	if p, ok := PriceOf(s.Model); ok && s.Unknown == 0 {
		t.Cost += p.Cost(s.Usage)
	} else {
		t.CostKnown = false
	}
}

// RunCost は runs.usage に保存したJSONから推定金額を出す。単価の分からない段階があれば ok=false。
func RunCost(usageJSON string) (usd float64, ok bool) {
	if usageJSON == "" {
		return 0, false
	}
	var stages map[string]*StageUsage
	if err := json.Unmarshal([]byte(usageJSON), &stages); err != nil || len(stages) == 0 {
		return 0, false
	}
	ok = true
	for _, s := range stages {
		p, known := PriceOf(s.Model)
		if !known || s.Unknown > 0 {
			ok = false
			continue
		}
		usd += p.Cost(s.Usage)
	}
	return usd, ok
}
