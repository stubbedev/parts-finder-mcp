package main

import (
	"context"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// This is the retrieval cornerstone: every outbound request goes through
// doRequest, which (1) rotates a realistic browser fingerprint stable per host
// so a session looks coherent, (2) throttles per host so we never trip a
// rate-limit ban — losing a host means losing its deals — and (3) retries
// transient failures with backoff that honours Retry-After. TLS-fingerprint
// walls (eBay/Akamai) are still the renderer's job.

// fingerprints: realistic desktop browser identities. One is chosen per host
// (hashed) and kept stable for the process — flipping identity mid-session is
// itself a bot tell.
var fingerprints = []struct {
	UA         string
	AcceptLang string
	SecChUA    string
	Platform   string
}{
	{
		UA:         "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		AcceptLang: "en-US,en;q=0.9",
		SecChUA:    `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		Platform:   `"Linux"`,
	},
	{
		UA:         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		AcceptLang: "en-GB,en;q=0.9",
		SecChUA:    `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		Platform:   `"Windows"`,
	},
	{
		UA:         "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15",
		AcceptLang: "en-US,en;q=0.9",
		SecChUA:    "",
		Platform:   `"macOS"`,
	},
	{
		UA:         "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0",
		AcceptLang: "en-US,en;q=0.9",
		SecChUA:    "",
		Platform:   "",
	},
}

func fingerprintFor(host string) int {
	h := fnv.New32a()
	h.Write([]byte(host))
	return int(h.Sum32() % uint32(len(fingerprints)))
}

// applyHeaders sets a full browser header set for the request's host.
func applyHeaders(req *http.Request) {
	fp := fingerprints[fingerprintFor(req.URL.Hostname())]
	set := func(k, v string) {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	set("User-Agent", fp.UA)
	set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	set("Accept-Language", fp.AcceptLang)
	// No Accept-Encoding: the Go transport adds gzip and decompresses
	// transparently. Setting it here would disable that and hand us a
	// br/deflate body we can't decode.
	set("Sec-CH-UA", fp.SecChUA)
	set("Sec-CH-UA-Platform", fp.Platform)
	set("Upgrade-Insecure-Requests", "1")
	set("Sec-Fetch-Dest", "document")
	set("Sec-Fetch-Mode", "navigate")
	set("Sec-Fetch-Site", "none")
	set("Sec-Fetch-User", "?1")
}

// hostGate rate-limits and caps concurrency per host so we stay polite and
// unbanned. minInterval + jitter spaces requests; sem caps parallelism.
type hostGate struct {
	mu   sync.Mutex
	last time.Time
	sem  chan struct{}
}

const (
	perHostConcurrency = 2
	perHostMinInterval = 600 * time.Millisecond
)

var (
	gatesMu sync.Mutex
	gates   = map[string]*hostGate{}
)

func gateFor(host string) *hostGate {
	gatesMu.Lock()
	defer gatesMu.Unlock()
	g := gates[host]
	if g == nil {
		g = &hostGate{sem: make(chan struct{}, perHostConcurrency)}
		gates[host] = g
	}
	return g
}

func (g *hostGate) acquire(ctx context.Context) error {
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	wait := time.Until(g.last.Add(perHostMinInterval))
	if wait > 0 {
		wait += time.Duration(rand.Int64N(int64(250 * time.Millisecond))) // jitter
	}
	g.last = time.Now().Add(maxDur(0, wait))
	g.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			<-g.sem
			return ctx.Err()
		}
	}
	return nil
}

func (g *hostGate) release() { <-g.sem }

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

const maxAttempts = 4

// doRequest performs a throttled, fingerprinted, retrying GET. extra headers
// (e.g. Referer, Accept overrides) win over the defaults. The returned
// response's Body is open — caller closes it.
func doRequest(ctx context.Context, method, rawURL string, extra map[string]string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, err
		}
		applyHeaders(req)
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		host := req.URL.Hostname()
		g := gateFor(host)
		if err := g.acquire(ctx); err != nil {
			return nil, err
		}
		resp, err := httpClient.Do(req)
		g.release()
		if err != nil {
			lastErr = err
			if !sleepBackoff(ctx, attempt, 0) {
				return nil, err
			}
			continue
		}
		if isRetryable(resp.StatusCode) && attempt < maxAttempts-1 {
			ra := retryAfter(resp)
			resp.Body.Close()
			if !sleepBackoff(ctx, attempt, ra) {
				return nil, ctx.Err()
			}
			continue
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, context.DeadlineExceeded
}

func isRetryable(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// retryAfter reads a Retry-After header (seconds form) if present.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		if secs > 30 {
			secs = 30 // cap — don't stall a shopping run on a hostile header
		}
		return time.Duration(secs) * time.Second
	}
	return 0
}

// sleepBackoff waits before the next attempt. Honours an explicit Retry-After,
// else exponential backoff with jitter. Returns false if the context ended.
func sleepBackoff(ctx context.Context, attempt int, explicit time.Duration) bool {
	d := explicit
	if d == 0 {
		base := 400 * time.Millisecond
		d = base * (1 << attempt)
		d += time.Duration(rand.Int64N(int64(300 * time.Millisecond)))
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// drainClose reads and closes a body so the connection returns to the pool.
func drainClose(r io.ReadCloser) {
	io.Copy(io.Discard, io.LimitReader(r, 1<<20))
	r.Close()
}
