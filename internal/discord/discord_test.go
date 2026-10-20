package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTruncate(t *testing.T) {
	if got := Truncate("あいうえお", 5); got != "あいうえお" {
		t.Errorf("got %q", got)
	}
	if got := Truncate("あいうえおか", 5); got != "あいうえ…" {
		t.Errorf("got %q", got)
	}
}

func TestNewEmbedOmitsEmptyImage(t *testing.T) {
	b, _ := json.Marshal(NewEmbed("t", "https://ex.com", "s", "c / f", "", 1))
	if strings.Contains(string(b), "image") {
		t.Errorf("画像なしでimageキーが出ている: %s", b)
	}
}

func TestBatchIndexes(t *testing.T) {
	small := make([]Embed, 12)
	got := BatchIndexes(small, 5)
	if fmt.Sprint(got) != "[[0 1 2 3 4] [5 6 7 8 9] [10 11]]" {
		t.Errorf("got %v", got)
	}
	if got := BatchIndexes(small, 50); len(got[0]) != 10 {
		t.Errorf("上限10を超えている: %v", got)
	}
	// 文字数合計が予算（5800字）を超える手前で区切る
	big := []Embed{
		{Description: strings.Repeat("a", 3000)},
		{Description: strings.Repeat("a", 3000)},
		{Description: strings.Repeat("a", 100)},
	}
	if got := BatchIndexes(big, 10); fmt.Sprint(got) != "[[0] [1 2]]" {
		t.Errorf("got %v", got)
	}
}

// fakeDiscord は応答コードを順に返すWebhook。
type fakeDiscord struct {
	mu       sync.Mutex
	codes    []int
	payloads []string
	queries  []string
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	f.payloads = append(f.payloads, string(b))
	f.queries = append(f.queries, r.URL.RawQuery)
	code := http.StatusOK
	if len(f.codes) > 0 {
		code, f.codes = f.codes[0], f.codes[1:]
	}
	w.WriteHeader(code)
}

func newTestClient(t *testing.T, f *fakeDiscord) *Client {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New(srv.URL + "/api/webhooks/1/x")
	c.sleep = func(time.Duration) {}
	return c
}

func TestPostUsesWaitTrue(t *testing.T) {
	f := &fakeDiscord{}
	c := newTestClient(t, f)
	if err := c.SendContent(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if f.queries[0] != "wait=true" {
		t.Errorf("wait=true が付いていない: %q", f.queries[0])
	}
}

func TestPostEmbedsFallsBackToIndividualKeepingImage(t *testing.T) {
	// バッチ: 500×3（リトライ込み）→ 10秒後の再送も500×3 → 個別送信は成功
	f := &fakeDiscord{codes: []int{500, 500, 500, 500, 500, 500}}
	c := newTestClient(t, f)
	embeds := []Embed{
		NewEmbed("a", "https://ex.com/a", "s", "f", "https://img/a.png", 1),
		NewEmbed("b", "https://ex.com/b", "s", "f", "https://img/b.png", 1),
	}
	posted := c.PostEmbeds(context.Background(), embeds, 10)
	if len(posted) != 2 {
		t.Fatalf("posted = %v", posted)
	}
	last := f.payloads[len(f.payloads)-1]
	if !strings.Contains(last, "img/b.png") {
		t.Errorf("個別送信で画像が外されている: %s", last)
	}
}

func TestPostEmbedsStripsImageOnlyForFailingEmbed(t *testing.T) {
	// バッチ400 → 個別a成功 → 個別b 400 → 画像を外したbは成功
	f := &fakeDiscord{codes: []int{400, 200, 400, 200}}
	c := newTestClient(t, f)
	embeds := []Embed{
		NewEmbed("a", "https://ex.com/a", "s", "f", "https://img/a.png", 1),
		NewEmbed("b", "https://ex.com/b", "s", "f", "https://img/b.png", 1),
	}
	posted := c.PostEmbeds(context.Background(), embeds, 10)
	if len(posted) != 2 {
		t.Fatalf("posted = %v", posted)
	}
	if !strings.Contains(f.payloads[1], "img/a.png") {
		t.Errorf("成功したembedの画像が外されている: %s", f.payloads[1])
	}
	if strings.Contains(f.payloads[3], "img/b.png") {
		t.Errorf("失敗したembedの画像が外れていない: %s", f.payloads[3])
	}
}
