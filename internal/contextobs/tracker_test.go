package contextobs

import (
	"testing"
	"time"
)

func TestTrackerCalculatesContextDeltaForStableLineage(t *testing.T) {
	a, _ := NewAnalyzer([]byte("tracker-test-key"), 0)
	tracker := NewTracker(TrackerOptions{MaxEntries: 8, TTL: time.Hour})

	first := a.AnalyzeRequest([]byte(`{
		"conversation_id":"conv-1",
		"instructions":"same instructions",
		"input":[
			{"type":"message","role":"user","content":"A"},
			{"type":"message","role":"assistant","content":"B"}
		]
	}`))
	if d := tracker.CompareAndStore(first); d.Available {
		t.Fatal("first request in a lineage must not claim a predecessor")
	}

	second := a.AnalyzeRequest([]byte(`{
		"conversation_id":"conv-1",
		"instructions":"same instructions",
		"input":[
			{"type":"message","role":"user","content":"A"},
			{"type":"message","role":"assistant","content":"B"},
			{"type":"function_call_output","call_id":"c1","output":"C"}
		]
	}`))
	d := tracker.CompareAndStore(second)
	if !d.Available {
		t.Fatal("expected lineage comparison")
	}
	if d.PreviousRequestBytes != first.RequestBytes {
		t.Fatalf("previous bytes=%d want=%d", d.PreviousRequestBytes, first.RequestBytes)
	}
	if d.ReusedItemCount != 3 { // instructions + A + B
		t.Fatalf("reused item count=%d want=3", d.ReusedItemCount)
	}
	if d.ReusedContextBytes <= 0 || d.NovelContextBytes <= 0 {
		t.Fatalf("unexpected delta: %+v", d)
	}
	if got, want := d.ContextGrowthBytes, second.RequestBytes-first.RequestBytes; got != want {
		t.Fatalf("growth=%d want=%d", got, want)
	}
	if d.ContextReuseRatio <= 0 || d.ContextReuseRatio >= 1 || d.NovelContextRatio <= 0 || d.NovelContextRatio >= 1 {
		t.Fatalf("unexpected ratios: %+v", d)
	}
	if d.ContextAmplificationRatio <= 1 {
		t.Fatalf("amplification=%f want > 1", d.ContextAmplificationRatio)
	}
}

func TestTrackerResolvesPreviousResponseWithoutStableConversationID(t *testing.T) {
	a, _ := NewAnalyzer([]byte("tracker-response-key"), 0)
	tracker := NewTracker(TrackerOptions{MaxEntries: 8, TTL: time.Hour})

	first := a.AnalyzeRequest([]byte(`{"input":[{"type":"message","role":"user","content":"A"}]}`))
	tracker.CompareAndStore(first)
	responseRef := a.fingerprint("r_", []byte("resp-1"))
	tracker.BindResponse(first, responseRef)

	second := a.AnalyzeRequest([]byte(`{"previous_response_id":"resp-1","input":[{"type":"message","role":"user","content":"A"},{"type":"message","role":"user","content":"B"}]}`))
	d := tracker.CompareAndStore(second)
	if !d.Available || d.ReusedItemCount != 1 {
		t.Fatalf("expected response-linked reuse, got %+v", d)
	}
}

func TestTrackerEvictsByCapacityAndTTL(t *testing.T) {
	a, _ := NewAnalyzer([]byte("tracker-bound-key"), 0)
	tracker := NewTracker(TrackerOptions{MaxEntries: 1, TTL: time.Second})
	now := time.Unix(100, 0)
	tracker.now = func() time.Time { return now }

	one := a.AnalyzeRequest([]byte(`{"conversation_id":"one","input":[{"role":"user","content":"A"}]}`))
	two := a.AnalyzeRequest([]byte(`{"conversation_id":"two","input":[{"role":"user","content":"B"}]}`))
	tracker.CompareAndStore(one)
	tracker.CompareAndStore(two)
	if d := tracker.CompareAndStore(one); d.Available {
		t.Fatal("capacity bound should evict least-recently-used lineage")
	}

	now = now.Add(2 * time.Second)
	if d := tracker.CompareAndStore(one); d.Available {
		t.Fatal("expired lineage must not be reused")
	}
}

func TestTrackerCalculatesDeltaFromPromptCacheKeyLineage(t *testing.T) {
	a, _ := NewAnalyzer([]byte("tracker-prompt-cache-key"), 0)
	tracker := NewTracker(TrackerOptions{MaxEntries: 8, TTL: time.Hour})

	first := a.AnalyzeRequest([]byte(`{"prompt_cache_key":"session-cache-key","input":[{"type":"message","role":"user","content":"A"}]}`))
	if first.LineageSource != "prompt_cache_key" || first.LineageRef == "" {
		t.Fatalf("first lineage=%+v", first)
	}
	if d := tracker.CompareAndStore(first); d.Available {
		t.Fatal("first prompt-cache lineage request must not claim a predecessor")
	}

	second := a.AnalyzeRequest([]byte(`{"prompt_cache_key":"session-cache-key","input":[{"type":"message","role":"user","content":"A"},{"type":"message","role":"assistant","content":"B"}]}`))
	d := tracker.CompareAndStore(second)
	if !d.Available || d.ReusedItemCount != 1 || d.ContextGrowthBytes <= 0 || d.ContextReuseRatio <= 0 {
		t.Fatalf("prompt-cache lineage delta=%+v", d)
	}

	other := a.AnalyzeRequest([]byte(`{"prompt_cache_key":"different-session-key","input":[{"type":"message","role":"user","content":"A"}]}`))
	if d := tracker.CompareAndStore(other); d.Available {
		t.Fatalf("different prompt_cache_key must not share lineage: %+v", d)
	}
}
