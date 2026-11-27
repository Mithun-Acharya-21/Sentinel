package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// forbiddenLabels prevents accidental registration of unbounded-cardinality or sensitive labels.
var forbiddenLabels = map[string]bool{
	"tenant_id":    true,
	"incident_id":  true,
	"job_id":       true,
	"api_key":      true,
	"key_id":       true,
	"token":        true,
	"secret":       true,
	"principal_id": true,
	"ip":           true,
	"client_ip":    true,
	"remote_addr":  true,
}

// DefaultDurationBuckets are standard latency buckets in seconds for HTTP, DB, and worker operations.
var DefaultDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Registry holds Prometheus-compatible metrics and renders text exposition format 0.0.4.
type Registry struct {
	mu         sync.RWMutex
	collectors []collector
	onScrape   []func()
}

type collector interface {
	metricName() string
	writePrometheus(w io.Writer)
}

// NewRegistry creates an empty metrics Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// OnScrape registers a callback invoked before rendering /metrics (e.g., to sample sql.DB.Stats).
func (r *Registry) OnScrape(fn func()) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onScrape = append(r.onScrape, fn)
}

// Handler returns an http.Handler serving Prometheus text exposition format 0.0.4.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.RLock()
		hooks := make([]func(), len(r.onScrape))
		copy(hooks, r.onScrape)
		cols := make([]collector, len(r.collectors))
		copy(cols, r.collectors)
		r.mu.RUnlock()

		for _, fn := range hooks {
			fn()
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		for _, c := range cols {
			c.writePrometheus(w)
		}
	})
}

func validateLabelNames(names []string) error {
	for _, n := range names {
		if forbiddenLabels[strings.ToLower(strings.TrimSpace(n))] {
			return fmt.Errorf("forbidden high-cardinality or sensitive metric label: %q", n)
		}
	}
	return nil
}

func formatLabelKey(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		val := ""
		if i < len(values) {
			val = values[i]
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(val))
		b.WriteByte('"')
	}
	return b.String()
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// CounterVec is a thread-safe Prometheus counter supporting bounded label dimensions.
type CounterVec struct {
	name       string
	help       string
	labelNames []string
	mu         sync.RWMutex
	values     map[string]float64
}

// RegisterCounter registers a new CounterVec in the Registry.
func (r *Registry) RegisterCounter(name, help string, labelNames ...string) (*CounterVec, error) {
	if err := validateLabelNames(labelNames); err != nil {
		return nil, err
	}
	c := &CounterVec{
		name:       name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
		values:     make(map[string]float64),
	}
	r.mu.Lock()
	r.collectors = append(r.collectors, c)
	r.mu.Unlock()
	return c, nil
}

// Add increments the counter for the given label values by delta (must be >= 0).
func (c *CounterVec) Add(delta float64, labelValues ...string) {
	if c == nil || delta < 0 || math.IsNaN(delta) {
		return
	}
	key := formatLabelKey(c.labelNames, labelValues)
	c.mu.Lock()
	c.values[key] += delta
	c.mu.Unlock()
}

// Inc increments the counter for the given label values by 1.
func (c *CounterVec) Inc(labelValues ...string) {
	c.Add(1, labelValues...)
}

// Value returns the current counter value for the given label values.
func (c *CounterVec) Value(labelValues ...string) float64 {
	if c == nil {
		return 0
	}
	key := formatLabelKey(c.labelNames, labelValues)
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.values[key]
}

func (c *CounterVec) metricName() string { return c.name }

func (c *CounterVec) writePrometheus(w io.Writer) {
	c.mu.RLock()
	keys := make([]string, 0, len(c.values))
	snapshot := make(map[string]float64, len(c.values))
	for k, v := range c.values {
		keys = append(keys, k)
		snapshot[k] = v
	}
	c.mu.RUnlock()

	sort.Strings(keys)
	fmt.Fprintf(w, "# HELP %s %s\n", c.name, c.help)
	fmt.Fprintf(w, "# TYPE %s counter\n", c.name)
	for _, k := range keys {
		if k == "" {
			fmt.Fprintf(w, "%s %s\n", c.name, strconv.FormatFloat(snapshot[k], 'g', -1, 64))
		} else {
			fmt.Fprintf(w, "%s{%s} %s\n", c.name, k, strconv.FormatFloat(snapshot[k], 'g', -1, 64))
		}
	}
}

// GaugeVec is a thread-safe Prometheus gauge supporting bounded label dimensions.
type GaugeVec struct {
	name       string
	help       string
	labelNames []string
	mu         sync.RWMutex
	values     map[string]float64
}

// RegisterGauge registers a new GaugeVec in the Registry.
func (r *Registry) RegisterGauge(name, help string, labelNames ...string) (*GaugeVec, error) {
	if err := validateLabelNames(labelNames); err != nil {
		return nil, err
	}
	g := &GaugeVec{
		name:       name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
		values:     make(map[string]float64),
	}
	r.mu.Lock()
	r.collectors = append(r.collectors, g)
	r.mu.Unlock()
	return g, nil
}

// Set sets the gauge to v for the given label values.
func (g *GaugeVec) Set(v float64, labelValues ...string) {
	if g == nil || math.IsNaN(v) {
		return
	}
	key := formatLabelKey(g.labelNames, labelValues)
	g.mu.Lock()
	g.values[key] = v
	g.mu.Unlock()
}

// Add adds delta to the gauge for the given label values.
func (g *GaugeVec) Add(delta float64, labelValues ...string) {
	if g == nil || math.IsNaN(delta) {
		return
	}
	key := formatLabelKey(g.labelNames, labelValues)
	g.mu.Lock()
	g.values[key] += delta
	g.mu.Unlock()
}

// Inc increments the gauge by 1.
func (g *GaugeVec) Inc(labelValues ...string) { g.Add(1, labelValues...) }

// Dec decrements the gauge by 1.
func (g *GaugeVec) Dec(labelValues ...string) { g.Add(-1, labelValues...) }

// Value returns the current gauge value.
func (g *GaugeVec) Value(labelValues ...string) float64 {
	if g == nil {
		return 0
	}
	key := formatLabelKey(g.labelNames, labelValues)
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.values[key]
}

func (g *GaugeVec) metricName() string { return g.name }

func (g *GaugeVec) writePrometheus(w io.Writer) {
	g.mu.RLock()
	keys := make([]string, 0, len(g.values))
	snapshot := make(map[string]float64, len(g.values))
	for k, v := range g.values {
		keys = append(keys, k)
		snapshot[k] = v
	}
	g.mu.RUnlock()

	sort.Strings(keys)
	fmt.Fprintf(w, "# HELP %s %s\n", g.name, g.help)
	fmt.Fprintf(w, "# TYPE %s gauge\n", g.name)
	for _, k := range keys {
		if k == "" {
			fmt.Fprintf(w, "%s %s\n", g.name, strconv.FormatFloat(snapshot[k], 'g', -1, 64))
		} else {
			fmt.Fprintf(w, "%s{%s} %s\n", g.name, k, strconv.FormatFloat(snapshot[k], 'g', -1, 64))
		}
	}
}

type histogramSeries struct {
	buckets []uint64 // cumulative per bucket boundary
	sum     float64
	count   uint64
}

// HistogramVec is a thread-safe Prometheus cumulative histogram.
type HistogramVec struct {
	name       string
	help       string
	labelNames []string
	bounds     []float64
	mu         sync.RWMutex
	series     map[string]*histogramSeries
}

// RegisterHistogram registers a new HistogramVec in the Registry.
func (r *Registry) RegisterHistogram(name, help string, bounds []float64, labelNames ...string) (*HistogramVec, error) {
	if err := validateLabelNames(labelNames); err != nil {
		return nil, err
	}
	if len(bounds) == 0 {
		bounds = DefaultDurationBuckets
	}
	sortedBounds := append([]float64(nil), bounds...)
	sort.Float64s(sortedBounds)

	h := &HistogramVec{
		name:       name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
		bounds:     sortedBounds,
		series:     make(map[string]*histogramSeries),
	}
	r.mu.Lock()
	r.collectors = append(r.collectors, h)
	r.mu.Unlock()
	return h, nil
}

// Observe records a sample value in the histogram.
func (h *HistogramVec) Observe(val float64, labelValues ...string) {
	if h == nil || math.IsNaN(val) || val < 0 {
		return
	}
	key := formatLabelKey(h.labelNames, labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[key]
	if !ok {
		s = &histogramSeries{buckets: make([]uint64, len(h.bounds))}
		h.series[key] = s
	}
	for i, bound := range h.bounds {
		if val <= bound {
			s.buckets[i]++
		}
	}
	s.sum += val
	s.count++
}

// Count returns the total number of observations for the given label values.
func (h *HistogramVec) Count(labelValues ...string) uint64 {
	if h == nil {
		return 0
	}
	key := formatLabelKey(h.labelNames, labelValues)
	h.mu.RLock()
	defer h.mu.RUnlock()
	if s, ok := h.series[key]; ok {
		return s.count
	}
	return 0
}

func (h *HistogramVec) metricName() string { return h.name }

func (h *HistogramVec) writePrometheus(w io.Writer) {
	h.mu.RLock()
	keys := make([]string, 0, len(h.series))
	snapshot := make(map[string]histogramSeries, len(h.series))
	for k, s := range h.series {
		keys = append(keys, k)
		cp := histogramSeries{
			buckets: append([]uint64(nil), s.buckets...),
			sum:     s.sum,
			count:   s.count,
		}
		snapshot[k] = cp
	}
	h.mu.RUnlock()

	sort.Strings(keys)
	fmt.Fprintf(w, "# HELP %s %s\n", h.name, h.help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", h.name)
	for _, k := range keys {
		s := snapshot[k]
		prefix := ""
		if k != "" {
			prefix = k + ","
		}
		for i, bound := range h.bounds {
			le := strconv.FormatFloat(bound, 'g', -1, 64)
			fmt.Fprintf(w, "%s_bucket{%sle=\"%s\"} %d\n", h.name, prefix, le, s.buckets[i])
		}
		fmt.Fprintf(w, "%s_bucket{%sle=\"+Inf\"} %d\n", h.name, prefix, s.count)
		if k == "" {
			fmt.Fprintf(w, "%s_sum %s\n", h.name, strconv.FormatFloat(s.sum, 'g', -1, 64))
			fmt.Fprintf(w, "%s_count %d\n", h.name, s.count)
		} else {
			fmt.Fprintf(w, "%s_sum{%s} %s\n", h.name, k, strconv.FormatFloat(s.sum, 'g', -1, 64))
			fmt.Fprintf(w, "%s_count{%s} %d\n", h.name, k, s.count)
		}
	}
}
