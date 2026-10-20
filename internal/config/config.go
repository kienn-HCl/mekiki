// Package config はWeb画面から編集する設定（DBのsettingsテーブル）の型・デフォルト値・検証を持つ。
package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	BackendAPI = "api" // Anthropic API（デフォルト）
	BackendCLI = "cli" // claude -p（Claude Codeのサブスクリプション枠）
)

const DefaultModel = "claude-opus-5-5"

const DefaultCriteria = `- 幅広いカテゴリから選ぶようにしてください（1カテゴリに偏らない）
- 技術的に興味深いもの、実務に役立つもの、業界動向として重要なものを優先してください
- 同じ話題の重複記事は、最も信頼できるソースのものを1つだけ選んでください`

type Settings struct {
	// AI
	Backend  string // api | cli
	APIKey   string // 空なら環境変数 ANTHROPIC_API_KEY
	Model    string // api用
	Effort   string // api用: low | medium | high | xhigh | max
	CLIPath  string // cli用: claude コマンド
	CLIModel string // cli用: --model（空ならCLIのデフォルト）
	WebFetch bool   // 本文が取れない記事でClaudeにWebFetchさせるか

	// 選定・ページ
	SelectCriteria string
	UserMemory     string // ユーザーの興味・趣向
	PickCount      int    // 1回の実行で追加する最大記事数
	HoursBack      int    // 何時間前までの記事を候補にするか
	Overview       bool   // その日のまとめ（概要）を生成するか

	// スケジュール
	Timezone      string
	FetchInterval int      // フィード取得間隔（分）。0で定期取得しない
	DigestTimes   []string // 要約実行時刻 "HH:MM"

	// フィード取得
	ArticlesPerFeed   int
	MaxFailures       int
	FailureRetryHours int
	RetentionDays     int // 記事（RSSリーダー側）の保持日数

	// Discord（任意）
	DiscordWebhook   string
	DiscordEmbeds    int
	DiscordTitle     string
	DiscordColor     int
	DiscordBaseURL   string // 投稿にWeb画面へのリンクを付ける場合のURL（例 http://host:8080）
	DiscordAfterFail bool   // 失敗時に⚠️通知を送るか
}

func Defaults() Settings {
	return Settings{
		Backend:           BackendAPI,
		Model:             DefaultModel,
		Effort:            "medium",
		CLIPath:           "claude",
		WebFetch:          true,
		SelectCriteria:    DefaultCriteria,
		PickCount:         20,
		HoursBack:         24,
		Overview:          true,
		Timezone:          "Asia/Tokyo",
		FetchInterval:     60,
		DigestTimes:       []string{"07:00"},
		ArticlesPerFeed:   20,
		MaxFailures:       5,
		FailureRetryHours: 168,
		RetentionDays:     30,
		DiscordEmbeds:     5,
		DiscordTitle:      "注目記事ピックアップ",
		DiscordColor:      0x2ECC71,
		DiscordAfterFail:  true,
	}
}

// Load はDBのkey-valueからSettingsを組み立てる。未設定・不正値はデフォルト。
func Load(kv map[string]string) Settings {
	s := Defaults()
	str := func(key string, dst *string) {
		if v, ok := kv[key]; ok {
			*dst = v
		}
	}
	num := func(key string, dst *int) {
		if v, ok := kv[key]; ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				*dst = n
			}
		}
	}
	flag := func(key string, dst *bool) {
		if v, ok := kv[key]; ok {
			*dst = v == "1" || v == "true" || v == "on"
		}
	}
	str("backend", &s.Backend)
	str("api_key", &s.APIKey)
	str("model", &s.Model)
	str("effort", &s.Effort)
	str("cli_path", &s.CLIPath)
	str("cli_model", &s.CLIModel)
	flag("web_fetch", &s.WebFetch)
	str("select_criteria", &s.SelectCriteria)
	str("user_memory", &s.UserMemory)
	num("pick_count", &s.PickCount)
	num("hours_back", &s.HoursBack)
	flag("overview", &s.Overview)
	str("timezone", &s.Timezone)
	num("fetch_interval", &s.FetchInterval)
	if v, ok := kv["digest_times"]; ok {
		s.DigestTimes, _ = ParseTimes(v)
	}
	num("articles_per_feed", &s.ArticlesPerFeed)
	num("max_failures", &s.MaxFailures)
	num("failure_retry_hours", &s.FailureRetryHours)
	num("retention_days", &s.RetentionDays)
	str("discord_webhook", &s.DiscordWebhook)
	num("discord_embeds", &s.DiscordEmbeds)
	str("discord_title", &s.DiscordTitle)
	num("discord_color", &s.DiscordColor)
	str("discord_base_url", &s.DiscordBaseURL)
	flag("discord_after_fail", &s.DiscordAfterFail)
	return s
}

// Map はDB保存用のkey-valueに変換する。
func (s Settings) Map() map[string]string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return map[string]string{
		"backend":             s.Backend,
		"api_key":             s.APIKey,
		"model":               s.Model,
		"effort":              s.Effort,
		"cli_path":            s.CLIPath,
		"cli_model":           s.CLIModel,
		"web_fetch":           b(s.WebFetch),
		"select_criteria":     s.SelectCriteria,
		"user_memory":         s.UserMemory,
		"pick_count":          strconv.Itoa(s.PickCount),
		"hours_back":          strconv.Itoa(s.HoursBack),
		"overview":            b(s.Overview),
		"timezone":            s.Timezone,
		"fetch_interval":      strconv.Itoa(s.FetchInterval),
		"digest_times":        strings.Join(s.DigestTimes, ","),
		"articles_per_feed":   strconv.Itoa(s.ArticlesPerFeed),
		"max_failures":        strconv.Itoa(s.MaxFailures),
		"failure_retry_hours": strconv.Itoa(s.FailureRetryHours),
		"retention_days":      strconv.Itoa(s.RetentionDays),
		"discord_webhook":     s.DiscordWebhook,
		"discord_embeds":      strconv.Itoa(s.DiscordEmbeds),
		"discord_title":       s.DiscordTitle,
		"discord_color":       strconv.Itoa(s.DiscordColor),
		"discord_base_url":    s.DiscordBaseURL,
		"discord_after_fail":  b(s.DiscordAfterFail),
	}
}

// Validate は保存前の検証。
func (s Settings) Validate() error {
	if s.Backend != BackendAPI && s.Backend != BackendCLI {
		return fmt.Errorf("AI連携方式が不正です: %q", s.Backend)
	}
	switch s.Effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("effortが不正です: %q", s.Effort)
	}
	if s.Backend == BackendAPI && strings.TrimSpace(s.Model) == "" {
		return fmt.Errorf("モデル名を指定してください")
	}
	if strings.TrimSpace(s.SelectCriteria) == "" {
		return fmt.Errorf("選定基準を入力してください")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("タイムゾーンが不正です: %q", s.Timezone)
	}
	checks := []struct {
		name      string
		v, lo, hi int
	}{
		{"1回の追加記事数", s.PickCount, 1, 100},
		{"対象期間（時間）", s.HoursBack, 1, 24 * 14},
		{"フィード取得間隔（分）", s.FetchInterval, 0, 24 * 60},
		{"1フィードの取得記事数", s.ArticlesPerFeed, 1, 200},
		{"スキップまでの連続失敗回数", s.MaxFailures, 1, 1000},
		{"スキップ中フィードの再試行間隔（時間）", s.FailureRetryHours, 1, 24 * 365},
		{"記事の保持日数", s.RetentionDays, 1, 3650},
		{"Discordの1メッセージあたりembed数", s.DiscordEmbeds, 1, 10},
	}
	for _, c := range checks {
		if c.v < c.lo || c.v > c.hi {
			return fmt.Errorf("%sは%d〜%dで指定してください", c.name, c.lo, c.hi)
		}
	}
	if s.FetchInterval > 0 && s.FetchInterval < 5 {
		return fmt.Errorf("フィード取得間隔は5分以上にしてください（0で定期取得なし）")
	}
	return nil
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

func (s Settings) Location() *time.Location {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.FixedZone("JST", 9*3600)
	}
	return loc
}

// EffectiveAPIKey は画面で設定されたキー、なければ環境変数のキー。
func (s Settings) EffectiveAPIKey() string {
	if s.APIKey != "" {
		return s.APIKey
	}
	return os.Getenv("ANTHROPIC_API_KEY")
}
