package httpapi

import (
	"sync"
	"time"
)

// windowLimiter 是固定窗口限流器，用于登录接口防爆破。
// 只用一个 map + 定期清理，内存占用与活跃 IP 数线性相关。
type windowLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	max     int
	window  time.Duration
	lastGC  time.Time
}

type bucket struct {
	start time.Time
	count int
}

func newWindowLimiter(max int, windowSeconds int) *windowLimiter {
	return &windowLimiter{
		buckets: make(map[string]*bucket, 128),
		max:     max,
		window:  time.Duration(windowSeconds) * time.Second,
		lastGC:  time.Now(),
	}
}

// loginLimiter 用于登录接口防爆破：60 秒内每个 IP 最多 10 次。
var loginLimiter = newWindowLimiter(10, 60)

// allow 判断该 key 当前是否放行。
func (l *windowLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	// 每 10 分钟清一次过期桶，避免长期运行内存增长
	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.start) > l.window {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}

	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) > l.window {
		l.buckets[key] = &bucket{start: now, count: 1}
		return true
	}

	if b.count >= l.max {
		return false
	}
	b.count++
	return true
}

// reset 在登录成功后清空该 key 的计数。
func (l *windowLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.buckets, key)
	l.mu.Unlock()
}
