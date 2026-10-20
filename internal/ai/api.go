package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// 拒否（refusal）時にサーバー側で別モデルに引き継がせる "fallbacks: default" に対応するモデル。
// 対応外のモデルに付けると400になるため、ここに載っているものだけに付ける
var fallbackModels = map[string]bool{
	"claude-fable-5-1":  true,
	"claude-opus-5-5":   true,
	"claude-opus-5":     true,
	"claude-sonnet-5-5": true,
}

// web_fetch はサーバー側でループするため、上限に達すると pause_turn で返ってくる。再開する最大回数
const maxPauseResumes = 3

// API はAnthropic API（Messages API）経由の呼び出し。
type API struct {
	client anthropic.Client
	model  string
	effort string
}

func NewAPI(apiKey, model, effort string) (*API, error) {
	if apiKey == "" {
		return nil, errors.New("APIキーが未設定です（設定画面で入力するか、環境変数 ANTHROPIC_API_KEY を設定してください）")
	}
	return &API{
		client: anthropic.NewClient(option.WithAPIKey(apiKey), option.WithMaxRetries(3)),
		model:  model,
		effort: effort,
	}, nil
}

func (a *API) Name() string { return "api:" + a.model }

func (a *API) Complete(ctx context.Context, req Request) (Response, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 16000,
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(req.Prompt)),
		},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffort(a.effort),
		},
	}
	if req.JSONSchema != nil {
		params.OutputConfig.Format = anthropic.BetaJSONOutputFormatParam{Schema: req.JSONSchema}
	}
	if req.AllowWebFetch {
		params.Tools = []anthropic.BetaToolUnionParam{{
			OfWebFetchTool20260209: &anthropic.BetaWebFetchTool20260209Param{
				MaxUses:          anthropic.Int(3),
				MaxContentTokens: anthropic.Int(20000),
			},
		}}
	}
	if fallbackModels[a.model] {
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
		params.Betas = append(params.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	}

	total := &Usage{}
	for i := 0; ; i++ {
		msg, err := a.client.Beta.Messages.New(ctx, params)
		if err != nil {
			return Response{Usage: total}, describeAPIError(err)
		}
		total.Input += msg.Usage.InputTokens
		total.Output += msg.Usage.OutputTokens
		total.CacheRead += msg.Usage.CacheReadInputTokens
		total.CacheCreation += msg.Usage.CacheCreationInputTokens

		switch msg.StopReason {
		case anthropic.BetaStopReasonRefusal:
			return Response{Usage: total}, fmt.Errorf("Claudeが応答を拒否しました（%s）", msg.StopDetails.Category)
		case anthropic.BetaStopReasonPauseTurn:
			// サーバー側ツールのループが上限に達した。応答をそのまま履歴に足して再送すると続きから再開する
			if i < maxPauseResumes {
				params.Messages = append(params.Messages, msg.ToParam())
				continue
			}
		case anthropic.BetaStopReasonMaxTokens:
			return Response{Usage: total}, errors.New("出力がmax_tokensで打ち切られました")
		}

		var b strings.Builder
		for _, block := range msg.Content {
			if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
				b.WriteString(t.Text)
			}
		}
		return Response{Text: strings.TrimSpace(b.String()), Usage: total}, nil
	}
}

func describeAPIError(err error) error {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		switch apierr.StatusCode {
		case 401:
			return fmt.Errorf("APIキーが無効です (401)")
		case 403:
			return fmt.Errorf("APIキーの権限がありません (403)")
		case 404:
			return fmt.Errorf("モデルが見つかりません (404): %s", apierr.Error())
		case 429:
			return fmt.Errorf("レートリミット (429): %s", apierr.Error())
		}
		if apierr.StatusCode >= 500 {
			return fmt.Errorf("APIサーバーエラー (%d): %s", apierr.StatusCode, apierr.Error())
		}
		return fmt.Errorf("APIエラー (%d): %s", apierr.StatusCode, apierr.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("タイムアウト")
	}
	return fmt.Errorf("API呼び出しに失敗: %w", err)
}
