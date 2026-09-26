package burst

import (
	"testing"
	"time"

	"github.com/xtls/xray-core/app/observatory"
)

func statusOf(t *testing.T, r *HealthPingRTTS) *observatory.OutboundStatus {
	t.Helper()
	o := &Observer{hp: &HealthPing{Results: map[string]*HealthPingRTTS{"exit": r}}}
	result := o.createResult()
	if len(result) != 1 {
		t.Fatalf("%d statuses, want 1", len(result))
	}
	return result[0]
}

func TestOneFailureKeepsAnExitAlive(t *testing.T) {
	r := NewHealthPingResult(5, time.Hour)
	r.Put(80 * time.Millisecond)
	r.Put(90 * time.Millisecond)
	r.Put(rttFailed)
	if !statusOf(t, r).Alive {
		t.Fatal("a single failure after successes marked the exit dead")
	}
}

func TestTwoFailuresInARowMarkAnExitDead(t *testing.T) {
	r := NewHealthPingResult(5, time.Hour)
	r.Put(80 * time.Millisecond)
	r.Put(90 * time.Millisecond)
	r.Put(70 * time.Millisecond)
	r.Put(rttFailed)
	r.Put(rttFailed)
	if statusOf(t, r).Alive {
		t.Fatal("two failures in a row left the exit alive while three successes were still in the window")
	}
	r.Put(85 * time.Millisecond)
	if !statusOf(t, r).Alive {
		t.Fatal("a success after the failures did not bring the exit back")
	}
}

func TestFailuresApartDoNotAddUp(t *testing.T) {
	r := NewHealthPingResult(5, time.Hour)
	r.Put(rttFailed)
	r.Put(80 * time.Millisecond)
	r.Put(rttFailed)
	r.Put(90 * time.Millisecond)
	r.Put(rttFailed)
	if !statusOf(t, r).Alive {
		t.Fatal("three failures separated by successes marked the exit dead")
	}
}

func TestTheStreakWrapsAroundTheWindow(t *testing.T) {
	r := NewHealthPingResult(3, time.Hour)
	r.Put(80 * time.Millisecond)
	r.Put(80 * time.Millisecond)
	r.Put(rttFailed)
	r.Put(rttFailed)
	if statusOf(t, r).Alive {
		t.Fatal("two failures straddling the end of the ring were not counted as a streak")
	}
}

func TestAnExitThatNeverAnsweredIsDead(t *testing.T) {
	r := NewHealthPingResult(5, time.Hour)
	r.Put(rttFailed)
	status := statusOf(t, r)
	if status.Alive {
		t.Fatal("an exit whose only sample failed is alive")
	}
	if status.LastSeenTime != 0 {
		t.Fatalf("last seen %d for an exit that never answered, want 0", status.LastSeenTime)
	}
	if status.LastTryTime == 0 {
		t.Fatal("last try is empty after a probe")
	}
}

func TestLastSeenStaysOnTheLastSuccess(t *testing.T) {
	r := NewHealthPingResult(5, time.Hour)
	r.Put(80 * time.Millisecond)
	seen := r.lastSeen
	r.Put(rttFailed)
	if !r.lastSeen.Equal(seen) {
		t.Fatalf("a failure moved last seen from %s to %s", seen, r.lastSeen)
	}
	if r.lastTry.Before(seen) {
		t.Fatalf("last try %s is before the success at %s", r.lastTry, seen)
	}
	status := statusOf(t, r)
	if status.LastSeenTime != seen.Unix() || status.LastTryTime != r.lastTry.Unix() {
		t.Fatalf("status reports seen=%d try=%d, want %d and %d",
			status.LastSeenTime, status.LastTryTime, seen.Unix(), r.lastTry.Unix())
	}
}
