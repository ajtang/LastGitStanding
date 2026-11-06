package gateway

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// These are generous on purpose. The thing to stop is a script, and the likely
// false positive is a whole office behind one IP looking at the same demo.
const (
	requestsPerSecond  = 30
	requestBurst       = 120
	newGameEvery       = 2 * time.Minute // starting a game costs far more than reading one
	newGameBurst       = 10
	streamsPerClient   = 12
	clientIdleAfter    = 10 * time.Minute
	wrongPasscodeEvery = time.Minute // a passcode someone can type is short enough to guess at speed
	wrongPasscodeBurst = 5
)

type client struct {
	requests *rate.Limiter
	newGames *rate.Limiter
	guesses  *rate.Limiter
	streams  int
	seen     time.Time
}

// limiter is per-IP and per-process. Every gateway replica counts separately, which
// is fine for a limit that only has to be the right order of magnitude.
type limiter struct {
	header  string
	now     func() time.Time
	mu      sync.Mutex
	clients map[string]*client
	swept   time.Time
}

func newLimiter(header string) *limiter {
	return &limiter{header: header, now: time.Now, clients: map[string]*client{}}
}

func (l *limiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" { // the kubelet isn't abusing anybody
			next.ServeHTTP(w, r)
			return
		}
		ip := l.clientIP(r)
		now := l.now()

		l.mu.Lock()
		c := l.client(ip, now)
		allowed := c.requests.AllowN(now, 1)
		message, retryAfter := "Too many requests. Give it a second.", "1"
		if allowed && r.Method == http.MethodPost && r.URL.Path == "/api/v1/games" {
			allowed = c.newGames.AllowN(now, 1)
			message, retryAfter = "That's a lot of new games. Try again in a couple of minutes.", "120"
		}
		stream := r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream")
		if allowed && stream {
			allowed = c.streams < streamsPerClient
			message, retryAfter = "Too many open games from this address. Close a tab.", "30"
			if allowed {
				c.streams++
			}
		}
		l.mu.Unlock()

		if !allowed {
			w.Header().Set("Retry-After", retryAfter)
			http.Error(w, message, http.StatusTooManyRequests)
			return
		}
		if stream {
			defer func() {
				l.mu.Lock()
				c.streams--
				c.seen = l.now()
				l.mu.Unlock()
			}()
		}
		next.ServeHTTP(w, r)
	})
}

// guess reports whether an admin request may proceed. Only wrong passcodes spend the
// allowance, so editing a whole leaderboard is never throttled. Once it's gone even the
// right passcode is refused, or a script could tell when it had found it.
func (l *limiter) guess(r *http.Request, correct bool) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.client(l.clientIP(r), now)
	if correct {
		return c.guesses.TokensAt(now) >= 1
	}
	return c.guesses.AllowN(now, 1)
}

// clientIP only believes the configured header. Trusting X-Forwarded-For by default
// would let anyone pick a fresh IP per request and walk straight past the limits, so
// it stays off unless something in front of the gateway is known to set the header.
func (l *limiter) clientIP(r *http.Request) string {
	if l.header != "" {
		if ip := strings.TrimSpace(r.Header.Get(l.header)); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// client must be called with mu held. Sweeping here rather than on a timer means
// there's no goroutine to own, and a quiet gateway has nothing to clean up anyway.
func (l *limiter) client(ip string, now time.Time) *client {
	if now.Sub(l.swept) >= clientIdleAfter {
		l.swept = now
		for k, c := range l.clients {
			if c.streams == 0 && now.Sub(c.seen) >= clientIdleAfter {
				delete(l.clients, k)
			}
		}
	}
	c, ok := l.clients[ip]
	if !ok {
		c = &client{
			requests: rate.NewLimiter(requestsPerSecond, requestBurst),
			newGames: rate.NewLimiter(rate.Every(newGameEvery), newGameBurst),
			guesses:  rate.NewLimiter(rate.Every(wrongPasscodeEvery), wrongPasscodeBurst),
		}
		l.clients[ip] = c
	}
	c.seen = now
	return c
}
