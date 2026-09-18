// Package accountpool owns bounded, credential-free account selection state.
package accountpool

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrUnavailable = errors.New("account_unavailable")
	ErrContinuity  = errors.New("session_continuity_unavailable")
	ErrCapacity    = errors.New("account_pool_busy")
)

type Quota int

const (
	Unknown Quota = iota
	Usable
	Exhausted
)

type entry struct {
	gate    chan struct{}
	refs    int
	profile string
	expires time.Time
}

type Pool struct {
	mu      sync.Mutex
	entries map[string]*entry
	max     int
	ttl     time.Duration
	now     func() time.Time
	quota   map[string]quotaObservation
}

type quotaObservation struct {
	state   Quota
	expires time.Time
}

func New(max int, ttl time.Duration) *Pool {
	if max <= 0 {
		max = 1024
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &Pool{entries: make(map[string]*entry), quota: make(map[string]quotaObservation), max: max, ttl: ttl, now: time.Now}
}

// Lease serializes one session through the entire stream. No global lock spans I/O.
type Lease struct {
	p         *Pool
	key       string
	e         *entry
	explicit  bool
	attempted bool
	provider  string
	model     string
}

func (p *Pool) Acquire(ctx context.Context, provider, session string) (*Lease, error) {
	key := provider + "\x00" + session
	p.mu.Lock()
	now := p.now()
	for k, e := range p.entries {
		if e.refs == 0 && !e.expires.After(now) {
			delete(p.entries, k)
		}
	}
	e := p.entries[key]
	if e == nil {
		if len(p.entries) >= p.max {
			var oldest string
			for k, candidate := range p.entries {
				if candidate.refs == 0 && (oldest == "" || candidate.expires.Before(p.entries[oldest].expires)) {
					oldest = k
				}
			}
			if oldest == "" {
				p.mu.Unlock()
				return nil, ErrCapacity
			}
			delete(p.entries, oldest)
		}
		e = &entry{gate: make(chan struct{}, 1)}
		p.entries[key] = e
	}
	e.refs++
	p.mu.Unlock()
	select {
	case e.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-e.gate
			p.unref(e)
			return nil, err
		}
		return &Lease{p: p, key: key, e: e, provider: provider}, nil
	case <-ctx.Done():
		p.unref(e)
		return nil, ctx.Err()
	}
}

func (p *Pool) unref(e *entry) { p.mu.Lock(); e.refs--; p.mu.Unlock() }

// Select never silently switches a known session, even when its account disappears.
func (l *Lease) Select(preferred, selector string, candidates []string, history bool) (string, error) {
	l.explicit = selector != ""
	contains := func(id string) bool {
		for _, c := range candidates {
			if id == c {
				return true
			}
		}
		return false
	}
	if history && (l.e.profile == "" || (selector != "" && selector != l.e.profile)) {
		return "", ErrContinuity
	}
	id := l.e.profile
	if selector != "" {
		id = selector
	}
	if id == "" {
		id = preferred
	}
	if id == "" && len(candidates) > 0 {
		id = candidates[0]
	}
	if l.e.profile == "" && selector == "" && l.knownQuota(id) == Exhausted {
		id = ""
		for _, candidate := range candidates {
			if l.knownQuota(candidate) == Usable {
				id = candidate
				break
			}
		}
	}
	if id == "" || !contains(id) {
		return "", ErrUnavailable
	}
	l.e.profile = id
	return id, nil
}

func (l *Lease) SetModel(model string) { l.model = model }
func (l *Lease) quotaKey(profile string) string {
	return l.provider + "\x00" + l.model + "\x00" + profile
}
func (l *Lease) knownQuota(profile string) Quota {
	l.p.mu.Lock()
	defer l.p.mu.Unlock()
	q := l.p.quota[l.quotaKey(profile)]
	if !q.expires.After(l.p.now()) {
		return Unknown
	}
	return q.state
}

// Observations are model-specific and short-lived; they never evict a pinned
// session. Unknown probe results cannot create quota exhaustion evidence.
func (l *Lease) ObserveQuota(profile string, state Quota) {
	if state == Unknown {
		return
	}
	l.p.mu.Lock()
	defer l.p.mu.Unlock()
	now := l.p.now()
	for k, q := range l.p.quota {
		if !q.expires.After(now) {
			delete(l.p.quota, k)
		}
	}
	key := l.quotaKey(profile)
	if _, exists := l.p.quota[key]; !exists && len(l.p.quota) >= l.p.max*4 {
		oldest := ""
		for k, q := range l.p.quota {
			if oldest == "" || q.expires.Before(l.p.quota[oldest].expires) {
				oldest = k
			}
		}
		delete(l.p.quota, oldest)
	}
	l.p.quota[key] = quotaObservation{state: state, expires: now.Add(time.Minute)}
}

func (l *Lease) CanProbeFailover() bool { return !l.explicit && !l.attempted }

// Alternate permits a single verified switch before streaming. Pinned selectors
// and tool history never move to another signature authority.
func (l *Lease) Alternate(current Quota, candidates []string, history bool, probe func(string) Quota) string {
	if l.explicit || l.attempted || history || current != Exhausted {
		return ""
	}
	l.attempted = true
	for _, id := range candidates {
		if id != l.e.profile && probe(id) == Usable {
			return id
		}
	}
	return ""
}

// ObserveSuccess commits a successful account choice before releasing the lease.
func (l *Lease) ObserveSuccess(profile string) { l.e.profile = profile }
func (l *Lease) Release() {
	l.p.mu.Lock()
	l.e.expires = l.p.now().Add(l.p.ttl)
	l.p.mu.Unlock()
	<-l.e.gate
	l.p.unref(l.e)
}
