package main

import (
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Rate limiter (per-IP token bucket)
// ---------------------------------------------------------------------------

type rateLimiter struct {
	mu      sync.Mutex
	clients map[string]*bucket
}

type bucket struct {
	tokens    float64
	lastCheck time.Time
}

func newRateLimiter() *rateLimiter {
	rl := &rateLimiter{clients: make(map[string]*bucket)}
	// Cleanup stale entries every 10 minutes
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rl.mu.Lock()
				now := time.Now()
				for ip, b := range rl.clients {
					if now.Sub(b.lastCheck) > 30*time.Minute {
						delete(rl.clients, ip)
					}
				}
				rl.mu.Unlock()
			case <-shutdownCh:
				return
			}
		}
	}()
	return rl
}

// allow checks if the IP is within the rate limit.
// rate = tokens per second, burst = max tokens (bucket capacity).
func (rl *rateLimiter) allow(ip string, rate float64, burst float64) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.clients[ip]
	if !ok {
		rl.clients[ip] = &bucket{tokens: burst - 1, lastCheck: time.Now()}
		return true
	}
	now := time.Now()
	elapsed := now.Sub(b.lastCheck).Seconds()
	b.lastCheck = now
	b.tokens += elapsed * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ---------------------------------------------------------------------------
// fifoSem — a FIFO-ordered concurrency semaphore for download slots.
// Slots are granted strictly in arrival order, eliminating the race where
// a later ticket wins a slot over an earlier one when multiple tickets
// become simultaneously eligible under the old poll-based approach.
// ---------------------------------------------------------------------------

type fifoWaiter struct {
	ch chan struct{} // receives exactly once when slot is granted
}

type fifoSem struct {
	mu      sync.Mutex
	slots   int // free (unoccupied) slots
	max     int
	nextSeq int32
	waiters []*fifoWaiter // strictly in arrival order — seq assigned under same lock
}

func newFifoSem(max int) *fifoSem {
	if max < 1 {
		max = 1
	}
	return &fifoSem{slots: max, max: max}
}

// enqueue registers the caller and returns a waiter and a 1-based sequence number.
// Both are assigned atomically under the lock, so the queue order is strictly
// determined by mutex-acquisition order — which for HTTP requests arriving at
// different times reliably matches arrival order. No external ticket or sort needed.
func (s *fifoSem) enqueue() (*fifoWaiter, int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSeq++
	seq := s.nextSeq
	w := &fifoWaiter{ch: make(chan struct{}, 1)}
	if s.slots > 0 {
		s.slots--
		w.ch <- struct{}{} // granted immediately
	} else {
		// Append while holding the lock — order is guaranteed by mutex acquisition order.
		s.waiters = append(s.waiters, w)
	}
	return w, seq
}

// cancel removes a waiter that has not yet been granted a slot.
// Returns true if the waiter was successfully removed (caller must NOT release).
// Returns false if the slot was already granted (caller MUST release).
func (s *fifoSem) cancel(w *fifoWaiter) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ww := range s.waiters {
		if ww == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return true
		}
	}
	return false // already granted
}

// release returns a slot, granting it immediately to the next FIFO waiter.
// The channel send happens under the lock to prevent a race where cancel()
// sees the waiter already removed but the goroutine hasn't received the
// signal yet, which could cause a double-release and slot leak.
// After a resize-down, slots are capped at max so that excess active slots
// are absorbed as they finish rather than exceeding the new limit.
func (s *fifoSem) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.waiters) > 0 {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		w.ch <- struct{}{} // safe: buffered channel, guaranteed empty
	} else if s.slots < s.max {
		s.slots++
	}
	// else: slot absorbed by a previous resize-down
}

// position returns the 1-based queue position of w, or 0 if already granted / not queued.
func (s *fifoSem) position(w *fifoWaiter) int32 {
	if w == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ww := range s.waiters {
		if ww == w {
			return int32(i + 1)
		}
	}
	return 0
}

// idle reports whether every slot is free and nobody is queued.
func (s *fifoSem) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slots >= s.max && len(s.waiters) == 0
}

// resize changes the concurrency limit live, immediately granting extra slots to queued waiters.
func (s *fifoSem) resize(newMax int) {
	if newMax < 1 {
		newMax = 1
	}
	s.mu.Lock()
	delta := newMax - s.max
	s.max = newMax
	for delta > 0 && len(s.waiters) > 0 {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		delta--
		w.ch <- struct{}{}
	}
	if delta > 0 {
		s.slots += delta
	} else if delta < 0 {
		// Shrinking: reclaim free slots (can't forcibly revoke active ones).
		reclaim := -delta
		if reclaim > s.slots {
			reclaim = s.slots
		}
		s.slots -= reclaim
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Per-IP download concurrency
// ---------------------------------------------------------------------------

// ipSemEntry tracks a per-IP semaphore plus the last time it was handed out,
// so idle entries can be pruned instead of growing without bound.
type ipSemEntry struct {
	sem      *fifoSem
	lastUsed time.Time
}

// maxIPSems caps the per-IP semaphore map. Reaching it forces an inline prune.
const maxIPSems = 10000

// ipSemTTL is how long an idle, fully-released per-IP semaphore is kept.
const ipSemTTL = 30 * time.Minute

// getIPDownloadSem returns (or creates) a per-IP FIFO semaphore that limits
// how many downloads a single IP can have actively running at once.
func getIPDownloadSem(ip string) *fifoSem {
	max := cfg().MaxDownloadsPerIP
	if max < 1 {
		max = 1
	}
	ipDlSemsMu.Lock()
	defer ipDlSemsMu.Unlock()
	if e, ok := ipDlSems[ip]; ok {
		e.lastUsed = time.Now()
		return e.sem
	}
	if len(ipDlSems) >= maxIPSems {
		pruneIPSemsLocked(0)
	}
	e := &ipSemEntry{sem: newFifoSem(max), lastUsed: time.Now()}
	ipDlSems[ip] = e
	return e.sem
}

// pruneIPSems drops per-IP semaphores that have been idle for longer than ttl
// and are not currently holding or queueing any download.
func pruneIPSems(ttl time.Duration) {
	ipDlSemsMu.Lock()
	defer ipDlSemsMu.Unlock()
	pruneIPSemsLocked(ttl)
}

func pruneIPSemsLocked(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)
	for ip, e := range ipDlSems {
		if e.lastUsed.Before(cutoff) && e.sem.idle() {
			delete(ipDlSems, ip)
		}
	}
}

// resizeIPSems resizes all existing per-IP download semaphores to the new limit.
func resizeIPSems(newMax int) {
	if newMax < 1 {
		newMax = 1
	}
	ipDlSemsMu.Lock()
	defer ipDlSemsMu.Unlock()
	for _, e := range ipDlSems {
		e.sem.resize(newMax)
	}
}
