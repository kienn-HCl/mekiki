package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAPI はMessages APIの応答を順に返し、受けたリクエストを記録する。
type fakeAPI struct {
	mu        sync.Mutex
	responses []string
	bodies    []map[string]any
	betas     []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	f.bodies = append(f.bodies, body)
	f.betas = append(f.betas, r.Header.Get("anthropic-beta"))
	resp := f.responses[0]
	if len(f.responses) > 1 {
		f.responses = f.responses[1:]
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, resp)
}

func message(stop, text string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
	"content":[{"type":"thinking","thinking":"","signature":"x"},{"type":"text","text":` + jsonString(text) + `}],
	"stop_reason":"` + stop + `","usage":{"input_tokens":120,"output_tokens":30,"cache_read_input_tokens":5}}`
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func newFakeAPI(t *testing.T, model string, responses ...string) (*API, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{responses: responses}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	a, err := NewAPI("sk-ant-test", model, "low")
	if err != nil {
		t.Fatal(err)
	}
	return a, f
}

func TestAPIStructuredRequest(t *testing.T) {
	a, f := newFakeAPI(t, "claude-opus-5-5", message("end_turn", `{"indexes":[1,0]}`))
	resp, err := a.Complete(context.Background(), Request{
		Stage: "select", Prompt: "選んで", JSONSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"indexes":[1,0]}` {
		t.Errorf("text = %q（thinkingブロックを除いたテキストのはず）", resp.Text)
	}
	if resp.Usage.Input != 120 || resp.Usage.Output != 30 || resp.Usage.CacheRead != 5 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	body := f.bodies[0]
	oc, _ := body["output_config"].(map[string]any)
	if oc["effort"] != "low" || oc["format"] == nil {
		t.Errorf("output_config = %v", body["output_config"])
	}
	if body["fallbacks"] != "default" || !strings.Contains(f.betas[0], "server-side-fallback-2026-07-01") {
		t.Errorf("fallbacks = %v, beta = %q", body["fallbacks"], f.betas[0])
	}
	if body["tools"] != nil {
		t.Errorf("本文を渡す呼び出しにツールが付いている: %v", body["tools"])
	}
	if _, ok := body["thinking"]; ok {
		t.Errorf("thinkingは指定しない（モデルのデフォルトに任せる）: %v", body["thinking"])
	}
}

func TestAPIWebFetchResumesPauseTurn(t *testing.T) {
	a, f := newFakeAPI(t, "claude-sonnet-5-5",
		message("pause_turn", ""), message("end_turn", "要約です"))
	resp, err := a.Complete(context.Background(), Request{Stage: "summarize_fallback", Prompt: "p", AllowWebFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "要約です" || len(f.bodies) != 2 {
		t.Fatalf("text=%q requests=%d", resp.Text, len(f.bodies))
	}
	tools, _ := f.bodies[0]["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_fetch_20260209" {
		t.Errorf("tools = %v", f.bodies[0]["tools"])
	}
	// 再開リクエストには直前のassistant応答がそのまま付く（"Continue"等のユーザー発話は足さない）
	msgs, _ := f.bodies[1]["messages"].([]any)
	if len(msgs) != 2 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Errorf("再開リクエストのmessages = %v", msgs)
	}
	if resp.Usage.Input != 240 {
		t.Errorf("usageが合算されていない: %+v", resp.Usage)
	}
}

func TestAPINoFallbackForUnsupportedModel(t *testing.T) {
	a, f := newFakeAPI(t, "claude-haiku-5-5", message("end_turn", "ok"))
	if _, err := a.Complete(context.Background(), Request{Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.bodies[0]["fallbacks"]; ok {
		t.Error("fallbacks非対応モデルに fallbacks が付いている")
	}
}

func TestAPIRefusal(t *testing.T) {
	a, _ := newFakeAPI(t, "claude-opus-5-5",
		`{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],
		"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber"},"usage":{"input_tokens":1,"output_tokens":0}}`)
	if _, err := a.Complete(context.Background(), Request{Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "拒否") {
		t.Errorf("refusalがエラーになっていない: %v", err)
	}
}

func TestNewAPIRequiresKey(t *testing.T) {
	if _, err := NewAPI("", "claude-opus-5-5", "low"); err == nil {
		t.Error("APIキーなしでエラーにならない")
	}
}
