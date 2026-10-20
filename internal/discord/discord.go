// Package discord はDiscord Webhookへの投稿（任意機能）を担う。
// 設定画面でWebhook URLを入れたときだけ、要約実行で追加された記事を投稿する。
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	MaxEmbedsPerMessage = 10
	// Discordは1メッセージ内の全embedの文字数合計を6000字に制限している。安全マージン込みの予算
	embedCharBudget  = 5800
	titleLimit       = 256
	descriptionLimit = 4096
	footerLimit      = 2048
	contentLimit     = 2000
	maxAttempts      = 3 // 429・5xx時の再試行を含む1リクエストあたりの最大送信回数
	postTimeout      = 15 * time.Second
)

type Error struct {
	Code int // HTTPステータス。接続エラー等は0
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

type Embed struct {
	Title       string       `json:"title,omitempty"`
	URL         string       `json:"url,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color,omitempty"`
	Footer      *EmbedFooter `json:"footer,omitempty"`
	Image       *EmbedImage  `json:"image,omitempty"`
}

type EmbedFooter struct {
	Text string `json:"text"`
}

type EmbedImage struct {
	URL string `json:"url"`
}

func Truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}

func NewEmbed(title, link, summary, footer, image string, color int) Embed {
	e := Embed{
		Title:       Truncate(title, titleLimit),
		URL:         link,
		Description: Truncate(summary, descriptionLimit),
		Color:       color,
		Footer:      &EmbedFooter{Text: Truncate(footer, footerLimit)},
	}
	if image != "" {
		e.Image = &EmbedImage{URL: image}
	}
	return e
}

func charCount(e Embed) int {
	n := len([]rune(e.Title)) + len([]rune(e.Description))
	if e.Footer != nil {
		n += len([]rune(e.Footer.Text))
	}
	return n
}

// BatchIndexes はembed列を「最大maxPer個」かつ「文字数合計が予算以内」のバッチに分割する。
func BatchIndexes(embeds []Embed, maxPer int) [][]int {
	maxPer = max(1, min(MaxEmbedsPerMessage, maxPer))
	var batches [][]int
	var cur []int
	curChars := 0
	for i, e := range embeds {
		c := charCount(e)
		if len(cur) > 0 && (len(cur) >= maxPer || curChars+c > embedCharBudget) {
			batches = append(batches, cur)
			cur, curChars = nil, 0
		}
		cur = append(cur, i)
		curChars += c
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

type Client struct {
	Webhook string
	http    *http.Client
	sleep   func(time.Duration)
}

func New(webhook string) *Client {
	return &Client{Webhook: webhook, http: &http.Client{Timeout: postTimeout}, sleep: time.Sleep}
}

// withWait は ?wait=true を付ける。デフォルト（wait=false）はメッセージが保存されなくても
// エラーを返さないため、Discord側の一時障害による投稿の消失を検知できない。
func withWait(u string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "wait=true"
}

func (c *Client) post(ctx context.Context, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, withWait(c.Webhook), bytes.NewReader(body))
		if err != nil {
			return &Error{Msg: err.Error()}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "mekiki/2.0")
		resp, err := c.http.Do(req)
		if err != nil {
			return &Error{Msg: err.Error()}
		}
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		last := attempt >= maxAttempts-1
		if !last && resp.StatusCode == http.StatusTooManyRequests {
			wait := retryAfter(resp.Header)
			slog.Warn("Discordレートリミット", "wait", wait)
			c.sleep(wait)
			continue
		}
		if !last && resp.StatusCode >= 500 {
			slog.Warn("Discord 5xx、2秒後に再試行", "status", resp.StatusCode)
			c.sleep(2 * time.Second)
			continue
		}
		return &Error{Code: resp.StatusCode, Msg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, detail)}
	}
}

func retryAfter(h http.Header) time.Duration {
	for _, name := range []string{"X-RateLimit-Reset-After", "Retry-After"} {
		if v := h.Get(name); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return time.Duration((f + 0.5) * float64(time.Second))
			}
		}
	}
	return 5500 * time.Millisecond
}

func (c *Client) SendContent(ctx context.Context, content string) error {
	return c.post(ctx, map[string]any{"content": Truncate(content, contentLimit)})
}

// PostEmbeds はembedをバッチ送信し、投稿に成功したembedのindexを返す。
//
// バッチ失敗時の縮退: 5xxなら同一内容を1回再送 → 1embedずつ個別送信（画像は保持）→
// 個別でも失敗したembedだけ画像を外して再送 → それでも失敗したらスキップ。
// 実コンテンツ入りの複数embedメッセージは時間帯によって500になることがあり、
// 「バッチ全体から画像除去」は画像を失うだけで解決しないことが運用で分かっている。
func (c *Client) PostEmbeds(ctx context.Context, embeds []Embed, perMessage int) []int {
	var posted []int
	for _, batch := range BatchIndexes(embeds, perMessage) {
		payload := map[string]any{"embeds": pick(embeds, batch)}
		err := c.post(ctx, payload)
		if err == nil {
			posted = append(posted, batch...)
			continue
		}
		var de *Error
		if errors.As(err, &de) && de.Code >= 500 {
			slog.Warn("バッチ送信が5xx、10秒後に同一内容で再送", "err", err)
			c.sleep(10 * time.Second)
			if err := c.post(ctx, payload); err == nil {
				posted = append(posted, batch...)
				continue
			}
		}
		slog.Warn("バッチ送信失敗、1embedずつ個別送信に切替", "err", err)
		for _, i := range batch {
			e := embeds[i]
			if err := c.post(ctx, map[string]any{"embeds": []Embed{e}}); err == nil {
				posted = append(posted, i)
				continue
			} else {
				slog.Warn("個別送信失敗", "url", e.URL, "err", err)
			}
			if e.Image != nil {
				e.Image = nil
				if err := c.post(ctx, map[string]any{"embeds": []Embed{e}}); err == nil {
					posted = append(posted, i)
					slog.Info("画像を外して投稿", "url", e.URL)
					continue
				}
			}
			slog.Error("embed送信失敗（スキップ）", "url", e.URL)
		}
	}
	return posted
}

func pick(embeds []Embed, idx []int) []Embed {
	out := make([]Embed, len(idx))
	for i, j := range idx {
		out[i] = embeds[j]
	}
	return out
}
