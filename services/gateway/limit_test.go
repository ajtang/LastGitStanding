package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type limitRig struct {
	t       *testing.T
	limiter *limiter
	clock   time.Time
	handler http.Handler
	release chan struct{} // holds stream requests open when set
}

func newRig(t *testing.T, header string) *limitRig {
	r := &limitRig{t: t, limiter: newLimiter(header), clock: time.Unix(1_000_000, 0)}
	r.limiter.now = func() time.Time { return r.clock }
	r.handler = r.limiter.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if r.release != nil {
			<-r.release
		}
		w.WriteHeader(http.StatusOK)
	}))
	return r
}

func (r *limitRig) do(method, path, remote string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

func (r *limitRig) newGame(remote string, headers map[string]string) int {
	return r.do(http.MethodPost, "/api/v1/games", remote, headers).Code
}

// A script gets ten games and then waits, without locking out the next person along,
// and without the waiter being stuck forever.
func TestNewGamesAreLimitedPerAddress(t *testing.T) {
	r := newRig(t, "")
	for i := range newGameBurst {
		if code := r.newGame("198.51.100.1:1000", nil); code != http.StatusOK {
			t.Fatalf("game %d: got %d", i, code)
		}
	}
	rec := r.do(http.MethodPost, "/api/v1/games", "198.51.100.1:1000", nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: got %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if code := r.newGame("198.51.100.2:1000", nil); code != http.StatusOK {
		t.Fatalf("someone else was blocked: %d", code)
	}
	// Reading your own game is unaffected by having started too many.
	if code := r.do(http.MethodGet, "/api/v1/games/abc", "198.51.100.1:1000", nil).Code; code != http.StatusOK {
		t.Fatalf("reads were blocked: %d", code)
	}
	r.clock = r.clock.Add(newGameEvery)
	if code := r.newGame("198.51.100.1:1000", nil); code != http.StatusOK {
		t.Fatalf("still blocked after waiting: %d", code)
	}
}

// If the header were believed by default, the limit would be one curl flag away from
// not existing. Behind a real proxy it has to work, or everyone shares one bucket.
func TestClientIPHeaderIsOnlyTrustedWhenConfigured(t *testing.T) {
	spoof := func(i int) map[string]string {
		return map[string]string{"CF-Connecting-IP": "203.0.113." + string(rune('0'+i%10))}
	}

	r := newRig(t, "")
	blocked := false
	for i := range newGameBurst + 1 {
		blocked = r.newGame("198.51.100.1:1000", spoof(i)) == http.StatusTooManyRequests
	}
	if !blocked {
		t.Fatal("a spoofed header got past the limit")
	}

	r = newRig(t, "CF-Connecting-IP")
	for range newGameBurst {
		r.newGame("127.0.0.1:1000", map[string]string{"CF-Connecting-IP": "203.0.113.1"})
	}
	if code := r.newGame("127.0.0.1:1000", map[string]string{"CF-Connecting-IP": "203.0.113.2"}); code != http.StatusOK {
		t.Fatalf("clients behind the proxy share a bucket: %d", code)
	}
}

func TestOpenStreamsAreCappedAndReleased(t *testing.T) {
	r := newRig(t, "")
	r.release = make(chan struct{})
	path := "/api/v1/games/abc/stream"
	done := make(chan struct{})
	for range streamsPerClient {
		go func() {
			r.do(http.MethodGet, path, "198.51.100.1:1000", nil)
			done <- struct{}{}
		}()
	}
	waitFor(t, func() bool {
		r.limiter.mu.Lock()
		defer r.limiter.mu.Unlock()
		c := r.limiter.clients["198.51.100.1"]
		return c != nil && c.streams == streamsPerClient
	})
	if code := r.do(http.MethodGet, path, "198.51.100.1:1000", nil).Code; code != http.StatusTooManyRequests {
		t.Fatalf("stream over the cap: got %d", code)
	}
	close(r.release)
	for range streamsPerClient {
		<-done
	}
	r.release = nil
	if code := r.do(http.MethodGet, path, "198.51.100.1:1000", nil).Code; code != http.StatusOK {
		t.Fatalf("closed streams weren't given back: %d", code)
	}
}

func TestHealthChecksAreNeverLimited(t *testing.T) {
	r := newRig(t, "")
	for range requestBurst * 2 {
		if code := r.do(http.MethodGet, "/healthz", "198.51.100.1:1000", nil).Code; code != http.StatusOK {
			t.Fatalf("health check limited: %d", code)
		}
	}
}

// One address per request for a while, then quiet: the table has to shrink again, or
// the limiter is the memory leak it was added to prevent.
func TestIdleClientsAreForgotten(t *testing.T) {
	r := newRig(t, "")
	r.do(http.MethodGet, "/api/v1/leaderboard", "198.51.100.1:1000", nil)
	r.do(http.MethodGet, "/api/v1/leaderboard", "198.51.100.2:1000", nil)
	r.clock = r.clock.Add(clientIdleAfter)
	r.do(http.MethodGet, "/api/v1/leaderboard", "198.51.100.3:1000", nil)
	if n := len(r.limiter.clients); n != 1 {
		t.Fatalf("%d clients remembered, want 1", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(time.Millisecond)
	}
}
