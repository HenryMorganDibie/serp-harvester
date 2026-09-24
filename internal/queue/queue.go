// Package queue provides an in-memory job queue. At 10M+ requests/day this
// interface is what would sit in front of Kafka, SQS, or Redis Streams in
// production — the worker pool only depends on the channel, not on how jobs
// got there.
package queue

// Job is a single unit of work: one query to harvest.
type Job struct {
	Query string
}

// New builds a buffered job channel and feeds it from queries, closing the
// channel once all queries have been pushed.
func New(queries []string, buffer int) <-chan Job {
	ch := make(chan Job, buffer)
	go func() {
		defer close(ch)
		for _, q := range queries {
			ch <- Job{Query: q}
		}
	}()
	return ch
}
