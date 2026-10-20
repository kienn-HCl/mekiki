package config

import (
	"fmt"
	"testing"
)

func TestParseTimes(t *testing.T) {
	got, err := ParseTimes("18:00, 7:00、12:30 07:00")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[07:00 12:30 18:00]" {
		t.Errorf("got %v", got)
	}
	if got, _ := ParseTimes(""); len(got) != 0 {
		t.Errorf("空文字列: %v", got)
	}
	if _, err := ParseTimes("25:00"); err == nil {
		t.Error("不正な時刻がエラーにならない")
	}
}

func TestRoundTrip(t *testing.T) {
	s := Defaults()
	s.Backend = BackendOpenRouter
	s.APIKeys[BackendOpenRouter] = "sk-or-xxx"
	s.Models[BackendOpenRouter] = "openai/gpt-5.1"
	s.WebFetch[BackendAnthropic] = false
	s.Efforts[BackendOpenAI] = "low"
	got := Load(s.Map())
	if fmt.Sprint(got) != fmt.Sprint(s) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, s)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("検証を通らない: %v", err)
	}
	if err := Defaults().Validate(); err != nil {
		t.Errorf("デフォルト値が検証を通らない: %v", err)
	}
}

func TestLegacyBackendName(t *testing.T) {
	if got := Load(map[string]string{"backend": "api"}); got.Backend != BackendAnthropic {
		t.Errorf("旧名 api が anthropic にならない: %q", got.Backend)
	}
}

func TestProviderResolution(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "env-key")
	s := Defaults()
	if s.Model(BackendAnthropic) != "claude-opus-5-5" {
		t.Errorf("Anthropicの既定モデル: %q", s.Model(BackendAnthropic))
	}
	if s.APIKey(BackendGemini) != "env-key" {
		t.Error("環境変数のキーが使われない")
	}
	s.APIKeys[BackendGemini] = "screen-key"
	if s.APIKey(BackendGemini) != "screen-key" {
		t.Error("画面のキーが優先されない")
	}
	if s.BaseURL(BackendGemini) != "https://generativelanguage.googleapis.com/v1beta/openai" {
		t.Errorf("GeminiのURL: %q", s.BaseURL(BackendGemini))
	}
	s.CompatURL = "http://localhost:11434/v1/"
	if s.BaseURL(BackendCompat) != "http://localhost:11434/v1" {
		t.Errorf("互換URLの末尾スラッシュ: %q", s.BaseURL(BackendCompat))
	}
}

func TestValidate(t *testing.T) {
	bad := []func(*Settings){
		func(s *Settings) { s.Backend = "unknown" },
		func(s *Settings) { s.FetchInterval = 1 },
		func(s *Settings) { s.Timezone = "Mars/Base" },
		func(s *Settings) { s.Backend = BackendOpenAI }, // モデル未指定
		func(s *Settings) { s.Backend = BackendCompat; s.Models[BackendCompat] = "llama"; s.CompatURL = "" },
	}
	for i, f := range bad {
		s := Defaults()
		f(&s)
		if s.Validate() == nil {
			t.Errorf("case %d: 不正な設定が通る", i)
		}
	}
	s := Defaults()
	s.Backend = BackendCLI
	if err := s.Validate(); err != nil {
		t.Errorf("CLI: %v", err)
	}
}

func TestProfileValidate(t *testing.T) {
	if err := DefaultProfile("tech", "技術").Validate(); err != nil {
		t.Errorf("既定プロファイル: %v", err)
	}
	for _, slug := range []string{"", "Tech", "a b", "日本語", "-x"} {
		if DefaultProfile(slug, "x").Validate() == nil {
			t.Errorf("不正なID %q が通る", slug)
		}
	}
	p := DefaultProfile("news", "ニュース")
	p.DiscordWebhook = "http://insecure"
	if p.Validate() == nil {
		t.Error("httpのWebhookが通る")
	}
}

func TestLegacyEffortAndWebFetch(t *testing.T) {
	s := Load(map[string]string{"effort": "high", "web_fetch": "0"})
	if s.Effort(BackendAnthropic) != "high" || s.Effort(BackendOpenAI) != "" {
		t.Errorf("旧effort: anthropic=%q openai=%q", s.Effort(BackendAnthropic), s.Effort(BackendOpenAI))
	}
	if s.UseWebFetch(BackendAnthropic) || s.UseWebFetch(BackendCLI) {
		t.Error("旧web_fetch=0 が引き継がれていない")
	}
	d := Load(map[string]string{})
	if !d.UseWebFetch(BackendAnthropic) || d.UseWebFetch(BackendOpenAI) || d.Effort(BackendAnthropic) != "medium" {
		t.Errorf("既定値: %+v", d)
	}
}
