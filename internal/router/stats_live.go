// 实时会话统计：当前存活的会话与调用方。
//
// 与 llm_requests 台账互补：台账是落盘的历史聚合（分钟级、重启不丢），
// 这里是内存态的实时视图（秒级、重启清零）。语义：
//
//   - 进行中：当前正在处理的上游调用数（acquire→release 之间）；
//   - 存活会话：5 分钟滑动窗口内出现过请求的会话键（X-Session-Affinity
//     优先、X-Session-Id 回退，与选路亲和同键），去重计数；
//   - 活跃调用方：同窗口内出现过的调用方 IP，去重计数。
//
// 只计数与去重，不存任何会话键或 IP 明文——隐私边界与台账一致（台账存
// IP 是为了按来源聚合排障；这里只回答"有多少个在活跃"）。
//
// 内存上界：每条目 24 字节（时间戳），容量按峰值并发 × 窗口时长估算，
// 5000 条上限足够；达到上限时丢增量并计数，等滑动窗口自然回收。

package router

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	// liveSessionTTL 是会话/调用方的存活窗口：5 分钟内有过请求即算"存活"。
	liveSessionTTL = 5 * time.Minute
	// liveTrackerMaxEntries 是单个去重表的上限，防异常流量撑爆内存。
	liveTrackerMaxEntries = 5000
)

// liveSet 是一个按最后活跃时间过期的去重集合（会话键或调用方 IP）。
type liveSet struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	capacity int
}

func newLiveSet(capacity int) *liveSet {
	return &liveSet{lastSeen: make(map[string]time.Time, 64), capacity: capacity}
}

// touch 记录一次活跃。返回集合当前活跃条目数（顺带满足页面取数，
// 调用方忽略返回值即可）。
func (s *liveSet) touch(key string, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" {
		s.lastSeen[key] = now
	}
	// 惰性过期：map 增长到容量上限时才整表清扫，把正常路径压到 O(1)。
	if len(s.lastSeen) > s.capacity {
		s.evictExpired(now)
	}
	return len(s.lastSeen)
}

// evictExpired 清掉窗口外的条目。调用方需持锁。
func (s *liveSet) evictExpired(now time.Time) {
	cutoff := now.Add(-liveSessionTTL)
	kept := make(map[string]time.Time, len(s.lastSeen))
	for key, at := range s.lastSeen {
		if at.After(cutoff) {
			kept[key] = at
		}
	}
	s.lastSeen = kept
}

// count 返回窗口内的活跃条目数。读路径顺带回收过期条目，避免读数虚高、
// 也让窗口外的键及时释放；条目上限 5000，O(n) 巡检在页面刷新频率下可忽略。
func (s *liveSet) count(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lastSeen) == 0 {
		return 0
	}
	cutoff := now.Add(-liveSessionTTL)
	kept := make(map[string]time.Time, len(s.lastSeen))
	active := 0
	for key, at := range s.lastSeen {
		if at.After(cutoff) {
			kept[key] = at
			active++
		}
	}
	if active < len(s.lastSeen) {
		s.lastSeen = kept
	}
	return active
}

type liveTracker struct {
	sessions *liveSet
	clients  *liveSet
	inflight atomic.Int64
	now      func() time.Time
}

func newLiveTracker() *liveTracker {
	return &liveTracker{
		sessions: newLiveSet(liveTrackerMaxEntries),
		clients:  newLiveSet(liveTrackerMaxEntries),
		now:      time.Now,
	}
}

func (t *liveTracker) recordRequest(sessionKey, clientIP string) {
	now := t.now()
	t.sessions.touch(sessionKey, now)
	t.clients.touch(clientIP, now)
}

// liveTracker 返回进程内实时统计，懒初始化让测试可直接构造 Router{}。
func (r *Router) liveTracker() *liveTracker {
	r.liveOnce.Do(func() { r.live = newLiveTracker() })
	return r.live
}

func (t *liveTracker) enter() { t.inflight.Add(1) }
func (t *liveTracker) leave() { t.inflight.Add(-1) }

// LiveStats 是统计页的实时区块。
type LiveStats struct {
	Inflight       int64 `json:"inflight"`
	ActiveSessions int   `json:"active_sessions"`
	ActiveClients  int   `json:"active_clients"`
}

func (t *liveTracker) snapshot() LiveStats {
	return LiveStats{
		Inflight:       t.inflight.Load(),
		ActiveSessions: t.sessions.count(t.now()),
		ActiveClients:  t.clients.count(t.now()),
	}
}
