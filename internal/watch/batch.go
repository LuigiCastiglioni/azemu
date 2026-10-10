package watch

import (
	"sort"
	"time"
)

// batcher is the pure debounce/coalesce state machine: events in, batches out.
// It owns no timers or goroutines; the caller passes the clock in and sleeps until next().
type batcher struct {
	quiet    time.Duration
	dirty    map[string]bool // keys changed since the last batch (includes ones changed mid-flight)
	deadline time.Time       // last event + quiet
	inflight bool
}

func newBatcher(quiet time.Duration) *batcher {
	return &batcher{quiet: quiet, dirty: map[string]bool{}}
}

// add marks keys dirty and restarts the quiet period.
func (b *batcher) add(now time.Time, keys ...string) {
	if len(keys) == 0 {
		return
	}
	for _, k := range keys {
		b.dirty[k] = true
	}
	b.deadline = now.Add(b.quiet)
}

// next reports when poll can next yield a batch; false = nothing to wait for
// (idle, or a rebuild is in flight and finish() will re-arm).
func (b *batcher) next() (time.Time, bool) {
	return b.deadline, len(b.dirty) > 0 && !b.inflight
}

// poll returns the sorted batch if the quiet period elapsed and no rebuild is in flight,
// and marks one in flight.
func (b *batcher) poll(now time.Time) ([]string, bool) {
	if b.inflight || len(b.dirty) == 0 || now.Before(b.deadline) {
		return nil, false
	}
	keys := make([]string, 0, len(b.dirty))
	for k := range b.dirty {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.dirty = map[string]bool{}
	b.inflight = true
	return keys, true
}

// finish ends the in-flight rebuild; keys dirtied meanwhile stay pending for the next poll.
func (b *batcher) finish() { b.inflight = false }
