// Package conffile は設定ファイル（config.toml）の読み書きを担う。
//
// 設定（全体設定・プロファイル・フィード一覧）の正本は config.toml で、DBには実行時の写しを持つ。
//   - Web画面での変更は DB を更新してから Save でファイルに書き出す
//   - ファイルを直接編集すると、Watch が変更を検知して検証・反映する（再起動不要）
//   - ファイルが不正なら反映せず、エラーを保持して画面に出す（前の設定のまま動く）
//
// 記事・ページ・実行履歴・フィードの取得状態はアプリが自動で作るデータなので DB にだけ持つ。
package conffile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/store"
)

const header = `# mekiki の設定ファイル
#
# Web画面で保存するとこのファイルが書き換わり、このファイルを編集すると数秒でmekikiに反映されます
# （再起動は不要。書き間違いがあれば反映されず、Web画面にエラーが表示されます）。
# Web画面から保存し直すと、手で書いたコメントは消えます。
# APIキー・Webhook URLを含むため、他の人に見せないでください。
#
# 記事・日付ページ・実行履歴は mekiki.db に保存されます（このファイルには入りません）。

`

// ---- ファイルの形 ----

type File struct {
	Settings Settings  `toml:"settings"`
	Profiles []Profile `toml:"profiles" comment:"プロファイル（上から順に画面上部に並び、先頭がトップページの既定）"`
}

type Settings struct {
	Backend           string     `toml:"backend" comment:"使うAI: anthropic / openai / gemini / openrouter / compat / cli"`
	Timezone          string     `toml:"timezone" comment:"「今日」の日付と要約実行時刻の基準"`
	FetchInterval     int        `toml:"fetch_interval_minutes" comment:"フィード取得間隔（分）。0で定期取得しない"`
	ArticlesPerFeed   int        `toml:"articles_per_feed" comment:"1フィードから取得する最大記事数"`
	MaxFailures       int        `toml:"max_failures" comment:"連続して何回失敗したフィードを取得スキップするか"`
	FailureRetryHours int        `toml:"failure_retry_hours" comment:"スキップ中のフィードを再試行する間隔（時間）"`
	RetentionDays     int        `toml:"retention_days" comment:"RSS記事の保持日数（日付ページに載った記事は消えない）"`
	AI                AISettings `toml:"ai" comment:"AIごとの設定（APIキーが空なら環境変数を使う）"`
}

type AISettings struct {
	Anthropic  APIProvider    `toml:"anthropic"`
	OpenAI     APIProvider    `toml:"openai"`
	Gemini     APIProvider    `toml:"gemini"`
	OpenRouter APIProvider    `toml:"openrouter"`
	Compat     CompatProvider `toml:"compat" comment:"OpenAI互換API（Ollama等）"`
	CLI        CLIProvider    `toml:"cli" comment:"claude -p（Claude Code CLI）"`
}

type APIProvider struct {
	APIKey   string `toml:"api_key"`
	Model    string `toml:"model"`
	Effort   string `toml:"effort" comment:"low / medium / high / xhigh / max。空ならモデルの既定"`
	WebFetch *bool  `toml:"web_fetch,omitempty" comment:"本文が取れない記事でAIにWebページを取得させるか"`
}

type CompatProvider struct {
	URL    string `toml:"url" comment:"/chat/completions の手前まで（例 http://localhost:11434/v1）"`
	APIKey string `toml:"api_key" comment:"不要なら空のまま"`
	Model  string `toml:"model"`
	Effort string `toml:"effort" comment:"low / medium / high / xhigh / max。空ならモデルの既定"`
}

type CLIProvider struct {
	Path     string `toml:"path"`
	Model    string `toml:"model"`
	WebFetch bool   `toml:"web_fetch" comment:"本文が取れない記事でAIにWebページを取得させるか"`
}

type Profile struct {
	ID             int64    `toml:"id" comment:"内部ID（ページ・履歴との対応に使うので変えないでください。新しく足すときは省略）"`
	Slug           string   `toml:"slug" comment:"URLに使うID（/p/slug/）"`
	Name           string   `toml:"name"`
	Interests      string   `toml:"interests" multiline:"true" comment:"興味・趣向（選定・要約・今日のまとめに反映）"`
	SelectCriteria string   `toml:"select_criteria" multiline:"true"`
	PickCount      int      `toml:"pick_count" comment:"1回の実行で追加する最大記事数"`
	HoursBack      int      `toml:"hours_back" comment:"何時間前までの記事を候補にするか"`
	Overview       bool     `toml:"overview" comment:"今日のまとめを生成するか"`
	DigestTimes    []string `toml:"digest_times" comment:"要約実行の時刻（HH:MM）。空なら手動のみ"`
	Discord        Discord  `toml:"discord"`
	Feeds          []Feed   `toml:"feeds"`
}

type Discord struct {
	Webhook       string `toml:"webhook" comment:"空なら投稿しない"`
	Title         string `toml:"title"`
	Color         string `toml:"color" comment:"embedの色（16進）"`
	Embeds        int    `toml:"embeds_per_message"`
	BaseURL       string `toml:"base_url" comment:"投稿にページへのリンクを付ける場合のWeb画面のURL"`
	NotifyFailure bool   `toml:"notify_failure"`
}

type Feed struct {
	Title    string `toml:"title"`
	URL      string `toml:"url"`
	Category string `toml:"category"`
	Enabled  bool   `toml:"enabled"`
}

// ---- DB ⇔ ファイル ----

// Export はDBの設定をファイルの形にする。
func Export(ctx context.Context, st *store.Store) (*File, error) {
	kv, err := st.Settings(ctx)
	if err != nil {
		return nil, err
	}
	s := config.Load(kv)
	api := func(id string) APIProvider {
		p := APIProvider{APIKey: s.APIKeys[id], Model: s.Models[id], Effort: s.Efforts[id]}
		if v, ok := s.WebFetch[id]; ok {
			p.WebFetch = &v
		}
		return p
	}
	f := &File{Settings: Settings{
		Backend:           s.Backend,
		Timezone:          s.Timezone,
		FetchInterval:     s.FetchInterval,
		ArticlesPerFeed:   s.ArticlesPerFeed,
		MaxFailures:       s.MaxFailures,
		FailureRetryHours: s.FailureRetryHours,
		RetentionDays:     s.RetentionDays,
		AI: AISettings{
			Anthropic:  api(config.BackendAnthropic),
			OpenAI:     api(config.BackendOpenAI),
			Gemini:     api(config.BackendGemini),
			OpenRouter: api(config.BackendOpenRouter),
			Compat: CompatProvider{URL: s.CompatURL, APIKey: s.APIKeys[config.BackendCompat],
				Model: s.Models[config.BackendCompat], Effort: s.Efforts[config.BackendCompat]},
			CLI: CLIProvider{Path: s.CLIPath, Model: s.CLIModel, WebFetch: s.WebFetch[config.BackendCLI]},
		},
	}}
	profiles, err := st.Profiles(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		defs, err := st.FeedDefs(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		fp := Profile{
			ID: p.ID, Slug: p.Slug, Name: p.Name, Interests: p.UserMemory, SelectCriteria: p.SelectCriteria,
			PickCount: p.PickCount, HoursBack: p.HoursBack, Overview: p.Overview,
			DigestTimes: append([]string{}, p.DigestTimes...),
			Discord: Discord{
				Webhook: p.DiscordWebhook, Title: p.DiscordTitle, Color: fmt.Sprintf("%06X", p.DiscordColor),
				Embeds: p.DiscordEmbeds, BaseURL: p.DiscordBaseURL, NotifyFailure: p.DiscordAfterFail,
			},
		}
		for _, d := range defs {
			fp.Feeds = append(fp.Feeds, Feed{Title: d.Title, URL: d.URL, Category: d.Category, Enabled: d.Enabled})
		}
		f.Profiles = append(f.Profiles, fp)
	}
	return f, nil
}

// StripSecrets はAPIキーとWebhook URLを空にする（人に渡す用のコピー）。
func (f *File) StripSecrets() {
	for _, p := range []*APIProvider{&f.Settings.AI.Anthropic, &f.Settings.AI.OpenAI, &f.Settings.AI.Gemini,
		&f.Settings.AI.OpenRouter} {
		p.APIKey = ""
	}
	f.Settings.AI.Compat.APIKey = ""
	for i := range f.Profiles {
		f.Profiles[i].Discord.Webhook = ""
	}
}

func Encode(f *File) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode はファイルを読み、知らないキーはエラーにする（書き間違いに気づけるように）。
func Decode(data []byte) (*File, error) {
	var f File
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) && len(sm.Errors) > 0 {
			var keys []string
			for _, e := range sm.Errors {
				row, _ := e.Position()
				keys = append(keys, fmt.Sprintf("%d行目の %s", row, strings.Join(e.Key(), ".")))
			}
			return nil, fmt.Errorf("知らない設定項目があります: %s", strings.Join(keys, "、"))
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			return nil, fmt.Errorf("%d行%d列: %s", row, col, de.Error())
		}
		return nil, err
	}
	return &f, nil
}

// resolved はファイルの内容を検証済みのアプリの設定に直したもの。
type resolved struct {
	settings config.Settings
	profiles []config.Profile
	feeds    [][]store.FeedDef
}

func (f *File) resolve() (*resolved, error) {
	fs := f.Settings
	s := config.Defaults()
	s.Backend = fs.Backend
	s.Timezone = fs.Timezone
	s.FetchInterval = fs.FetchInterval
	s.ArticlesPerFeed = fs.ArticlesPerFeed
	s.MaxFailures = fs.MaxFailures
	s.FailureRetryHours = fs.FailureRetryHours
	s.RetentionDays = fs.RetentionDays
	for id, p := range map[string]APIProvider{
		config.BackendAnthropic: fs.AI.Anthropic, config.BackendOpenAI: fs.AI.OpenAI,
		config.BackendGemini: fs.AI.Gemini, config.BackendOpenRouter: fs.AI.OpenRouter,
		config.BackendCompat: {APIKey: fs.AI.Compat.APIKey, Model: fs.AI.Compat.Model, Effort: fs.AI.Compat.Effort},
	} {
		s.APIKeys[id] = strings.TrimSpace(p.APIKey)
		s.Models[id] = strings.TrimSpace(p.Model)
		s.Efforts[id] = strings.TrimSpace(p.Effort)
		if p.WebFetch != nil {
			if _, ok := s.WebFetch[id]; ok {
				s.WebFetch[id] = *p.WebFetch
			}
		}
	}
	s.CompatURL = strings.TrimSpace(fs.AI.Compat.URL)
	s.CLIPath = strings.TrimSpace(fs.AI.CLI.Path)
	if s.CLIPath == "" {
		s.CLIPath = "claude"
	}
	s.CLIModel = strings.TrimSpace(fs.AI.CLI.Model)
	s.WebFetch[config.BackendCLI] = fs.AI.CLI.WebFetch
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("[settings] %w", err)
	}

	if len(f.Profiles) == 0 {
		return nil, errors.New("プロファイルが1つもありません（[[profiles]] を1つ以上書いてください）")
	}
	r := &resolved{settings: s}
	slugs := map[string]bool{}
	ids := map[int64]bool{}
	for i, fp := range f.Profiles {
		where := fmt.Sprintf("%d番目のプロファイル（%s）", i+1, fp.Slug)
		times, err := config.ParseTimes(strings.Join(fp.DigestTimes, ","))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		color, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(fp.Discord.Color), "#"), 16, 32)
		if err != nil && strings.TrimSpace(fp.Discord.Color) != "" {
			return nil, fmt.Errorf("%s: discord.color は16進で書いてください: %q", where, fp.Discord.Color)
		}
		p := config.Profile{
			ID: fp.ID, Slug: strings.TrimSpace(fp.Slug), Name: strings.TrimSpace(fp.Name),
			UserMemory: strings.TrimSpace(fp.Interests), SelectCriteria: strings.TrimSpace(fp.SelectCriteria),
			PickCount: fp.PickCount, HoursBack: fp.HoursBack, Overview: fp.Overview, DigestTimes: times,
			DiscordWebhook: strings.TrimSpace(fp.Discord.Webhook), DiscordTitle: fp.Discord.Title,
			DiscordColor: int(color), DiscordEmbeds: fp.Discord.Embeds, DiscordBaseURL: strings.TrimSpace(fp.Discord.BaseURL),
			DiscordAfterFail: fp.Discord.NotifyFailure,
		}
		if p.DiscordEmbeds == 0 {
			p.DiscordEmbeds = 5
		}
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if slugs[p.Slug] {
			return nil, fmt.Errorf("%s: slug %q が重複しています", where, p.Slug)
		}
		slugs[p.Slug] = true
		if p.ID != 0 {
			if ids[p.ID] {
				return nil, fmt.Errorf("%s: id %d が重複しています", where, p.ID)
			}
			ids[p.ID] = true
		}
		var defs []store.FeedDef
		seen := map[string]bool{}
		for j, ff := range fp.Feeds {
			u, err := url.Parse(strings.TrimSpace(ff.URL))
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, fmt.Errorf("%s の%d番目のフィード: URLが不正です: %q", where, j+1, ff.URL)
			}
			if seen[u.String()] {
				return nil, fmt.Errorf("%s: フィードのURLが重複しています: %s", where, u)
			}
			seen[u.String()] = true
			title := strings.TrimSpace(ff.Title)
			if title == "" {
				title = u.Host
			}
			defs = append(defs, store.FeedDef{Title: title, URL: u.String(), Category: strings.TrimSpace(ff.Category), Enabled: ff.Enabled})
		}
		r.profiles = append(r.profiles, p)
		r.feeds = append(r.feeds, defs)
	}
	return r, nil
}

// ---- 管理 ----

// Manager はファイルとDBの同期を受け持つ。
type Manager struct {
	Store *store.Store
	Path  string

	mu      sync.Mutex
	lastSum [32]byte  // 最後に読み書きした内容のハッシュ（自分の書き込みを変更と誤検知しない）
	lastMod time.Time // 最後に確認したファイルの更新時刻
	lastErr string    // 最後のファイル読み込みエラー（画面に出す）
}

func New(st *store.Store, path string) *Manager { return &Manager{Store: st, Path: path} }

// Init は起動時に呼ぶ。ファイルがあればDBに反映し、なければDBの設定から作る。
func (m *Manager) Init(ctx context.Context) error {
	data, err := os.ReadFile(m.Path)
	if errors.Is(err, os.ErrNotExist) {
		slog.Info("設定ファイルを作成", "path", m.Path)
		return m.Save(ctx)
	}
	if err != nil {
		return err
	}
	if err := m.apply(ctx, data, "起動時"); err != nil {
		return fmt.Errorf("設定ファイル %s: %w", m.Path, err)
	}
	return nil
}

// Save はDBの設定をファイルに書き出す。Web画面で設定を変えたら呼ぶ。
func (m *Manager) Save(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := Export(ctx, m.Store)
	if err != nil {
		return err
	}
	if len(f.Profiles) == 0 {
		return nil // 初回起動の途中。プロファイルができてから書く
	}
	data, err := Encode(f)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if sum == m.lastSum {
		return nil
	}
	if err := writeAtomic(m.Path, data); err != nil {
		return fmt.Errorf("設定ファイルの書き込みに失敗: %w", err)
	}
	m.lastSum = sum
	if fi, err := os.Stat(m.Path); err == nil {
		m.lastMod = fi.ModTime()
	}
	m.lastErr = ""
	return nil
}

// SaveOrLog は Save の失敗をログに残すだけの版（画面操作の後に呼ぶ）。
func (m *Manager) SaveOrLog(ctx context.Context) {
	if m == nil {
		return
	}
	if err := m.Save(ctx); err != nil {
		slog.Error("設定ファイルの保存に失敗", "err", err)
	}
}

// Import はアップロードされた設定ファイルを検証してDBとファイルに反映する（復元用）。
func (m *Manager) Import(ctx context.Context, data []byte) error {
	// 失敗してもファイルは変えていないので、ファイルのエラー表示（lastErr）には残さない
	m.mu.Lock()
	err := m.applyLocked(ctx, data, "アップロード")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := writeAtomic(m.Path, data); err != nil {
		return err
	}
	m.lastSum = sha256.Sum256(data)
	if fi, err := os.Stat(m.Path); err == nil {
		m.lastMod = fi.ModTime()
	}
	return nil
}

// LastError は直近のファイル読み込みエラー（なければ空）。
func (m *Manager) LastError() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// Watch はファイルの変更を数秒おきに確認し、変わっていれば反映する。ctxが終わるまで戻らない。
func (m *Manager) Watch(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fi, err := os.Stat(m.Path)
		if err != nil {
			continue
		}
		m.mu.Lock()
		changed := !fi.ModTime().Equal(m.lastMod)
		m.mu.Unlock()
		if !changed {
			continue
		}
		data, err := os.ReadFile(m.Path)
		if err != nil {
			continue
		}
		m.mu.Lock()
		m.lastMod = fi.ModTime()
		same := sha256.Sum256(data) == m.lastSum
		if same {
			m.lastErr = "" // 書き間違いを直して、最後に反映した内容に戻った
		}
		m.mu.Unlock()
		if same {
			continue
		}
		if err := m.apply(ctx, data, "ファイルの変更"); err != nil {
			slog.Error("設定ファイルの変更を反映できません（前の設定のまま動きます）", "path", m.Path, "err", err)
		} else {
			slog.Info("設定ファイルの変更を反映しました", "path", m.Path)
		}
	}
}

// apply はファイルの内容を検証してDBに反映する。失敗したらDBは変えず、エラーを保持する。
func (m *Manager) apply(ctx context.Context, data []byte, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.applyLocked(ctx, data, reason)
	if err != nil {
		m.lastErr = err.Error()
		return err
	}
	m.lastErr = ""
	m.lastSum = sha256.Sum256(data)
	return nil
}

func (m *Manager) applyLocked(ctx context.Context, data []byte, reason string) error {
	f, err := Decode(data)
	if err != nil {
		return err
	}
	r, err := f.resolve()
	if err != nil {
		return err
	}
	st := m.Store
	existing, err := st.Profiles(ctx)
	if err != nil {
		return err
	}
	byID := map[int64]config.Profile{}
	bySlug := map[string]config.Profile{}
	for _, p := range existing {
		byID[p.ID] = p
		bySlug[p.Slug] = p
	}
	// 対応するDBのプロファイルを決める（idが一致するもの、なければslugが一致するもの）
	targets := make([]int64, len(r.profiles))
	used := map[int64]bool{}
	for i, p := range r.profiles {
		if old, ok := byID[p.ID]; ok && p.ID != 0 {
			targets[i] = old.ID
		} else if old, ok := bySlug[p.Slug]; ok && !used[old.ID] {
			targets[i] = old.ID
		}
		if targets[i] != 0 {
			used[targets[i]] = true
		}
	}
	var removed []config.Profile
	for _, p := range existing {
		if !used[p.ID] {
			removed = append(removed, p)
		}
	}
	// プロファイルを消すとページ・履歴も消えるので、先にDB全体を退避しておく
	if len(removed) > 0 {
		backup := filepath.Join(filepath.Dir(m.Path), "backups",
			fmt.Sprintf("mekiki-%s.db", time.Now().Format("20060102-150405")))
		if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
			return err
		}
		if err := st.BackupTo(ctx, backup); err != nil {
			return fmt.Errorf("削除前のバックアップに失敗: %w", err)
		}
		var names []string
		for _, p := range removed {
			names = append(names, p.Slug)
		}
		slog.Warn("設定ファイルにないプロファイルを削除します", "reason", reason,
			"profiles", strings.Join(names, ","), "backup", backup)
	}

	if err := st.SetSettings(ctx, r.settings.Map()); err != nil {
		return err
	}
	for _, p := range removed {
		if err := st.DeleteProfile(ctx, p.ID); err != nil {
			return err
		}
	}
	// slugの入れ替え（a⇔b）で一意制約に当たらないよう、更新するものは一旦仮のslugにする
	for i, p := range r.profiles {
		if targets[i] != 0 && byID[targets[i]].Slug != p.Slug {
			tmp := byID[targets[i]]
			tmp.Slug = fmt.Sprintf("tmp-%d-%d", tmp.ID, time.Now().UnixNano()%100000)
			if err := st.UpdateProfile(ctx, tmp); err != nil {
				return err
			}
		}
	}
	order := make([]int64, len(r.profiles))
	for i, p := range r.profiles {
		if targets[i] != 0 {
			p.ID = targets[i]
			if err := st.UpdateProfile(ctx, p); err != nil {
				return err
			}
		} else {
			id, err := st.CreateProfile(ctx, p)
			if err != nil {
				return err
			}
			p.ID = id
		}
		order[i] = p.ID
		if err := st.SyncFeeds(ctx, p.ID, r.feeds[i]); err != nil {
			return err
		}
	}
	return st.SetProfileOrder(ctx, order)
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
