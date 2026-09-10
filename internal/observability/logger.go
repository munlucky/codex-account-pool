package observability

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultQueueSize = 1024
	DefaultMaxBytes  = int64(20 << 20)
	DefaultMaxFiles  = 5
	DefaultDailyDays = 30
)

type LoggerConfig struct {
	Out            io.Writer
	FilePath       string
	DailyDir       string
	ServiceVersion string
	ServiceCommit  string
	QueueSize      int
	MaxBytes       int64
	MaxFiles       int
	DailyDays      int
	Now            func() time.Time
}

type Logger struct {
	out            io.Writer
	file           *rotatingWriter
	daily          *dailyStore
	serviceVersion string
	serviceCommit  string
	now            func() time.Time
	queue          chan Event
	dropped        atomic.Uint64
	wg             sync.WaitGroup
	closeOnce      sync.Once
}

func NewLogger(cfg LoggerConfig) (*Logger, error) {
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = DefaultMaxFiles
	}
	if cfg.DailyDays <= 0 {
		cfg.DailyDays = DefaultDailyDays
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ServiceVersion == "" {
		cfg.ServiceVersion = "unknown"
	}
	if cfg.ServiceCommit == "" {
		cfg.ServiceCommit = "unknown"
	}

	var file *rotatingWriter
	var err error
	if cfg.FilePath != "" {
		file, err = newRotatingWriter(cfg.FilePath, cfg.MaxBytes, cfg.MaxFiles)
		if err != nil {
			return nil, err
		}
	}
	var daily *dailyStore
	if cfg.DailyDir != "" {
		daily, err = newDailyStore(cfg.DailyDir, cfg.DailyDays, cfg.Now)
		if err != nil {
			if file != nil {
				_ = file.Close()
			}
			return nil, err
		}
	}
	l := &Logger{
		out: cfg.Out, file: file, daily: daily,
		serviceVersion: cfg.ServiceVersion, serviceCommit: cfg.ServiceCommit,
		now: cfg.Now, queue: make(chan Event, cfg.QueueSize),
	}
	l.wg.Add(1)
	go l.run()
	return l, nil
}

func (l *Logger) Emit(event Event) {
	if l == nil {
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = l.now().UTC()
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	if event.SchemaVersion == 0 {
		event.SchemaVersion = SchemaVersion
	}
	if event.ServiceVersion == "" {
		event.ServiceVersion = l.serviceVersion
	}
	if event.ServiceCommit == "" {
		event.ServiceCommit = l.serviceCommit
	}
	select {
	case l.queue <- event:
	default:
		l.dropped.Add(1)
	}
}

func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() { close(l.queue) })
	l.wg.Wait()
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

func (l *Logger) run() {
	defer l.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-l.queue:
			if !ok {
				l.flushDropped(l.now().UTC())
				if l.daily != nil {
					_ = l.daily.Flush()
				}
				return
			}
			l.flushDropped(event.Timestamp)
			l.write(event)
		case <-ticker.C:
			if l.daily != nil {
				_ = l.daily.Flush()
			}
		}
	}
}

func (l *Logger) flushDropped(ts time.Time) {
	count := l.dropped.Swap(0)
	if count == 0 {
		return
	}
	l.write(Event{
		Timestamp: ts.UTC(), SchemaVersion: SchemaVersion, EventType: EventLoggerHealth,
		ServiceVersion: l.serviceVersion, ServiceCommit: l.serviceCommit, DroppedEvents: count,
	})
}

func (l *Logger) write(event Event) {
	line, err := json.Marshal(event)
	if err != nil {
		return
	}
	line = append(line, '\n')
	_, _ = l.out.Write(line)
	if l.file != nil {
		_, _ = l.file.Write(line)
	}
	if l.daily != nil {
		_ = l.daily.Record(event)
	}
}

type rotatingWriter struct {
	path     string
	maxBytes int64
	maxFiles int
	file     *os.File
	size     int64
}

func newRotatingWriter(path string, maxBytes int64, maxFiles int) (*rotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create observability log directory: %w", err)
	}
	w := &rotatingWriter{path: path, maxBytes: maxBytes, maxFiles: maxFiles}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open observability log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat observability log: %w", err)
	}
	w.file = f
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	if w.file == nil {
		return 0, os.ErrInvalid
	}
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	for i := w.maxFiles - 1; i >= 1; i-- {
		dst := w.path + "." + strconv.Itoa(i)
		if i == w.maxFiles-1 {
			_ = os.Remove(dst)
		}
		if i > 1 {
			src := w.path + "." + strconv.Itoa(i-1)
			if _, err := os.Stat(src); err == nil {
				_ = os.Rename(src, dst)
			}
		}
	}
	if w.maxFiles > 1 {
		if _, err := os.Stat(w.path); err == nil {
			_ = os.Rename(w.path, w.path+".1")
		}
	} else {
		_ = os.Remove(w.path)
	}
	return w.open()
}

func (w *rotatingWriter) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

type histogram struct {
	Bounds []float64 `json:"bounds_ms"`
	Counts []uint64  `json:"counts"`
	Count  uint64    `json:"count"`
	Sum    float64   `json:"sum_ms"`
}

func newHistogram() histogram {
	bounds := []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000, 300000}
	return histogram{Bounds: bounds, Counts: make([]uint64, len(bounds)+1)}
}

func (h *histogram) Add(value float64) {
	if value < 0 {
		return
	}
	if len(h.Bounds) == 0 || len(h.Counts) != len(h.Bounds)+1 {
		*h = newHistogram()
	}
	idx := sort.SearchFloat64s(h.Bounds, value)
	h.Counts[idx]++
	h.Count++
	h.Sum += value
}

type dailySummary struct {
	Date               string               `json:"date"`
	SchemaVersion      int                  `json:"schema_version"`
	Requests           uint64               `json:"requests"`
	Attempts           uint64               `json:"attempts"`
	StatusCounts       map[string]uint64    `json:"status_counts"`
	RouteCounts        map[string]uint64    `json:"route_counts"`
	ErrorCategories    map[string]uint64    `json:"error_categories"`
	Outcomes           map[string]uint64    `json:"outcomes"`
	SemanticOutcomes   map[string]uint64    `json:"semantic_outcomes"`
	TransportFallbacks uint64               `json:"transport_fallbacks"`
	DroppedEvents      uint64               `json:"dropped_events"`
	Latency            map[string]histogram `json:"latency"`
}

type dailyStore struct {
	dir      string
	keepDays int
	now      func() time.Time
	date     string
	summary  dailySummary
	dirty    bool
}

func newDailyStore(dir string, keepDays int, now func() time.Time) (*dailyStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create daily observability directory: %w", err)
	}
	d := &dailyStore{dir: dir, keepDays: keepDays, now: now}
	_ = d.cleanup()
	return d, nil
}

func (d *dailyStore) Record(event Event) error {
	if event.EventType != EventRequestEnd && event.EventType != EventUpstreamAttempt && event.EventType != EventTransportFallback && event.EventType != EventLoggerHealth {
		return nil
	}
	date := event.Timestamp.UTC().Format("2006-01-02")
	if err := d.ensureDate(date); err != nil {
		return err
	}
	s := &d.summary
	switch event.EventType {
	case EventRequestEnd:
		s.Requests++
		s.StatusCounts[strconv.Itoa(event.StatusCode)]++
		s.RouteCounts[event.RouteTemplate]++
		if event.ErrorCategory != "" && event.ErrorCategory != "none" {
			s.ErrorCategories[event.ErrorCategory]++
		}
		if event.Outcome != "" {
			s.Outcomes[event.Outcome]++
		}
		if event.SemanticOutcome != "" {
			s.SemanticOutcomes[event.SemanticOutcome]++
		}
		if event.GatewayTotalMS >= 0 {
			h := s.Latency["gateway_total_ms"]
			h.Add(event.GatewayTotalMS)
			s.Latency["gateway_total_ms"] = h
		}
		if event.AuthMS > 0 {
			h := s.Latency["auth_ms"]
			h.Add(event.AuthMS)
			s.Latency["auth_ms"] = h
		}
		if event.FirstBodyMS > 0 {
			h := s.Latency["first_body_ms"]
			h.Add(event.FirstBodyMS)
			s.Latency["first_body_ms"] = h
		}
	case EventUpstreamAttempt:
		s.Attempts++
		if event.UpstreamHeadersMS > 0 {
			h := s.Latency["upstream_headers_ms"]
			h.Add(event.UpstreamHeadersMS)
			s.Latency["upstream_headers_ms"] = h
		}
	case EventTransportFallback:
		s.TransportFallbacks++
	case EventLoggerHealth:
		s.DroppedEvents += event.DroppedEvents
	}
	d.dirty = true
	return nil
}

func (d *dailyStore) ensureDate(date string) error {
	if d.date == date {
		return nil
	}
	if err := d.Flush(); err != nil {
		return err
	}
	d.date = date
	d.summary = newDailySummary(date)
	path := filepath.Join(d.dir, date+".json")
	if data, err := os.ReadFile(path); err == nil {
		var existing dailySummary
		if json.Unmarshal(data, &existing) == nil && existing.Date == date && existing.SchemaVersion == SchemaVersion {
			d.summary = existing
			normalizeDailySummary(&d.summary)
		}
	}
	d.dirty = false
	return d.cleanup()
}

func newDailySummary(date string) dailySummary {
	s := dailySummary{Date: date, SchemaVersion: SchemaVersion}
	normalizeDailySummary(&s)
	return s
}

func normalizeDailySummary(s *dailySummary) {
	if s.StatusCounts == nil {
		s.StatusCounts = map[string]uint64{}
	}
	if s.RouteCounts == nil {
		s.RouteCounts = map[string]uint64{}
	}
	if s.ErrorCategories == nil {
		s.ErrorCategories = map[string]uint64{}
	}
	if s.Outcomes == nil {
		s.Outcomes = map[string]uint64{}
	}
	if s.SemanticOutcomes == nil {
		s.SemanticOutcomes = map[string]uint64{}
	}
	if s.Latency == nil {
		s.Latency = map[string]histogram{}
	}
}

func (d *dailyStore) Flush() error {
	if d == nil || !d.dirty || d.date == "" {
		return nil
	}
	data, err := json.MarshalIndent(d.summary, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(d.dir, d.date+".json")
	tmp, err := os.CreateTemp(d.dir, ".daily-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0o600)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	d.dirty = false
	return nil
}

func (d *dailyStore) cleanup() error {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return err
	}
	cutoff := d.now().UTC().AddDate(0, 0, -d.keepDays)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		date, err := time.Parse("2006-01-02", entry.Name()[:len(entry.Name())-len(filepath.Ext(entry.Name()))])
		if err == nil && date.Before(time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)) {
			_ = os.Remove(filepath.Join(d.dir, entry.Name()))
		}
	}
	return nil
}
