// Package queue provides job sources for the worker pool. Source is the
// seam between "single process, in-memory channel" and "many worker
// processes across many hosts sharing one durable queue" — the worker pool
// only ever depends on the channel Source.Jobs returns, never on how jobs
// got there.
package queue

import "context"

// Job is a single unit of work: one query to harvest.
type Job struct {
	Query string

	// RunID groups jobs submitted together (e.g. one POST /jobs call, or one
	// scheduled harvest tick), so a downstream sink can report per-run
	// results. Optional: empty for ad hoc single-query runs.
	RunID string
	// Locale is a target market/language hint (e.g. "US-en"), passed
	// through to the fetcher (SerpApi-style providers use gl/hl params) and
	// stamped onto the resulting SerpResult. Optional.
	Locale string
	// Device is a target device hint (e.g. "desktop", "mobile"). Optional.
	Device string
}

// Source produces a stream of jobs. Jobs closes its returned channel once
// the source is exhausted or ctx is done.
type Source interface {
	Jobs(ctx context.Context) <-chan Job
}

// Producer submits a job onto a queue. This is the seam a job/API layer or a
// scheduler pushes through — it depends only on this interface, never on
// Fetcher or Parser, so submitting work stays separate from doing work.
// RedisStreamSource implements this today; MemorySource doesn't, since
// there's no meaningful "push one job" operation for a fixed slice.
type Producer interface {
	PushJob(ctx context.Context, job Job) error
}

// MemorySource feeds jobs from an in-memory slice. It's what a single
// process, single-run harvest uses; RedisStreamSource is the drop-in
// replacement for sharing one queue across many processes/hosts.
type MemorySource struct {
	Queries []string
	Buffer  int
}

// Jobs implements Source.
func (m *MemorySource) Jobs(ctx context.Context) <-chan Job {
	ch := make(chan Job, m.Buffer)
	go func() {
		defer close(ch)
		for _, q := range m.Queries {
			select {
			case ch <- Job{Query: q}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// New builds a buffered job channel from queries, closing the channel once
// all queries have been pushed. Equivalent to (&MemorySource{...}).Jobs.
func New(queries []string, buffer int) <-chan Job {
	return (&MemorySource{Queries: queries, Buffer: buffer}).Jobs(context.Background())
}
