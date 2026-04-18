package middleware

import (
	"context"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

type visitor struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64 // UnixNano
}

type rateLimiter struct {
	visitors sync.Map
	rps      rate.Limit
	burst    int
}

// NewRateLimiter returns Gin middleware that enforces a per-IP token-bucket
// rate limit. rps is the sustained requests-per-second rate and burst is the
// maximum burst size allowed. The cleanup goroutine runs until ctx is cancelled.
func NewRateLimiter(ctx context.Context, rps float64, burst int) gin.HandlerFunc {
	rl := &rateLimiter{
		rps:   rate.Limit(rps),
		burst: burst,
	}
	go rl.cleanup(ctx)
	return rl.handler
}

func (rl *rateLimiter) getVisitor(ip string) *rate.Limiter {
	now := time.Now().UnixNano()
	if val, ok := rl.visitors.Load(ip); ok {
		v := val.(*visitor)
		v.lastSeen.Store(now)
		return v.limiter
	}
	v := &visitor{limiter: rate.NewLimiter(rl.rps, rl.burst)}
	v.lastSeen.Store(now)
	if actual, loaded := rl.visitors.LoadOrStore(ip, v); loaded {
		v = actual.(*visitor)
		v.lastSeen.Store(now)
	}
	return v.limiter
}

func (rl *rateLimiter) handler(c *gin.Context) {
	ip := c.ClientIP()
	limiter := rl.getVisitor(ip)
	if !limiter.Allow() {
		log.Printf("rate limit exceeded: ip=%s", ip)
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}
	c.Next()
}

// cleanup removes visitors that have not been seen for 5 minutes.
func (rl *rateLimiter) cleanup(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UnixNano()
			rl.visitors.Range(func(key, value any) bool {
				v := value.(*visitor)
				if now-v.lastSeen.Load() > int64(5*time.Minute) {
					rl.visitors.Delete(key)
				}
				return true
			})
		}
	}
}
