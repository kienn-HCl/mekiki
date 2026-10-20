package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompat はOpenAI互換のChat Completions API（POST {base}/chat/completions）経由の呼び出し。
// OpenAI・Gemini（OpenAI互換エンドポイント）・OpenRouter・Ollama等のローカルLLMで共通に使う。
//
// プロバイダ・モデルによって構造化出力（response_format）や reasoning_effort に未対応のものがあるため、
// 400が返ったらそれらを外して1回だけ再送する。応答はプロンプトの出力形式指示＋寛容なパースで扱う。
// サーバー側のWebページ取得ツールは使わない（WebFetch救済はタイトルのみの要約になる）。
type OpenAICompat struct {
	name    string
	baseURL string
	apiKey  string
	model   string
	effort  string
	http    *http.Client
	retries int
}

func NewOpenAICompat(name, baseURL, apiKey, model, effort string, keyOptional bool) (*OpenAICompat, error) {
	if baseURL == "" {
		return nil, errors.New("APIのURLが未設定です")
	}
	if apiKey == "" && !keyOptional {
		return nil, fmt.Errorf("%sのAPIキーが未設定です（設定画面で入力するか、環境変数で渡してください）", name)
	}
	if model == "" {
		return nil, fmt.Errorf("%sのモデル名が未設定です", name)
	}
	return &OpenAICompat{
		name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model,
		effort: openAIEffort(effort), http: &http.Client{}, retries: 3,
	}, nil
}

// openAIEffort はAnthropicのeffort段階をOpenAI系の reasoning_effort（low/medium/high）に寄せる。
func openAIEffort(e string) string {
	switch e {
	case "xhigh", "max":
		return "high"
	}
	return e
}

func (c *OpenAICompat) Name() string { return c.name + ":" + c.model }

type chatRequest struct {
	Model           string          `json:"model"`
	Messages        []chatMessage   `json:"messages"`
	ResponseFormat  *responseFormat `json:"response_format,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema map[string]any `json:"json_schema,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
			Refusal *string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.msg) }

func (c *OpenAICompat) Complete(ctx context.Context, req Request) (Response, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	body := chatRequest{
		Model:           c.model,
		Messages:        []chatMessage{{Role: "user", Content: req.Prompt}},
		ReasoningEffort: c.effort,
	}
	if req.JSONSchema != nil {
		body.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: map[string]any{
			"name": "response", "schema": req.JSONSchema, "strict": true,
		}}
	}
	resp, err := c.send(ctx, body)
	var he *httpError
	if errors.As(err, &he) && he.code == http.StatusBadRequest &&
		(body.ResponseFormat != nil || body.ReasoningEffort != "") {
		// 構造化出力・reasoning_effort非対応のモデル。外して再送する
		body.ResponseFormat, body.ReasoningEffort = nil, ""
		resp, err = c.send(ctx, body)
	}
	if err != nil {
		return Response{}, fmt.Errorf("%s: %w", c.name, err)
	}
	if len(resp.Choices) == 0 {
		return Response{}, fmt.Errorf("%s: 応答が空です", c.name)
	}
	ch := resp.Choices[0]
	out := Response{}
	if resp.Usage != nil {
		out.Usage = &Usage{Input: resp.Usage.PromptTokens, Output: resp.Usage.CompletionTokens}
		if d := resp.Usage.PromptTokensDetails; d != nil {
			out.Usage.CacheRead = d.CachedTokens
		}
	}
	if ch.Message.Refusal != nil && *ch.Message.Refusal != "" {
		return out, fmt.Errorf("%s: 応答を拒否されました: %.200s", c.name, *ch.Message.Refusal)
	}
	if ch.FinishReason == "length" {
		return out, fmt.Errorf("%s: 出力が上限で打ち切られました", c.name)
	}
	if ch.Message.Content != nil {
		out.Text = strings.TrimSpace(*ch.Message.Content)
	}
	return out, nil
}

// send は1リクエストを送る。429・5xx・接続エラーは指数バックオフで再試行する。
func (c *OpenAICompat) send(ctx context.Context, body chatRequest) (*chatResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, timeoutErr(ctx, lastErr)
			case <-time.After(time.Duration(1<<(attempt-1)) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		// OpenRouterのアプリ識別用ヘッダ（他のプロバイダでは無視される）
		req.Header.Set("X-Title", "mekiki")
		res, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, timeoutErr(ctx, err)
			}
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, 10<<20))
		res.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var parsed chatResponse
		jsonErr := json.Unmarshal(data, &parsed)
		if res.StatusCode >= 300 {
			msg := strings.TrimSpace(string(data))
			if jsonErr == nil && parsed.Error != nil && parsed.Error.Message != "" {
				msg = parsed.Error.Message
			}
			if len(msg) > 300 {
				msg = msg[:300]
			}
			lastErr = &httpError{code: res.StatusCode, msg: msg}
			if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
				continue
			}
			return nil, lastErr
		}
		if jsonErr != nil {
			return nil, fmt.Errorf("応答JSONのパースに失敗: %w", jsonErr)
		}
		return &parsed, nil
	}
	return nil, lastErr
}

func timeoutErr(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("タイムアウト")
	}
	return err
}
