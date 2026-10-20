package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ツール制限。-p（非対話）モードでは権限が必要なツールは自動拒否されるが、
// 読み取り系は権限なしで動くため、信頼できない本文を渡す呼び出しでは明示的に塞ぐ。
// WebFetchを使わせる場合は --allowedTools での明示が必須（ないと自動拒否される）。
const (
	contentDisallowedTools  = "WebFetch WebSearch Bash Read Glob Grep Edit Write"
	fallbackAllowedTools    = "WebFetch"
	fallbackDisallowedTools = "WebSearch Bash Read Glob Grep Edit Write"
)

// CLI は claude -p（Claude Code CLI）経由の呼び出し。ログイン済みのサブスクリプション枠で動く。
type CLI struct {
	path  string
	model string
}

func NewCLI(path, model string) (*CLI, error) {
	if path == "" {
		path = "claude"
	}
	if _, err := exec.LookPath(path); err != nil {
		return nil, fmt.Errorf("claude コマンドが見つかりません（%s）: %w", path, err)
	}
	return &CLI{path: path, model: model}, nil
}

func (c *CLI) Name() string {
	if c.model != "" {
		return "cli:" + c.model
	}
	return "cli"
}

func (c *CLI) Complete(ctx context.Context, req Request) (Response, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	args := []string{"-p", "--output-format", "json"}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	if req.AllowWebFetch {
		args = append(args, "--allowedTools", fallbackAllowedTools, "--disallowedTools", fallbackDisallowedTools)
	} else {
		args = append(args, "--disallowedTools", contentDisallowedTools)
	}
	cmd := exec.CommandContext(ctx, c.path, args...)
	// プロンプトは必ずstdinで渡す。引数だと長い本文でLinuxの単一引数長上限（128KiB）を超える
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Response{}, errors.New("タイムアウト")
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		return Response{}, fmt.Errorf("claude CLIが異常終了 (%v): %s", err, msg)
	}

	var out struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Usage   *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		// JSONで返らなかった場合は生stdout全体を生成テキストとして扱う
		return Response{Text: strings.TrimSpace(stdout.String())}, nil
	}
	if out.IsError {
		return Response{}, fmt.Errorf("claudeがエラーを返却: %.500s", out.Result)
	}
	resp := Response{Text: strings.TrimSpace(out.Result)}
	if out.Usage != nil {
		resp.Usage = &Usage{
			Input:         out.Usage.InputTokens,
			Output:        out.Usage.OutputTokens,
			CacheRead:     out.Usage.CacheReadInputTokens,
			CacheCreation: out.Usage.CacheCreationInputTokens,
		}
	}
	return resp, nil
}
