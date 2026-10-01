// Package obs provides a small, dependency-free metrics registry.
//
// The application runs locally, so a Prometheus client or an OpenTelemetry
// exporter would be premature. What the service actually needs is a way to
// answer "how long did the last request take, how many AI calls failed, how deep
// is the work queue" without attaching a debugger. This registry tracks those
// numbers in process memory and renders them as JSON.
package obs

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Registry stores counters, gauges and duration distributions keyed by metric
// name.
//
// Metric names encode their dimensions (for example
// "http.requests.total.status_200"), which keeps the registry lock-free on the
// write path while staying trivial to inspect.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*atomic.Int64
	gauges   map[string]*atomic.Int64
	timings  map[string]*timing
}

type timing struct {
	count      atomic.Int64
	totalNanos atomic.Int64
	maxNanos   atomic.Int64
}

// Snapshot is a point-in-time rendering of the registry.
type Snapshot struct {
	Counters map[string]int64  `json:"counters"`
	Gauges   map[string]int64  `json:"gauges"`
	Timings  map[string]Timing `json:"timings"`
}

// Timing is the summary of one observed duration distribution.
type Timing struct {
	Count int64  `json:"count"`
	Total string `json:"total"`
	Mean  string `json:"mean"`
	Max   string `json:"max"`
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{
		counters: make(map[string]*atomic.Int64),
		gauges:   make(map[string]*atomic.Int64),
		timings:  make(map[string]*timing),
	}
}

// Inc adds one to the named counter.
func (r *Registry) Inc(name string) {
	r.counter(name).Add(1)
}

// Add adds delta to the named counter.
func (r *Registry) Add(name string, delta int64) {
	r.counter(name).Add(delta)
}

// Set assigns the current value of a gauge, such as queue depth.
func (r *Registry) Set(name string, value int64) {
	r.gauge(name).Store(value)
}

// Counter reads the current value of a counter, creating it at zero if needed.
func (r *Registry) Counter(name string) int64 {
	return r.counter(name).Load()
}

// Observe records a duration for the named timer.
func (r *Registry) Observe(name string, d time.Duration) {
	t := r.timer(name)
	t.count.Add(1)
	t.totalNanos.Add(int64(d))
	for {
		max := t.maxNanos.Load()
		if int64(d) <= max || t.maxNanos.CompareAndSwap(max, int64(d)) {
			return
		}
	}
}

// Start returns a function that records the elapsed time since the call. It is
// intended for `defer r.Start(name)()`.
func (r *Registry) Start(name string) func() {
	start := time.Now()
	return func() { r.Observe(name, time.Since(start)) }
}

// Value reads a single timer, returning a zero-value Timing if unseen.
func (r *Registry) Value(name string) Timing {
	r.mu.RLock()
	t, ok := r.timings[name]
	r.mu.RUnlock()
	if !ok {
		return Timing{}
	}
	return summarize(t)
}

// Snapshot renders every metric, sorted by name for stable output.
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	counters := make(map[string]int64, len(r.counters))
	for name, c := range r.counters {
		counters[name] = c.Load()
	}
	gauges := make(map[string]int64, len(r.gauges))
	for name, g := range r.gauges {
		gauges[name] = g.Load()
	}
	timings := make(map[string]Timing, len(r.timings))
	for name, t := range r.timings {
		timings[name] = summarize(t)
	}
	r.mu.RUnlock()

	return Snapshot{
		Counters: sortedCounters(counters),
		Gauges:   sortedCounters(gauges),
		Timings:  sortedTimings(timings),
	}
}

func sortedCounters(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out[name] = in[name]
	}
	return out
}

func sortedTimings(in map[string]Timing) map[string]Timing {
	out := make(map[string]Timing, len(in))
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out[name] = in[name]
	}
	return out
}

func summarize(t *timing) Timing {
	count := t.count.Load()
	out := Timing{Count: count}
	if count == 0 {
		return out
	}
	total := t.totalNanos.Load()
	out.Total = time.Duration(total).Round(time.Microsecond).String()
	out.Mean = time.Duration(total / count).Round(time.Microsecond).String()
	out.Max = time.Duration(t.maxNanos.Load()).Round(time.Microsecond).String()
	return out
}

func (r *Registry) gauge(name string) *atomic.Int64 {
	r.mu.RLock()
	g, ok := r.gauges[name]
	r.mu.RUnlock()
	if ok {
		return g
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok = r.gauges[name]; ok {
		return g
	}
	g = &atomic.Int64{}
	r.gauges[name] = g
	return g
}

func (r *Registry) counter(name string) *atomic.Int64 {
	r.mu.RLock()
	c, ok := r.counters[name]
	r.mu.RUnlock()
	if ok {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok = r.counters[name]; ok {
		return c
	}
	c = &atomic.Int64{}
	r.counters[name] = c
	return c
}

func (r *Registry) timer(name string) *timing {
	r.mu.RLock()
	t, ok := r.timings[name]
	r.mu.RUnlock()
	if ok {
		return t
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok = r.timings[name]; ok {
		return t
	}
	t = &timing{}
	r.timings[name] = t
	return t
}
