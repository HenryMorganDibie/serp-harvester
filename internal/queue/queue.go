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
}

// Source produces a stream of jobs. Jobs closes its returned channel once
// the source is exhausted or ctx is done.
type Source interface {
	Jobs(ctx context.Context) <-chan Job
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
