package web

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/kienn-HCl/mekiki/internal/ai"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/store"
)

// /api/stats は稼働状況と計測値を返す（読み取り専用）。APIキー・Webhook URLは含めない。
// ?days=N で集計期間（既定30日）、?profile=slug でプロファイルを絞り込む、?runs=N で実行の件数（既定50件）。

type statsAI struct {
	Backend  string `json:"backend"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	WebFetch bool   `json:"web_fetch"`
	KeySet   bool   `json:"key_set"` // APIキー（画面か環境変数）が設定されているか
}

type statsStage struct {
	Calls      int     `json:"calls"`
	Input      int64   `json:"input_tokens"`
	Output     int64   `json:"output_tokens"`
	CacheRead  int64   `json:"cache_read_tokens"`
	CacheWrite int64   `json:"cache_write_tokens"`
	CostUSD    float64 `json:"cost_usd"`
	CostKnown  bool    `json:"cost_known"`
}

type statsDist struct {
	Count int `json:"count"`
	Avg   int `json:"avg"`
	P50   int `json:"p50"`
	P90   int `json:"p90"`
	Max   int `json:"max"`
}

type statsFeed struct {
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Category    string    `json:"category"`
	Enabled     bool      `json:"enabled"`
	Articles    int       `json:"articles"`
	Candidates  int       `json:"candidates"` // 期間内に選定に渡した数
	Picked      int       `json:"picked"`     // そのうち選ばれた数
	Failures    int       `json:"failures"`
	LastError   string    `json:"last_error,omitempty"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastItemAt  time.Time `json:"last_item_at,omitzero"` // フィードに載っている記事のうち最新の投稿日時
}

type statsRun struct {
	ID         int64                     `json:"id"`
	Date       string                    `json:"date"`
	Kind       string                    `json:"kind"`
	Trigger    string                    `json:"trigger"`
	Status     string                    `json:"status"`
	Message    string                    `json:"message"`
	StartedAt  time.Time                 `json:"started_at"`
	Seconds    int                       `json:"seconds"`
	Candidates int                       `json:"candidates"`
	Picked     int                       `json:"picked"`
	CostUSD    float64                   `json:"cost_usd"`
	CostKnown  bool                      `json:"cost_known"`
	Usage      map[string]*ai.StageUsage `json:"usage,omitempty"`
	Reaction   store.RunReaction         `json:"reaction"`
}

type statsProfile struct {
	Slug           string                 `json:"slug"`
	Name           string                 `json:"name"`
	PickCount      int                    `json:"pick_count"`
	HoursBack      int                    `json:"hours_back"`
	DigestTimes    []string               `json:"digest_times"`
	Overview       bool                   `json:"overview"`
	SelectCriteria string                 `json:"select_criteria"`
	Interests      string                 `json:"interests"`
	Summary        runSummary             `json:"summary"`
	Stages         map[string]*statsStage `json:"stages"`
	ItemStages     map[string]int         `json:"item_stages"` // 要約元ごとの掲載件数
	InputChars     statsDist              `json:"summary_input_chars"`
	Saved          map[string]int         `json:"saved"`
	Feeds          []statsFeed            `json:"feeds"`
	Runs           []statsRun             `json:"runs"`
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	days := atoiOr(q.Get("days"), metricsDays)
	keep := atoiOr(q.Get("runs"), 50)
	since := time.Now().AddDate(0, 0, -days)
	cfg, err := s.App.Settings(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	model := cfg.Model(cfg.Backend)
	if cfg.Backend == config.BackendCLI {
		model = cfg.CLIModel
	}
	out := struct {
		Version     string         `json:"version"`
		GeneratedAt time.Time      `json:"generated_at"`
		Days        int            `json:"days"`
		Status      map[string]any `json:"status"`
		AI          statsAI        `json:"ai"`
		Fetch       map[string]int `json:"fetch"`
		Timezone    string         `json:"timezone"`
		Profiles    []statsProfile `json:"profiles"`
	}{
		Version: s.Version, GeneratedAt: time.Now(), Days: days, Timezone: cfg.Timezone,
		Status: map[string]any{"fetching": s.App.Status().Fetching, "digesting": s.App.Status().Digesting},
		AI: statsAI{Backend: cfg.Backend, Model: model, Effort: cfg.Effort(cfg.Backend),
			WebFetch: cfg.UseWebFetch(cfg.Backend), KeySet: cfg.Backend == config.BackendCLI || cfg.APIKey(cfg.Backend) != ""},
		Fetch: map[string]int{"interval_minutes": cfg.FetchInterval, "articles_per_feed": cfg.ArticlesPerFeed,
			"max_failures": cfg.MaxFailures, "retention_days": cfg.RetentionDays},
	}
	profiles, err := s.App.Store.Profiles(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	for _, p := range profiles {
		if slug := q.Get("profile"); slug != "" && slug != p.Slug {
			continue
		}
		sp, err := s.profileStats(r, p, days, keep, since)
		if err != nil {
			serverError(w, err)
			return
		}
		out.Profiles = append(out.Profiles, sp)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func (s *Server) profileStats(r *http.Request, p config.Profile, days, keep int, since time.Time) (statsProfile, error) {
	ctx := r.Context()
	st := s.App.Store
	sp := statsProfile{Slug: p.Slug, Name: p.Name, PickCount: p.PickCount, HoursBack: p.HoursBack,
		DigestTimes: p.DigestTimes, Overview: p.Overview, SelectCriteria: p.SelectCriteria, Interests: p.UserMemory,
		Stages: map[string]*statsStage{}}
	rows, sum, err := s.runStats(ctx, p, days, 1000)
	if err != nil {
		return sp, err
	}
	sp.Summary = sum
	for _, row := range rows {
		var usage map[string]*ai.StageUsage
		_ = json.Unmarshal([]byte(row.Usage), &usage)
		if row.StartedAt.After(since) {
			for name, u := range usage {
				t := sp.Stages[name]
				if t == nil {
					t = &statsStage{CostKnown: true}
					sp.Stages[name] = t
				}
				t.Calls += u.Calls
				t.Input += u.Input
				t.Output += u.Output
				t.CacheRead += u.CacheRead
				t.CacheWrite += u.CacheCreation
				if price, ok := ai.PriceOf(u.Model); ok && u.Unknown == 0 {
					t.CostUSD += price.Cost(u.Usage)
				} else {
					t.CostKnown = false
				}
			}
		}
		if len(sp.Runs) < keep {
			sec := 0
			if !row.FinishedAt.IsZero() {
				sec = int(row.FinishedAt.Sub(row.StartedAt).Seconds())
			}
			sp.Runs = append(sp.Runs, statsRun{ID: row.ID, Date: row.Date, Kind: row.Kind, Trigger: row.Trigger,
				Status: row.Status, Message: row.Message, StartedAt: row.StartedAt, Seconds: sec,
				Candidates: row.Candidates, Picked: row.Picked, CostUSD: row.Cost, CostKnown: row.CostKnown,
				Usage: usage, Reaction: row.React})
		}
	}
	if sp.ItemStages, err = st.ItemStages(ctx, p.ID, since); err != nil {
		return sp, err
	}
	lengths, err := st.InputLengths(ctx, p.ID, since)
	if err != nil {
		return sp, err
	}
	sp.InputChars = distribution(lengths)
	if sp.Saved, err = st.SavedCounts(ctx, p.ID); err != nil {
		return sp, err
	}
	picks, err := st.FeedPicks(ctx, p.ID, since)
	if err != nil {
		return sp, err
	}
	feeds, err := st.Feeds(ctx, p.ID)
	if err != nil {
		return sp, err
	}
	for _, f := range feeds {
		pk := picks[f.Title]
		sp.Feeds = append(sp.Feeds, statsFeed{Title: f.Title, URL: f.URL, Category: f.Category, Enabled: f.Enabled,
			Articles: f.ArticleCount, Candidates: pk.Candidates, Picked: pk.Picked, Failures: f.Failures,
			LastError: f.LastError, LastSuccess: f.LastSuccess, LastItemAt: f.LastItemAt})
	}
	return sp, nil
}

// distribution は昇順に並んだ値の分布。
func distribution(sorted []int) statsDist {
	n := len(sorted)
	if n == 0 {
		return statsDist{}
	}
	if !sort.IntsAreSorted(sorted) {
		sort.Ints(sorted)
	}
	total := 0
	for _, v := range sorted {
		total += v
	}
	at := func(q float64) int { return sorted[min(n-1, int(q*float64(n)))] }
	return statsDist{Count: n, Avg: total / n, P50: at(0.5), P90: at(0.9), Max: sorted[n-1]}
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}
