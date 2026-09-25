// Package store defines where finished results go. At scale this would be a
// Kafka producer or a bulk-insert sink into a warehouse; here it is a
// thread-safe JSON-Lines writer so results are easy to inspect in the demo.
package store

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/HenryMorganDibie/web-harvester/internal/model"
)

// Sink accepts finished results.
type Sink interface {
	Write(result *model.SerpResult) error
}

// PageSink accepts pages from the web crawler.
type PageSink interface {
	WritePage(page *model.Page) error
}

// JSONLSink writes one JSON object per line to an underlying writer. It
// implements both Sink and PageSink.
type JSONLSink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewJSONLSink wraps w.
func NewJSONLSink(w io.Writer) *JSONLSink {
	return &JSONLSink{w: w}
}

// Write serializes result as one JSON line.
func (s *JSONLSink) Write(result *model.SerpResult) error {
	return s.writeLine(result)
}

// WritePage serializes page as one JSON line.
func (s *JSONLSink) WritePage(page *model.Page) error {
	return s.writeLine(page)
}

func (s *JSONLSink) writeLine(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: marshal result: %w", err)
	}
	b = append(b, '\n')
	if _, err := s.w.Write(b); err != nil {
		return fmt.Errorf("store: write result: %w", err)
	}
	return nil
}
