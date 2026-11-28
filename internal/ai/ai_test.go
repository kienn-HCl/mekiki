package ai

import (
	"strings"
	"testing"
)

func TestStripCodeFence(t *testing.T) {
	if got := StripCodeFence("```json\n[1, 2]\n```"); got != "[1, 2]" {
		t.Errorf("got %q", got)
	}
	if got := StripCodeFence("  [1]  "); got != "[1]" {
		t.Errorf("got %q", got)
	}
}

func TestLenientJSON(t *testing.T) {
	var obj struct{ Indexes []int }
	if err := LenientJSON("選びました。\n{\"indexes\": [1, 2]}\n以上です", &obj); err != nil || len(obj.Indexes) != 2 {
		t.Errorf("前置き付きJSON: %v %v", err, obj)
	}
	var arr []int
	if err := LenientJSON("結果: [3, 4]", &arr); err != nil || len(arr) != 2 {
		t.Errorf("前置き付き配列: %v %v", err, arr)
	}
	// JSONの後に括弧を含む独り言が続く（実際に claude -p で起きた応答）
	obj.Indexes = nil
	text := "{\"indexes\": [396, 183]}\n\nWait — I need to verify indexes. 409 is wrong (see [3]). {maybe}"
	if err := LenientJSON(text, &obj); err != nil || len(obj.Indexes) != 2 || obj.Indexes[0] != 396 {
		t.Errorf("後書き付きJSON: %v %v", err, obj)
	}
	if err := LenientJSON("JSONなし", &arr); err == nil {
		t.Error("JSONなしがエラーにならない")
	}
}

func TestTracker(t *testing.T) {
	tr := NewTracker()
	tr.Record("select", &Usage{Input: 1000, Output: 50, CacheRead: 200})
	tr.Record("summarize", &Usage{Input: 2500, Output: 100})
	tr.Record("summarize", nil)
	tot := tr.Total()
	if tot.Calls != 3 || tot.Input != 3500 || tot.Unknown != 1 {
		t.Errorf("total = %+v", tot)
	}
	line := tr.Line()
	for _, want := range []string{"選定1回", "要約2回", "in 3,500 tok", "cache 200"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q に %q がない", line, want)
		}
	}
	if got := ParseUsageLine(tr.JSON()); got != line {
		t.Errorf("JSONからの復元: %q != %q", got, line)
	}
}

func TestRunCost(t *testing.T) {
	tr := NewTracker()
	tr.Record("select", &Usage{Input: 1_000_000, Output: 100_000, Model: "claude-opus-5-5"})
	tr.Record("summarize", &Usage{Input: 2_000_000, CacheRead: 1_000_000, Model: "anthropic/claude-haiku-5-5"})
	if c, ok := RunCost(tr.JSON()); !ok || c < 6.209 || c > 6.211 {
		t.Errorf("費用: %v %v", c, ok)
	}
	tr.Record("overview", &Usage{Input: 10, Model: "gpt-x"})
	if _, ok := RunCost(tr.JSON()); ok {
		t.Error("単価不明のモデルを含むのに確定扱い")
	}
}
