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

// fakeChat はChat Completions APIの応答（ステータス, ボディ）を順に返す。
type fakeChat struct {
	mu      sync.Mutex
	replies [][2]string
	bodies  []map[string]any
	auth    []string
	paths   []string
}

func (f *fakeChat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	f.bodies = append(f.bodies, body)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.paths = append(f.paths, r.URL.Path)
	reply := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	code := map[string]int{"200": 200, "400": 400, "429": 429, "500": 500}[reply[0]]
	w.WriteHeader(code)
	io.WriteString(w, reply[1])
}

func chatOK(text string) [2]string {
	return [2]string{"200", `{"choices":[{"message":{"role":"assistant","content":` + jsonString(text) +
		`},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":20}}}`}
}

func newFakeChat(t *testing.T, key string, replies ...[2]string) (*OpenAICompat, *fakeChat) {
	t.Helper()
	f := &fakeChat{replies: replies}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := NewOpenAICompat("openai", srv.URL+"/v1/", key, "gpt-test", "xhigh", key == "")
	if err != nil {
		t.Fatal(err)
	}
	c.retries = 2
	return c, f
}

func TestOpenAICompatStructured(t *testing.T) {
	c, f := newFakeChat(t, "sk-test", chatOK(`{"indexes":[2]}`))
	resp, err := c.Complete(context.Background(), Request{
		Prompt: "選んで", JSONSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"indexes":[2]}` || resp.Usage.Input != 50 || resp.Usage.Output != 7 || resp.Usage.CacheRead != 20 {
		t.Errorf("resp = %+v usage=%+v", resp, resp.Usage)
	}
	if f.paths[0] != "/v1/chat/completions" || f.auth[0] != "Bearer sk-test" {
		t.Errorf("path=%q auth=%q", f.paths[0], f.auth[0])
	}
	body := f.bodies[0]
	rf, _ := body["response_format"].(map[string]any)
	if body["model"] != "gpt-test" || rf["type"] != "json_schema" || body["reasoning_effort"] != "high" {
		t.Errorf("body = %v", body)
	}
}

func TestOpenAICompatRetriesWithoutOptionalParamsOn400(t *testing.T) {
	c, f := newFakeChat(t, "sk-test",
		[2]string{"400", `{"error":{"message":"response_format is not supported"}}`},
		chatOK("[1, 0]"))
	resp, err := c.Complete(context.Background(), Request{Prompt: "p", JSONSchema: map[string]any{"type": "object"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "[1, 0]" || len(f.bodies) != 2 {
		t.Fatalf("text=%q requests=%d", resp.Text, len(f.bodies))
	}
	if _, ok := f.bodies[1]["response_format"]; ok {
		t.Error("再送で response_format が外れていない")
	}
	if _, ok := f.bodies[1]["reasoning_effort"]; ok {
		t.Error("再送で reasoning_effort が外れていない")
	}
}

func TestOpenAICompatRetriesTransientErrors(t *testing.T) {
	c, f := newFakeChat(t, "",
		[2]string{"429", `{"error":{"message":"rate limited"}}`},
		[2]string{"500", `oops`},
		chatOK("要約"))
	resp, err := c.Complete(context.Background(), Request{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "要約" || len(f.bodies) != 3 {
		t.Errorf("text=%q requests=%d", resp.Text, len(f.bodies))
	}
	if f.auth[0] != "" {
		t.Errorf("キーなしなのにAuthorizationが付いている: %q", f.auth[0])
	}
}

func TestOpenAICompatErrors(t *testing.T) {
	c, _ := newFakeChat(t, "sk-test", [2]string{"400", `{"error":{"message":"invalid model"}}`})
	_, err := c.Complete(context.Background(), Request{Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "invalid model") {
		t.Errorf("エラーメッセージが伝わらない: %v", err)
	}
	c, _ = newFakeChat(t, "sk-test", [2]string{"200",
		`{"choices":[{"message":{"content":"途中","refusal":null},"finish_reason":"length"}]}`})
	if _, err := c.Complete(context.Background(), Request{Prompt: "p"}); err == nil {
		t.Error("打ち切りがエラーにならない")
	}
	if _, err := NewOpenAICompat("openai", "https://x", "", "m", "", false); err == nil {
		t.Error("キー必須なのにキーなしで作れる")
	}
	if _, err := NewOpenAICompat("openai", "https://x", "k", "", "", false); err == nil {
		t.Error("モデルなしで作れる")
	}
}
