package eval

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Jobs は eval をバックグラウンドで1つずつ走らせ、結果を直近の数件だけメモリに持つ（APIから使う）。
type Jobs struct {
	Runner *Runner

	mu      sync.Mutex
	running bool
	nextID  int
	jobs    []*Job
}

const keepJobs = 20

var ErrJobBusy = errors.New("別の eval を実行中です")

type Job struct {
	ID          int       `json:"id"`
	Kind        string    `json:"kind"` // select | summarize
	Run         bool      `json:"run"`
	Status      string    `json:"status"` // running | done | failed
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
	Output      string    `json:"output"`           // 集計（CLIの標準出力と同じ）
	Report      string    `json:"report,omitempty"` // summarize: 本番とやり直しの要約を並べたMarkdown
	out, report syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Start は eval を開始して、その Job を返す。実行中のものがあれば ErrJobBusy。
func (j *Jobs) Start(kind string, opt Options) (*Job, error) {
	if kind != "select" && kind != "summarize" {
		return nil, errors.New("kind は select か summarize です")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return nil, ErrJobBusy
	}
	j.running = true
	j.nextID++
	job := &Job{ID: j.nextID, Kind: kind, Run: opt.Run, Status: "running", StartedAt: time.Now()}
	j.jobs = append(j.jobs, job)
	if len(j.jobs) > keepJobs {
		j.jobs = j.jobs[len(j.jobs)-keepJobs:]
	}
	opt.Out = &job.out
	go func() {
		var err error
		if kind == "select" {
			_, err = j.Runner.Select(context.Background(), opt)
		} else {
			_, err = j.Runner.Summarize(context.Background(), opt, &job.report)
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		job.FinishedAt = time.Now()
		job.Status = "done"
		if err != nil {
			job.Status, job.Error = "failed", err.Error()
			slog.Warn("eval 失敗", "id", job.ID, "kind", kind, "err", err)
		}
		j.running = false
	}()
	return job.snapshot(), nil
}

// Get は Job の現在の状態（実行中なら途中までの出力）を返す。
func (j *Jobs) Get(id int) (*Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, job := range j.jobs {
		if job.ID == id {
			return job.snapshot(), true
		}
	}
	return nil, false
}

func (job *Job) snapshot() *Job {
	return &Job{ID: job.ID, Kind: job.Kind, Run: job.Run, Status: job.Status, Error: job.Error,
		StartedAt: job.StartedAt, FinishedAt: job.FinishedAt, Output: job.out.String(), Report: job.report.String()}
}
