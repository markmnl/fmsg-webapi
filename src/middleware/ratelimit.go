package middleware

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type rateLimiter struct {
	visitors sync.Map
	rps      rate.Limit
	burst    int
}

// NewRateLimiter returns Gin middleware that enforces a per-IP token-bucket
// rate limit. rps is the sustained requests-per-second rate and burst is the
// maximum burst size allowed.
func NewRateLimiter(rps float64, burst int) gin.HandlerFunc {
	rl := &rateLimiter{
		rps:   rate.Limit(rps),
		burst: burst,
	}
	go rl.cleanup()
	return rl.handler
}

func (rl *rateLimiter) getVisitor(ip string) *rate.Limiter {
	val, ok := rl.visitors.Load(ip)
	if ok {
		v := val.(*visitor)
		v.lastSeen = time.Now()
		return v.limiter
	}
	limiter := rate.NewLimiter(rl.rps, rl.burst)
	rl.visitors.Store(ip, &visitor{limiter: limiter, lastSeen: time.Now()})
	return limiter
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
func (rl *rateLimiter) cleanup() {
	for {
		time.Sleep(1 * time.Minute)
		rl.visitors.Range(func(key, value any) bool {
			v := value.(*visitor)
			if time.Since(v.lastSeen) > 5*time.Minute {
				rl.visitors.Delete(key)
			}
			return true
		})
	}
}
