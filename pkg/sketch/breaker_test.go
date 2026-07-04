package sketch

import (
	"testing"
	"time"
)

func TestBreakerAcceptsUpToMax(t *testing.T) {
	b := NewBreaker(3, time.Minute)
	now := time.Unix(0, 0)
	for i := uint64(0); i < 3; i++ {
		if !b.ObserveBirth(i, now) {
			t.Fatalf("birth %d: expected accept", i)
		}
	}
	if b.Tripped() {
		t.Fatal("should not be tripped after exactly max births")
	}
}

// TestBreakerTripsOverMax is the acceptance test: "birth rate > max -> trip
// event, flagged in Tracker" (the Breaker half; Tracker integration is
// covered in TestTrackerBreakerFlagged).
func TestBreakerTripsOverMax(t *testing.T) {
	b := NewBreaker(3, time.Minute)
	now := time.Unix(0, 0)
	for i := uint64(0); i < 3; i++ {
		if !b.ObserveBirth(i, now) {
			t.Fatalf("birth %d: expected accept", i)
		}
	}
	if b.ObserveBirth(99, now) {
		t.Fatal("4th birth within the window should be rejected")
	}
	if !b.Tripped() {
		t.Fatal("expected Tripped() == true after exceeding max births")
	}
}

func TestBreakerRollingWindowRecovers(t *testing.T) {
	b := NewBreaker(2, time.Minute)
	t0 := time.Unix(0, 0)
	if !b.ObserveBirth(1, t0) || !b.ObserveBirth(2, t0) {
		t.Fatal("expected first two births to be accepted")
	}
	// Past the window: the earlier births should have rolled off, so a
	// fresh birth is accepted again without needing an explicit Reset.
	later := t0.Add(2 * time.Minute)
	if !b.ObserveBirth(3, later) {
		t.Fatal("expected birth to be accepted once earlier births rolled out of the window")
	}
}

func TestBreakerReset(t *testing.T) {
	b := NewBreaker(1, time.Minute)
	now := time.Unix(0, 0)
	b.ObserveBirth(1, now)
	if b.ObserveBirth(2, now) {
		t.Fatal("expected trip on 2nd birth with max=1")
	}
	if !b.Tripped() {
		t.Fatal("expected tripped before Reset")
	}
	b.Reset()
	if b.Tripped() {
		t.Fatal("expected Tripped() == false after Reset")
	}
	if !b.ObserveBirth(3, now) {
		t.Fatal("expected birth accepted immediately after Reset")
	}
}
