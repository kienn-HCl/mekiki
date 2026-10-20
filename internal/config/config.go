// Package config はWeb画面から編集する設定の型・デフォルト値・検証を持つ。
//
// 設定は2種類ある:
//   - Settings: サーバ全体の設定（AI連携・フィード取得・タイムゾーン）。settingsテーブル
//   - Profile:  プロファイルごとの設定（選定基準・興味・実行時刻・Discord）。profilesテーブル
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AI連携の経路
const (
	BackendAnthropic  = "anthropic"  // Anthropic API（デフォルト）
	BackendOpenAI     = "openai"     // OpenAI API
	BackendGemini     = "gemini"     // Google Gemini API（OpenAI互換エンドポイント）
	BackendOpenRouter = "openrouter" // OpenRouter
	BackendCompat     = "compat"     // 任意のOpenAI互換API（Ollama・LM Studio・vLLM等）
	BackendCLI        = "cli"        // claude -p（Claude Codeのサブスクリプション枠）
)

// Provider はAPIキーとモデル名で使うAPI型の経路の定義。
type Provider struct {
	ID           string
	Label        string
	BaseURL      string // OpenAI互換の場合のエンドポイント（Anthropicは空）
	KeyEnv       string // 画面でキーを設定しない場合に読む環境変数
	DefaultModel string
	ModelHint    string // 画面のplaceholder
	KeyOptional  bool   // ローカルLLM等、キー不要のことがある
}

var Providers = []Provider{
	{ID: BackendAnthropic, Label: "Anthropic API", KeyEnv: "ANTHROPIC_API_KEY",
		DefaultModel: "claude-opus-5-5", ModelHint: "claude-opus-5-5"},
	{ID: BackendOpenAI, Label: "OpenAI", BaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY",
		ModelHint: "例: gpt-5.1"},
	{ID: BackendGemini, Label: "Google Gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		KeyEnv: "GEMINI_API_KEY", ModelHint: "例: gemini-2.5-flash"},
	{ID: BackendOpenRouter, Label: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1",
		KeyEnv: "OPENROUTER_API_KEY", ModelHint: "例: anthropic/claude-opus-5-5, openai/gpt-5.1"},
	{ID: BackendCompat, Label: "OpenAI互換（URLを指定）", KeyEnv: "OPENAI_COMPAT_API_KEY",
		ModelHint: "例: llama3.1", KeyOptional: true},
}

func ProviderByID(id string) (Provider, bool) {
	for _, p := range Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

const DefaultCriteria = `- 幅広いカテゴリから選ぶようにしてください（1カテゴリに偏らない）
- 技術的に興味深いもの、実務に役立つもの、業界動向として重要なものを優先してください
- 同じ話題の重複記事は、最も信頼できるソースのものを1つだけ選んでください`

// Settings はサーバ全体の設定。
type Settings struct {
	// AI
	Backend   string            // 経路（Backend* 定数）
	APIKeys   map[string]string // 経路ごとのAPIキー（空なら環境変数）
	Models    map[string]string // 経路ごとのモデル名
	CompatURL string            // BackendCompat のエンドポイント（例 http://localhost:11434/v1）
	Effort    string            // Anthropic: output_config.effort / OpenAI系: reasoning_effort（空なら送らない）
	CLIPath   string            // cli用: claude コマンド
	CLIModel  string            // cli用: --model（空ならCLIのデフォルト）
	WebFetch  bool              // 本文が取れない記事でAIにWebページを取得させるか（Anthropic・CLIのみ）

	// 全体
	Timezone string

	// フィード取得
	FetchInterval     int // 分。0で定期取得しない
	ArticlesPerFeed   int
	MaxFailures       int
	FailureRetryHours int
	RetentionDays     int // 記事（RSSリーダー側）の保持日数
}

func Defaults() Settings {
	return Settings{
		Backend:           BackendAnthropic,
		APIKeys:           map[string]string{},
		Models:            map[string]string{},
		Effort:            "medium",
		CLIPath:           "claude",
		WebFetch:          true,
		Timezone:          "Asia/Tokyo",
		FetchInterval:     60,
		ArticlesPerFeed:   20,
		MaxFailures:       5,
		FailureRetryHours: 168,
		RetentionDays:     30,
	}
}

func parseInt(kv map[string]string, key string, dst *int) {
	if v, ok := kv[key]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			*dst = n
		}
	}
}

func parseBool(kv map[string]string, key string, dst *bool) {
	if v, ok := kv[key]; ok {
		*dst = v == "1" || v == "true" || v == "on"
	}
}

func boolStr(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// Load はDBのkey-valueからSettingsを組み立てる。未設定・不正値はデフォルト。
func Load(kv map[string]string) Settings {
	s := Defaults()
	str := func(key string, dst *string) {
		if v, ok := kv[key]; ok {
			*dst = v
		}
	}
	str("backend", &s.Backend)
	if s.Backend == "api" { // 旧名
		s.Backend = BackendAnthropic
	}
	for _, p := range Providers {
		if v := kv["api_key."+p.ID]; v != "" {
			s.APIKeys[p.ID] = v
		}
		if v := kv["model."+p.ID]; v != "" {
			s.Models[p.ID] = v
		}
	}
	str("compat_url", &s.CompatURL)
	str("effort", &s.Effort)
	str("cli_path", &s.CLIPath)
	str("cli_model", &s.CLIModel)
	parseBool(kv, "web_fetch", &s.WebFetch)
	str("timezone", &s.Timezone)
	parseInt(kv, "fetch_interval", &s.FetchInterval)
	parseInt(kv, "articles_per_feed", &s.ArticlesPerFeed)
	parseInt(kv, "max_failures", &s.MaxFailures)
	parseInt(kv, "failure_retry_hours", &s.FailureRetryHours)
	parseInt(kv, "retention_days", &s.RetentionDays)
	return s
}

// Map はDB保存用のkey-valueに変換する。
func (s Settings) Map() map[string]string {
	m := map[string]string{
		"backend":             s.Backend,
		"compat_url":          s.CompatURL,
		"effort":              s.Effort,
		"cli_path":            s.CLIPath,
		"cli_model":           s.CLIModel,
		"web_fetch":           boolStr(s.WebFetch),
		"timezone":            s.Timezone,
		"fetch_interval":      strconv.Itoa(s.FetchInterval),
		"articles_per_feed":   strconv.Itoa(s.ArticlesPerFeed),
		"max_failures":        strconv.Itoa(s.MaxFailures),
		"failure_retry_hours": strconv.Itoa(s.FailureRetryHours),
		"retention_days":      strconv.Itoa(s.RetentionDays),
	}
	for _, p := range Providers {
		m["api_key."+p.ID] = s.APIKeys[p.ID]
		m["model."+p.ID] = s.Models[p.ID]
	}
	return m
}

// Validate は保存前の検証。
func (s Settings) Validate() error {
	if _, ok := ProviderByID(s.Backend); !ok && s.Backend != BackendCLI {
		return fmt.Errorf("AI連携方式が不正です: %q", s.Backend)
	}
	switch s.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("effortが不正です: %q", s.Effort)
	}
	if p, ok := ProviderByID(s.Backend); ok && s.Model(p.ID) == "" {
		return fmt.Errorf("%sのモデル名を指定してください", p.Label)
	}
	if s.Backend == BackendCompat && !strings.HasPrefix(s.CompatURL, "http") {
		return fmt.Errorf("OpenAI互換APIのURLを指定してください（例 http://localhost:11434/v1）")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("タイムゾーンが不正です: %q", s.Timezone)
	}
	checks := []struct {
		name      string
		v, lo, hi int
	}{
		{"フィード取得間隔（分）", s.FetchInterval, 0, 24 * 60},
		{"1フィードの取得記事数", s.ArticlesPerFeed, 1, 200},
		{"スキップまでの連続失敗回数", s.MaxFailures, 1, 1000},
		{"スキップ中フィードの再試行間隔（時間）", s.FailureRetryHours, 1, 24 * 365},
		{"記事の保持日数", s.RetentionDays, 1, 3650},
	}
	if err := checkRanges(checks); err != nil {
		return err
	}
	if s.FetchInterval > 0 && s.FetchInterval < 5 {
		return fmt.Errorf("フィード取得間隔は5分以上にしてください（0で定期取得なし）")
	}
	return nil
}

func checkRanges(checks []struct {
	name      string
	v, lo, hi int
}) error {
	for _, c := range checks {
		if c.v < c.lo || c.v > c.hi {
			return fmt.Errorf("%sは%d〜%dで指定してください", c.name, c.lo, c.hi)
		}
	}
	return nil
}

// Model は経路のモデル名（未設定ならその経路の既定）。
func (s Settings) Model(backend string) string {
	if m := strings.TrimSpace(s.Models[backend]); m != "" {
		return m
	}
	p, _ := ProviderByID(backend)
	return p.DefaultModel
}

// APIKey は画面で設定されたキー、なければ経路の環境変数のキー。
func (s Settings) APIKey(backend string) string {
	if k := s.APIKeys[backend]; k != "" {
		return k
	}
	if p, ok := ProviderByID(backend); ok && p.KeyEnv != "" {
		return os.Getenv(p.KeyEnv)
	}
	return ""
}

// BaseURL はOpenAI互換経路のエンドポイント。
func (s Settings) BaseURL(backend string) string {
	if backend == BackendCompat {
		return strings.TrimRight(strings.TrimSpace(s.CompatURL), "/")
	}
	p, _ := ProviderByID(backend)
	return p.BaseURL
}

// SupportsWebFetch はその経路でAIにWebページを取得させられるか。
func (s Settings) SupportsWebFetch() bool {
	return s.Backend == BackendAnthropic || s.Backend == BackendCLI
}

func (s Settings) Location() *time.Location {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.FixedZone("JST", 9*3600)
	}
	return loc
}

// ---- プロファイル ----

// Profile はプロファイルごとの設定。用途ごと（技術記事・ニュースなど）にページを分ける単位。
type Profile struct {
	ID       int64
	Slug     string // URLに使う識別子（例 tech）
	Name     string // 表示名
	Position int

	SelectCriteria string
	UserMemory     string   // 興味・趣向
	PickCount      int      // 1回の実行で追加する最大記事数
	HoursBack      int      // 何時間前までの記事を候補にするか
	Overview       bool     // 今日のまとめを生成するか
	DigestTimes    []string // 要約実行時刻 "HH:MM"

	// Discord（任意）
	DiscordWebhook   string
	DiscordEmbeds    int
	DiscordTitle     string
	DiscordColor     int
	DiscordBaseURL   string // 投稿にWeb画面へのリンクを付ける場合のURL（例 http://host:8080）
	DiscordAfterFail bool
}

func DefaultProfile(slug, name string) Profile {
	return Profile{
		Slug:             slug,
		Name:             name,
		SelectCriteria:   DefaultCriteria,
		PickCount:        20,
		HoursBack:        24,
		Overview:         true,
		DigestTimes:      []string{"07:00"},
		DiscordEmbeds:    5,
		DiscordTitle:     name + " 注目記事ピックアップ",
		DiscordColor:     0x2ECC71,
		DiscordAfterFail: true,
	}
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func (p Profile) Validate() error {
	if !slugRe.MatchString(p.Slug) {
		return fmt.Errorf("プロファイルIDは英小文字・数字・-・_ の32字以内で指定してください: %q", p.Slug)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("プロファイル名を入力してください")
	}
	if strings.TrimSpace(p.SelectCriteria) == "" {
		return fmt.Errorf("選定基準を入力してください")
	}
	if p.DiscordWebhook != "" && !strings.HasPrefix(p.DiscordWebhook, "https://") {
		return fmt.Errorf("Discord Webhook URLは https:// で始まるURLを指定してください")
	}
	return checkRanges([]struct {
		name      string
		v, lo, hi int
	}{
		{"1回の追加記事数", p.PickCount, 1, 100},
		{"対象期間（時間）", p.HoursBack, 1, 24 * 14},
		{"Discordの1メッセージあたりembed数", p.DiscordEmbeds, 1, 10},
	})
}

// ParseTimes は "07:00, 12:30" のような時刻リストをパースし、正規化・ソートして返す。
func ParseTimes(v string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '、'
	}) {
		t, err := time.Parse("15:04", strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("時刻の形式が不正です（HH:MM）: %q", part)
		}
		norm := t.Format("15:04")
		if !seen[norm] {
			seen[norm] = true
			out = append(out, norm)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Timezones は設定画面のタイムゾーン選択肢（よく使うもの）。
var Timezones = []string{
	"Asia/Tokyo", "Asia/Seoul", "Asia/Shanghai", "Asia/Taipei", "Asia/Hong_Kong", "Asia/Singapore",
	"Asia/Bangkok", "Asia/Jakarta", "Asia/Manila", "Asia/Kolkata", "Asia/Dubai",
	"Australia/Sydney", "Australia/Melbourne", "Australia/Perth", "Pacific/Auckland", "Pacific/Honolulu",
	"Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Berlin", "Europe/Madrid",
	"Europe/Rome", "Europe/Amsterdam", "Europe/Stockholm", "Europe/Helsinki", "Europe/Istanbul", "Europe/Moscow",
	"Africa/Cairo", "Africa/Johannesburg",
	"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles", "America/Anchorage",
	"America/Toronto", "America/Vancouver", "America/Mexico_City", "America/Sao_Paulo", "America/Buenos_Aires",
	"UTC",
}
