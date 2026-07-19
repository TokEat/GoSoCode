// Package metrics provides a lightweight in-memory traffic meter: lifetime
// totals plus a rolling throughput estimate (bytes/sec) for the live "流量检测"
// view. It is process-local; aggregate across instances via the DB counters.
package metrics

import (
	"sync"
	"time"
)

const (
	window  = 10   // seconds averaged for the throughput estimate
	histLen = 3600 // per-second history kept for the live chart (1 hour)
)

type bucket struct {
	sec      int64
	up, down int64
}

type Meter struct {
	mu               sync.Mutex
	totalUp, totalDn int64
	buckets          [histLen]bucket
}

func New() *Meter { return &Meter{} }

func (m *Meter) slot(sec int64) *bucket {
	b := &m.buckets[sec%histLen]
	if b.sec != sec { // stale bucket from a previous rotation -> reset
		b.sec, b.up, b.down = sec, 0, 0
	}
	return b
}

func (m *Meter) AddUp(n int64) {
	m.mu.Lock()
	m.totalUp += n
	m.slot(time.Now().Unix()).up += n
	m.mu.Unlock()
}

func (m *Meter) AddDown(n int64) {
	m.mu.Lock()
	m.totalDn += n
	m.slot(time.Now().Unix()).down += n
	m.mu.Unlock()
}

type Snapshot struct {
	TotalUp   int64 `json:"total_up"`
	TotalDown int64 `json:"total_down"`
	UpBps     int64 `json:"up_bps"`
	DownBps   int64 `json:"down_bps"`
}

// Snapshot returns lifetime totals and current throughput averaged over the
// window (excluding the in-progress current second for stability).
func (m *Meter) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	var up, dn int64
	for i := int64(1); i <= window; i++ {
		sec := now - i
		b := &m.buckets[sec%histLen]
		if b.sec == sec {
			up += b.up
			dn += b.down
		}
	}
	return Snapshot{
		TotalUp: m.totalUp, TotalDown: m.totalDn,
		UpBps: up / window, DownBps: dn / window,
	}
}

// Point is one second of measured traffic (bytes moved during that second).
type Point struct {
	Sec  int64 `json:"t"`
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// Series returns the last n seconds of per-second traffic, oldest first,
// excluding the in-progress current second. Seconds with no traffic are
// zero-filled so the chart has a continuous x-axis.
func (m *Meter) Series(n int) []Point {
	if n > histLen {
		n = histLen
	}
	now := time.Now().Unix()
	out := make([]Point, 0, n)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := int64(n); i >= 1; i-- {
		sec := now - i
		p := Point{Sec: sec}
		b := &m.buckets[sec%histLen]
		if b.sec == sec {
			p.Up, p.Down = b.up, b.down
		}
		out = append(out, p)
	}
	return out
}
