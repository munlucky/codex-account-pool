package contextobs

import (
	"container/list"
	"sync"
	"time"
)

const (
	defaultTrackerEntries = 256
	defaultTrackerTTL     = time.Hour
)

type trackerSnapshot struct {
	contextBytes int64
	items        []ItemRef
}

type trackerEntry struct {
	key       string
	snapshot  trackerSnapshot
	expiresAt time.Time
}

// Tracker keeps only bounded structural metadata in process memory.
type Tracker struct {
	mu         sync.Mutex
	maxEntries int
	ttl        time.Duration
	now        func() time.Time
	entries    map[string]*list.Element
	lru        *list.List
}

func NewTracker(opts TrackerOptions) *Tracker {
	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultTrackerEntries
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = defaultTrackerTTL
	}
	return &Tracker{
		maxEntries: maxEntries,
		ttl:        ttl,
		now:        time.Now,
		entries:    make(map[string]*list.Element, maxEntries),
		lru:        list.New(),
	}
}

// CompareAndStore compares against a directly referenced prior response first,
// then against a stable lineage key.
func (t *Tracker) CompareAndStore(m RequestMetrics) DeltaMetrics {
	current := snapshotFrom(m)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.evictExpired(now)

	var previous trackerSnapshot
	var found bool
	if m.PreviousResponseRef != "" {
		previous, found = t.getLocked("response:"+m.PreviousResponseRef, now)
	}
	if !found && m.LineageRef != "" {
		previous, found = t.getLocked("lineage:"+m.LineageRef, now)
	}

	if m.LineageRef != "" {
		t.putLocked("lineage:"+m.LineageRef, current, now)
	}
	if !found {
		return DeltaMetrics{}
	}
	return compareSnapshots(previous, current)
}

// BindResponse associates an opaque response reference with the request snapshot
// so a future previous_response_id can resolve its structural predecessor.
func (t *Tracker) BindResponse(m RequestMetrics, responseRef string) {
	if responseRef == "" {
		return
	}
	current := snapshotFrom(m)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.evictExpired(now)
	t.putLocked("response:"+responseRef, current, now)
}

func snapshotFrom(m RequestMetrics) trackerSnapshot {
	return trackerSnapshot{
		contextBytes: contextSize(m),
		items:        append([]ItemRef(nil), m.ItemRefs...),
	}
}

func contextSize(m RequestMetrics) int64 {
	if m.ContextBytes > 0 {
		return m.ContextBytes
	}
	return m.RequestBytes
}

func compareSnapshots(previous, current trackerSnapshot) DeltaMetrics {
	counts := make(map[string]int, len(previous.items))
	sizes := make(map[string]int64, len(previous.items))
	for _, item := range previous.items {
		counts[item.Ref]++
		sizes[item.Ref] = item.Size
	}

	var reusedBytes int64
	reusedCount := 0
	for _, item := range current.items {
		if counts[item.Ref] <= 0 {
			continue
		}
		counts[item.Ref]--
		reusedCount++
		if item.Size > 0 {
			reusedBytes += item.Size
		} else {
			reusedBytes += sizes[item.Ref]
		}
	}
	if reusedBytes > current.contextBytes {
		reusedBytes = current.contextBytes
	}
	novel := current.contextBytes - reusedBytes
	if novel < 0 {
		novel = 0
	}

	d := DeltaMetrics{
		Available:            true,
		PreviousRequestBytes: previous.contextBytes,
		PreviousContextBytes: previous.contextBytes,
		ContextGrowthBytes:   current.contextBytes - previous.contextBytes,
		ReusedItemCount:      reusedCount,
		ReusedContextBytes:   reusedBytes,
		NovelContextBytes:    novel,
	}
	if current.contextBytes > 0 {
		d.ContextReuseRatio = float64(reusedBytes) / float64(current.contextBytes)
		d.NovelContextRatio = float64(novel) / float64(current.contextBytes)
	}
	denominator := novel
	if denominator < 1 {
		denominator = 1
	}
	if current.contextBytes > 0 {
		d.ContextAmplificationRatio = float64(current.contextBytes) / float64(denominator)
	}
	return d
}

func (t *Tracker) getLocked(key string, now time.Time) (trackerSnapshot, bool) {
	el, ok := t.entries[key]
	if !ok {
		return trackerSnapshot{}, false
	}
	entry := el.Value.(*trackerEntry)
	if !entry.expiresAt.After(now) {
		t.removeLocked(el)
		return trackerSnapshot{}, false
	}
	t.lru.MoveToFront(el)
	return entry.snapshot, true
}

func (t *Tracker) putLocked(key string, snapshot trackerSnapshot, now time.Time) {
	if el, ok := t.entries[key]; ok {
		entry := el.Value.(*trackerEntry)
		entry.snapshot = snapshot
		entry.expiresAt = now.Add(t.ttl)
		t.lru.MoveToFront(el)
		return
	}
	entry := &trackerEntry{key: key, snapshot: snapshot, expiresAt: now.Add(t.ttl)}
	el := t.lru.PushFront(entry)
	t.entries[key] = el
	for len(t.entries) > t.maxEntries {
		t.removeLocked(t.lru.Back())
	}
}

func (t *Tracker) evictExpired(now time.Time) {
	for el := t.lru.Back(); el != nil; {
		prev := el.Prev()
		entry := el.Value.(*trackerEntry)
		if !entry.expiresAt.After(now) {
			t.removeLocked(el)
		}
		el = prev
	}
}

func (t *Tracker) removeLocked(el *list.Element) {
	if el == nil {
		return
	}
	entry := el.Value.(*trackerEntry)
	delete(t.entries, entry.key)
	t.lru.Remove(el)
}
