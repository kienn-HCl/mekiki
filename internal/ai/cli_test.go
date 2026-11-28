package ai

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCLIModelFromModelUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
cat >/dev/null
echo '{"result":"要約","is_error":false,"usage":{"input_tokens":3,"output_tokens":50,"cache_read_input_tokens":8000,"cache_creation_input_tokens":12000},"modelUsage":{"claude-haiku-5-5":{"outputTokens":5},"claude-opus-5-5":{"outputTokens":50}}}'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := NewCLI(path, "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Complete(context.Background(), Request{Stage: "summarize", Prompt: "p", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "要約" || resp.Usage == nil || resp.Usage.Model != "claude-opus-5-5" || resp.Usage.CacheCreation != 12000 {
		t.Errorf("応答: %+v %+v", resp, resp.Usage)
	}
}
