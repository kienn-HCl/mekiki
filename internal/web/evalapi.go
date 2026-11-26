package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/eval"
)

// apiEvalMaxCalls はAPIから始める eval 1回あたりの問い合わせの上限。
const apiEvalMaxCalls = 300

// actionStartEval は eval（mekiki eval と同じ）をバックグラウンドで始める。
// パラメータはフォーム値で受け取る: kind=select|summarize, profile, days, limit, backend, model, effort,
// pick, max_chars, judge, run（1 のときだけAIに送る。省略時は試算だけ）。
func (s *Server) actionStartEval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	num := func(name string, def int) int {
		if n, err := strconv.Atoi(r.FormValue(name)); err == nil && n >= 0 {
			return n
		}
		return def
	}
	p, err := s.App.Store.ProfileBySlug(ctx, r.FormValue("profile"))
	if r.FormValue("profile") == "" {
		var list []config.Profile
		if list, err = s.App.Store.Profiles(ctx); err == nil && len(list) > 0 {
			p = list[0]
		}
	}
	if err != nil || p.ID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "プロファイルが見つかりません"})
		return
	}
	cfg, err := s.App.Settings(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	kind := r.FormValue("kind")
	opt := eval.Options{
		Profile:  p,
		Settings: eval.Override(cfg, r.FormValue("backend"), r.FormValue("model"), r.FormValue("effort")),
		Since:    time.Now().AddDate(0, 0, -num("days", 14)),
		Limit:    num("limit", 0),
		Pick:     num("pick", 0),
		MaxChars: num("max_chars", 0),
		Judge:    r.FormValue("judge"),
		Run:      r.FormValue("run") == "1",
		MaxCalls: apiEvalMaxCalls,
	}
	if kind == "summarize" && opt.Limit == 0 {
		opt.Limit = 20
	}
	job, err := s.evalJobs.Start(kind, opt)
	switch {
	case errors.Is(err, eval.ErrJobBusy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusAccepted, job)
	}
}

func (s *Server) evalJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	job, ok := s.evalJobs.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "その eval はありません（再起動で消えます）"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
