package internet

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
)

const (
	systemDialTimeout = 5 * time.Second
	dialStalledAfter  = time.Second
	dialLogInterval   = 5 * time.Second
)

type inFlightDial struct {
	cancel context.CancelFunc
	start  time.Time
	dest   string
	slots  *chan struct{}
	stolen bool
}

var (
	systemDialSlots atomic.Pointer[chan struct{}]

	dialsMu       sync.Mutex
	inFlightDials = map[uint64]*inFlightDial{}
	lastDialID    uint64

	logMu         sync.Mutex
	evictedDials  int
	evictedOldest time.Duration
	evictedLast   string
	lastEvictWarn time.Time
	lastCensus    time.Time
)

func SetMaxConcurrentSystemDials(n int) {
	if n <= 0 {
		systemDialSlots.Store(nil)
		errors.LogInfo(context.Background(), "outbound system dials are unlimited")
		return
	}
	slots := make(chan struct{}, n)
	systemDialSlots.Store(&slots)
	errors.LogInfo(context.Background(), "outbound system dials limited to ", n, " at a time")
}

func ResetSystemDials() int {
	dialsMu.Lock()
	live := make([]*inFlightDial, 0, len(inFlightDials))
	for _, dial := range inFlightDials {
		live = append(live, dial)
	}
	dialsMu.Unlock()

	for _, dial := range live {
		dial.cancel()
	}
	if len(live) > 0 {
		errors.LogWarning(context.Background(), "system dials reset, cancelling ", len(live), " in flight")
	}
	return len(live)
}

func acquireSystemDial(ctx context.Context, dest string) (context.Context, func(), error) {
	slots := systemDialSlots.Load()
	if slots == nil {
		return registerSystemDial(ctx, dest, nil)
	}

	select {
	case *slots <- struct{}{}:
		return registerSystemDial(ctx, dest, slots)
	default:
	}

	censusSystemDials(ctx)
	if evictOldestSystemDial(slots) {
		return registerSystemDial(ctx, dest, slots)
	}

	errors.LogError(ctx, "no dial slot for ", dest, " and nothing in flight to evict")
	return nil, nil, errors.New("no dial slot available for ", dest)
}

func registerSystemDial(ctx context.Context, dest string, slots *chan struct{}) (context.Context, func(), error) {
	dialCtx, cancel := context.WithCancel(ctx)

	dialsMu.Lock()
	lastDialID++
	id := lastDialID
	dial := &inFlightDial{cancel: cancel, start: time.Now(), dest: dest, slots: slots}
	inFlightDials[id] = dial
	dialsMu.Unlock()

	release := func() {
		dialsMu.Lock()
		delete(inFlightDials, id)
		stolen := dial.stolen
		dialsMu.Unlock()
		cancel()
		if slots != nil && !stolen {
			<-*slots
		}
	}
	return dialCtx, release, nil
}

func evictOldestSystemDial(slots *chan struct{}) bool {
	dialsMu.Lock()
	var oldest *inFlightDial
	for _, dial := range inFlightDials {
		if dial.stolen || dial.slots != slots {
			continue
		}
		if oldest == nil || dial.start.Before(oldest.start) {
			oldest = dial
		}
	}
	if oldest == nil {
		dialsMu.Unlock()
		return false
	}
	oldest.stolen = true
	stalled, target := time.Since(oldest.start), oldest.dest
	dialsMu.Unlock()

	oldest.cancel()
	noteEviction(stalled, target)
	return true
}

func noteEviction(stalled time.Duration, dest string) {
	logMu.Lock()
	evictedDials++
	evictedLast = dest
	if stalled > evictedOldest {
		evictedOldest = stalled
	}
	count, oldest, last := evictedDials, evictedOldest, evictedLast
	if time.Since(lastEvictWarn) < dialLogInterval {
		logMu.Unlock()
		return
	}
	lastEvictWarn = time.Now()
	evictedDials, evictedOldest, evictedLast = 0, 0, ""
	logMu.Unlock()

	errors.LogWarning(context.Background(), "evicted ", count, " stalled dials in the last window, oldest waited ",
		oldest.Truncate(time.Millisecond).String(), ", last to ", last)
}

func censusSystemDials(ctx context.Context) {
	logMu.Lock()
	if time.Since(lastCensus) < dialLogInterval {
		logMu.Unlock()
		return
	}
	lastCensus = time.Now()
	logMu.Unlock()

	now := time.Now()
	perHost := make(map[string]int)
	total, stalled := 0, 0
	topHost, topCount := "", 0

	dialsMu.Lock()
	for _, dial := range inFlightDials {
		total++
		if now.Sub(dial.start) >= dialStalledAfter {
			stalled++
		}
		host, _, err := net.SplitHostPort(dial.dest)
		if err != nil {
			host = dial.dest
		}
		perHost[host]++
		if perHost[host] > topCount {
			topHost, topCount = host, perHost[host]
		}
	}
	dialsMu.Unlock()

	errors.LogInfo(ctx, "dial slot census: ", total, " in flight, ", stalled, " of them over ",
		dialStalledAfter.String(), ", most common destination ", topHost, " with ", topCount)
}
