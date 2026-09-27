// Package analytics: per-request telemetry (Tier 1).
//
// One row per completed inference. Written asynchronously — the stream
// relay never blocks on the store. Raw rows are pruned to 14 days by
// the retention sweep. The table and insert/query methods live in
// package embeddedpb (requests.go).
package analytics

import (
	"log"
	"sync"
	"time"

	"llmgateway/internal/embeddedpb"
)

// Store: async batched writer over the ops SQLite handle.
type Store struct {
	mu      sync.Mutex
	ch      chan embeddedpb.RequestRecord
	done    chan struct{}
	stopped bool
}

func NewStore(ops *embeddedpb.App) *Store {
	if ops == nil {
		return nil
	}
	s := &Store{
		ch:   make(chan embeddedpb.RequestRecord, 512),
		done: make(chan struct{}),
	}
	go s.writerLoop(ops)
	return s
}

// Add: enqueue a record. Never blocks — drops on overflow (telemetry
// must not backpressure inference; a full 512-deep queue means the DB
// is badly backed up and losing one telemetry row is fine). Safe to call
// after Close (drops).
func (s *Store) Add(rec embeddedpb.RequestRecord) {
	if s == nil {
		return
	}
	s.mu.Lock()
	closed := s.stopped
	ch := s.ch
	s.mu.Unlock()
	if closed || ch == nil {
		return
	}
	if rec.TS.IsZero() {
		rec.TS = time.Now()
	}
	select {
	case ch <- rec:
	default:
	}
}

// Close: flush and stop the writer.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.ch)
	s.mu.Unlock()
	<-s.done
}

func (s *Store) writerLoop(ops *embeddedpb.App) {
	defer close(s.done)
	batch := make([]embeddedpb.RequestRecord, 0, 128)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case rec, ok := <-s.ch:
			if !ok {
				flush(ops, batch)
				return
			}
			batch = append(batch, rec)
			if len(batch) >= 128 {
				batch = flush(ops, batch)
			}
		case <-tick.C:
			batch = flush(ops, batch)
		}
	}
}

func flush(ops *embeddedpb.App, batch []embeddedpb.RequestRecord) []embeddedpb.RequestRecord {
	if len(batch) == 0 {
		return batch[:0]
	}
	if err := ops.InsertRequests(batch); err != nil {
		// Telemetry failures are logged, never fatal.
		log.Printf("analytics: insert %d records: %v", len(batch), err)
	}
	return batch[:0]
}
