// Package ai はClaude呼び出しを抽象化する。経路はAnthropic API（デフォルト）と claude -p の2つ。
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Request は1回のClaude呼び出し。
type Request struct {
	Stage  string // select | summarize | summarize_fallback | overview（使用量集計用）
	Prompt string
	// JSONSchema が非nilなら構造化出力を要求する（APIは output_config.format、
	// CLIはプロンプト中の出力形式指示に頼り、応答を寛容にパースする）
	JSONSchema map[string]any
	// AllowWebFetch が true のときだけClaudeにWebページ取得を許可する
	// （信頼できない本文を含むプロンプトでは常にfalse）
	AllowWebFetch bool
	Timeout       time.Duration
}

type Usage struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
	Unknown       int   `json:"unknown,omitempty"` // usage不明の呼び出し回数
}

type Response struct {
	Text  string
	Usage *Usage
}

type Backend interface {
	Name() string
	Complete(ctx context.Context, req Request) (Response, error)
}

// ---- 使用量集計 ----

type StageUsage struct {
	Calls int `json:"calls"`
	Usage
}

// Tracker は段階別の呼び出し回数・トークン数の集計。並列呼び出しから使える。
type Tracker struct {
	mu     sync.Mutex
	Stages map[string]*StageUsage `json:"stages"`
}

func NewTracker() *Tracker { return &Tracker{Stages: map[string]*StageUsage{}} }

func (t *Tracker) Record(stage string, u *Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.Stages[stage]
	if !ok {
		s = &StageUsage{}
		t.Stages[stage] = s
	}
	s.Calls++
	if u == nil {
		s.Unknown++
		return
	}
	s.Input += u.Input
	s.Output += u.Output
	s.CacheRead += u.CacheRead
	s.CacheCreation += u.CacheCreation
}

func (t *Tracker) Total() StageUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	var tot StageUsage
	for _, s := range t.Stages {
		tot.Calls += s.Calls
		tot.Input += s.Input
		tot.Output += s.Output
		tot.CacheRead += s.CacheRead
		tot.CacheCreation += s.CacheCreation
		tot.Unknown += s.Unknown
	}
	return tot
}

func (t *Tracker) JSON() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, _ := json.Marshal(t.Stages)
	return string(b)
}

var stageNames = []struct{ key, label string }{
	{"select", "選定"}, {"summarize", "要約"}, {"summarize_fallback", "救済要約"}, {"overview", "まとめ"},
}

// Line は1行の使用量サマリ（ログ・Discord・Web表示用）。
func (t *Tracker) Line() string {
	t.mu.Lock()
	var parts []string
	for _, s := range stageNames {
		if st, ok := t.Stages[s.key]; ok {
			parts = append(parts, fmt.Sprintf("%s%d回", s.label, st.Calls))
		}
	}
	t.mu.Unlock()
	tot := t.Total()
	if len(parts) == 0 {
		return "AI呼び出しなし"
	}
	return fmt.Sprintf("AI使用量: %s / in %s tok (cache %s) / out %s tok",
		strings.Join(parts, "・"), comma(tot.Input), comma(tot.CacheRead), comma(tot.Output))
}

// ParseUsageLine は runs.usage に保存したJSONから1行サマリを復元する。
func ParseUsageLine(s string) string {
	if s == "" {
		return ""
	}
	t := NewTracker()
	if err := json.Unmarshal([]byte(s), &t.Stages); err != nil {
		return ""
	}
	return t.Line()
}

func comma(n int64) string {
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ---- 応答パース ----

// StripCodeFence は応答にコードブロック記号（```）が付いていたら剥がす。
func StripCodeFence(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	lines := strings.Split(t, "\n")[1:]
	if n := len(lines); n > 0 && strings.TrimSpace(lines[n-1]) == "```" {
		lines = lines[:n-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// LenientJSON はJSONをパースする。失敗したら最初の { / [ から対応する最後の } / ] までを切り出して再試行。
func LenientJSON(text string, v any) error {
	t := StripCodeFence(text)
	if err := json.Unmarshal([]byte(t), v); err == nil {
		return nil
	}
	for _, pair := range [][2]string{{"{", "}"}, {"[", "]"}} {
		start, end := strings.Index(t, pair[0]), strings.LastIndex(t, pair[1])
		if start != -1 && end > start {
			if err := json.Unmarshal([]byte(t[start:end+1]), v); err == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("JSONとして解釈できません: %.200q", t)
}
