package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// limiter 是进程内的滑动窗口计数器。
//
// 刻意做得简单：控制面是单进程，重启即清零。它要挡的是「脚本连续试口令」，
// 不是持久化风控 —— 后者需要的基础设施远超这个项目的取向。
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLimiter() *limiter {
	return &limiter{hits: map[string][]time.Time{}}
}

// allow 报告 key 在窗口内是否还有配额，有则记一次。
func (l *limiter) allow(key string, limit int, window time.Duration) bool {
	return l.allowAt(key, limit, window, time.Now())
}

// allowAt 是 allow 的可测版本：注入当前时间，测试不必 sleep。
func (l *limiter) allowAt(key string, limit int, window time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)

	// 顺手清掉其它已经空掉的 key，否则 map 会随着被试过的邮箱无限增长
	if len(l.hits) > 1024 {
		for k, v := range l.hits {
			if len(v) == 0 {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// clientIP 取请求方地址。
//
// 默认只认 RemoteAddr。无条件读 X-Forwarded-For 会让按 IP 的限流被一行
// 请求头绕过 —— 只有确实部署在反向代理后面（trustProxy）时才读它。
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// 最左段是原始客户端，其余是各级代理
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
