package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Zero-config headless rendering. Resolution order for a CDP endpoint:
//  1. LIGHTPANDA_URL env (externally managed lightpanda/chrome)
//  2. cached binary in ~/.cache/parts-finder/ (per release tag) — spawned on demand
//  3. auto-download of the LATEST GitHub release (sha256-verified), then spawn
//
// No PATH lookup: self-managed binaries stay on the tracked latest version.
// The spawned process lives for the MCP's lifetime and dies with it.
// fetch_content auto-escalates to rendering when a site bot-blocks plain
// HTTP (403/429), so the user never has to configure or request it.

// lightpandaAPI resolves the newest release: tag, per-platform asset URLs,
// and each asset's sha256 digest. Always tracking latest means trusting
// lightpanda's release channel — the digest (same API) still catches
// transit/CDN corruption, and a mismatching binary is discarded, never run.
const lightpandaAPI = "https://api.github.com/repos/lightpanda-io/browser/releases/latest"

type lpAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"` // "sha256:<hex>"
}

// latestLightpanda asks GitHub for the newest release and picks this
// platform's asset.
func latestLightpanda(ctx context.Context, platform string) (tag string, asset lpAsset, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lightpandaAPI, nil)
	if err != nil {
		return "", lpAsset{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", lpAsset{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", lpAsset{}, fmt.Errorf("github releases API: %s", resp.Status)
	}
	var body struct {
		TagName string    `json:"tag_name"`
		Assets  []lpAsset `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", lpAsset{}, err
	}
	want := "lightpanda-" + platform
	for _, a := range body.Assets {
		if a.Name == want {
			return body.TagName, a, nil
		}
	}
	return "", lpAsset{}, fmt.Errorf("release %s has no asset %s", body.TagName, want)
}

// Renderer POOL: concurrent fetches (shop_spec fans out, searches escalate)
// each get their own lightpanda process instead of interleaving CDP sessions
// on one — lightpanda serves a single debugger session per instance, so a
// shared process made concurrent renders corrupt each other. The pool grows
// on demand up to maxRenderers; a process lives for the MCP's lifetime.
const maxRenderers = 3

type lpProc struct {
	base string
	cmd  *exec.Cmd // nil for an external LIGHTPANDA_URL endpoint
}

var (
	lpMu    sync.Mutex
	lpProcs []*lpProc    // every process/token ever pooled
	lpIdle  chan *lpProc // checked-in renderers, buffered maxRenderers
)

// acquireRenderer checks a renderer out of the pool, growing it up to
// maxRenderers on demand and blocking when all are busy. Unhealthy checkouts
// are killed and respawned. Callers MUST releaseRenderer(p).
func acquireRenderer(ctx context.Context) (*lpProc, error) {
	lpMu.Lock()
	if lpIdle == nil {
		lpIdle = make(chan *lpProc, maxRenderers)
		if raw := os.Getenv("LIGHTPANDA_URL"); raw != "" {
			// External endpoint: it multiplexes sessions itself; the tokens
			// just cap our concurrency at the same limit as the local pool.
			for range maxRenderers {
				p := &lpProc{base: raw}
				lpProcs = append(lpProcs, p)
				lpIdle <- p
			}
		}
	}
	select {
	case p := <-lpIdle:
		lpMu.Unlock()
		return checkoutHealthy(ctx, p)
	default:
	}
	if len(lpProcs) < maxRenderers {
		bin, err := lightpandaBinary(ctx)
		if err != nil {
			lpMu.Unlock()
			return nil, err
		}
		base, cmd, err := spawnLightpanda(ctx, bin)
		if err != nil {
			lpMu.Unlock()
			return nil, err
		}
		p := &lpProc{base: base, cmd: cmd}
		lpProcs = append(lpProcs, p)
		lpMu.Unlock()
		return p, nil
	}
	lpMu.Unlock()
	select {
	case p := <-lpIdle:
		return checkoutHealthy(ctx, p)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// checkoutHealthy verifies a checked-out renderer answers CDP; a dead local
// process is killed (alive-but-stuck must never be orphaned) and respawned in
// place. On respawn failure the token goes BACK to the pool — a dead token
// that vanished would shrink the pool forever.
func checkoutHealthy(ctx context.Context, p *lpProc) (*lpProc, error) {
	if _, err := wsFromBase(ctx, p.base); err == nil {
		return p, nil
	}
	if p.cmd == nil {
		releaseRenderer(p)
		return nil, fmt.Errorf("external renderer at %s unresponsive", p.base)
	}
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	bin, err := lightpandaBinary(ctx)
	if err != nil {
		releaseRenderer(p)
		return nil, err
	}
	base, cmd, err := spawnLightpanda(ctx, bin)
	if err != nil {
		releaseRenderer(p)
		return nil, err
	}
	p.base, p.cmd = base, cmd
	return p, nil
}

// releaseRenderer checks a renderer back in. Non-blocking and nil-safe so a
// release racing stopRenderer can never hang a fetch goroutine.
func releaseRenderer(p *lpProc) {
	lpMu.Lock()
	ch := lpIdle
	lpMu.Unlock()
	if ch != nil {
		select {
		case ch <- p:
		default:
		}
	}
}

// lightpandaBinary fetches and manages the lightpanda executable: the newest
// release, resolved live and cached per version so a new release is picked up
// on the next cold start, falling back to any cached build when the release
// API is unreachable — a slightly old renderer beats none. Deliberately no
// PATH lookup: a system binary would be whatever version happens to be
// installed, outside our update/cleanup management (LIGHTPANDA_URL covers
// running your own).
func lightpandaBinary(ctx context.Context) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheDir, "parts-finder")
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	osName := map[string]string{"linux": "linux", "darwin": "macos"}[runtime.GOOS]
	if arch == "" || osName == "" {
		return "", fmt.Errorf("no lightpanda build for %s/%s — set LIGHTPANDA_URL to a running instance", runtime.GOOS, runtime.GOARCH)
	}
	tag, asset, err := latestLightpanda(ctx, arch+"-"+osName)
	if err != nil {
		if p := newestCachedLightpanda(dir); p != "" {
			fmt.Fprintf(os.Stderr, "parts-finder: lightpanda release lookup failed (%v) — using cached %s\n", err, filepath.Base(p))
			return p, nil
		}
		return "", fmt.Errorf("resolve latest lightpanda: %w", err)
	}
	dest := filepath.Join(dir, "lightpanda-"+tag)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	if err := downloadVerified(ctx, asset, dest); err != nil {
		return "", err
	}
	cleanupOldLightpanda(dir, dest)
	return dest, nil
}

// cleanupOldLightpanda deletes superseded cached builds once a new one is
// verified in place — only after, so a failed download never leaves us with
// nothing. Best-effort.
func cleanupOldLightpanda(dir, keep string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "lightpanda-*"))
	matches = append(matches, filepath.Join(dir, "lightpanda")) // legacy unversioned name
	for _, m := range matches {
		if m != keep {
			os.Remove(m)
		}
	}
}

// newestCachedLightpanda returns the most recently downloaded cached build
// ("" if none). Mod time, not tag order — version strings don't sort.
func newestCachedLightpanda(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "lightpanda-*"))
	// Pre-latest-tracking cache used an unversioned name; still usable.
	if fi, err := os.Stat(filepath.Join(dir, "lightpanda")); err == nil && !fi.IsDir() {
		matches = append(matches, filepath.Join(dir, "lightpanda"))
	}
	best, bestAt := "", time.Time{}
	for _, m := range matches {
		if strings.HasSuffix(m, ".tmp") {
			continue
		}
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(bestAt) {
			best, bestAt = m, fi.ModTime()
		}
	}
	return best
}

// downloadVerified streams an asset to dest, checking its sha256 against the
// release digest. The binary gets EXECUTED — a mismatch is deleted, never run.
func downloadVerified(ctx context.Context, asset lpAsset, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("download lightpanda: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download lightpanda: %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	want := strings.TrimPrefix(asset.Digest, "sha256:")
	if want == "" {
		// Digest field missing from the API — proceed but say so; HTTPS is
		// then the only integrity layer.
		fmt.Fprintf(os.Stderr, "parts-finder: release API carried no digest for %s — skipping checksum\n", asset.Name)
	} else if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return fmt.Errorf("lightpanda %s checksum mismatch: got %s want %s", asset.Name, got, want)
	}
	return os.Rename(tmp, dest)
}

// spawnLightpanda starts `lightpanda serve` on a free port and waits for the
// CDP endpoint to answer.
func spawnLightpanda(ctx context.Context, bin string) (base string, cmd *exec.Cmd, err error) {
	port, err := freePort()
	if err != nil {
		return "", nil, err
	}
	cmd = exec.Command(bin, "serve", "--host", "127.0.0.1", "--port", fmt.Sprint(port))
	cmd.Stdout, cmd.Stderr = nil, nil // MCP protocol runs on our stdio — keep it clean
	dieWithParent(cmd)                // never orphan a renderer, even if we're SIGKILLed
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start lightpanda: %w", err)
	}
	// Reap on exit (no zombie) and expose "process died" to the poll loop —
	// cmd.ProcessState is only set by Wait.
	died := make(chan struct{})
	go func() {
		defer recoverLog("lightpanda-reaper")
		cmd.Wait()
		close(died)
	}()
	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	// Readiness: poll the real CDP endpoint, abort early if the process died.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-died:
			return "", nil, fmt.Errorf("lightpanda exited during startup")
		default:
		}
		if _, err := wsFromBase(ctx, base); err == nil {
			return base, cmd, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	cmd.Process.Kill()
	return "", nil, fmt.Errorf("lightpanda did not become ready on %s", base)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// stopRenderer kills every spawned lightpanda. Called when the MCP exits.
func stopRenderer() {
	lpMu.Lock()
	defer lpMu.Unlock()
	for _, p := range lpProcs {
		if p.cmd != nil && p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
	}
	lpProcs, lpIdle = nil, nil
}

// wsFromBase resolves a CDP websocket URL from an http(s) base or passes
// through ws:// URLs unchanged.
func wsFromBase(ctx context.Context, raw string) (string, error) {
	if strings.HasPrefix(raw, "ws://") || strings.HasPrefix(raw, "wss://") {
		return raw, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u := strings.TrimRight(raw, "/") + "/json/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("no webSocketDebuggerUrl at %s", u)
	}
	return v.WebSocketDebuggerURL, nil
}

// fetchRendered drives a pooled lightpanda over CDP to load a page (spawning
// or even downloading the browser on first use), waits out delayed hydration,
// gives challenge interstitials a human-like nudge, then extracts readable
// text. Safe to call concurrently — each call checks out its own renderer.
func fetchRendered(ctx context.Context, rawURL string) (title, text string, err error) {
	p, err := acquireRenderer(ctx)
	if err != nil {
		return "", "", fmt.Errorf("renderer unavailable: %w", err)
	}
	defer releaseRenderer(p)
	ws, err := wsFromBase(ctx, p.base)
	if err != nil {
		return "", "", fmt.Errorf("renderer unavailable: %w", err)
	}
	// Budget covers navigation + hydration polling + one challenge attempt.
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, ws)
	defer cancelAlloc()
	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	defer cancelTab()

	// network.Enable lets us read the cookie store afterwards (below).
	if err := chromedp.Run(tabCtx, network.Enable(), chromedp.Navigate(rawURL)); err != nil {
		return "", "", fmt.Errorf("render %s: %w", rawURL, err)
	}
	html, err := waitStableHTML(tabCtx, 10*time.Second)
	if err != nil {
		return "", "", fmt.Errorf("render %s: %w", rawURL, err)
	}
	// Same extraction as the plain fetcher (readability + table preservation):
	// rendering is an implementation detail, never a downgrade.
	t, x, exErr := extractHTML([]byte(html), rawURL, "")
	// Anti-bot challenge interstitial: do what a human does — click the
	// checkbox if there is one, then wait for the challenge to clear. If the
	// wall persists, return it anyway — fetchCached flags it as BotWall so
	// the caller knows this is a wall, not the page.
	if exErr == nil && isBotWall(t+"\n"+x) {
		if cleared, ok := tryClearBotWall(tabCtx, rawURL); ok {
			t, x, exErr = extractHTML([]byte(cleared), rawURL, "")
		}
	}
	// Harvest the browser's cookies into the shared jar so the cheap HTTP path
	// rides whatever the render earned — most importantly a WAF clearance
	// cookie (cf_clearance et al.): once lightpanda passes the wall, plain
	// fetches to this host stop hitting it, instead of re-rendering every time.
	harvestRenderCookies(tabCtx, rawURL)
	return t, x, exErr
}

// harvestRenderCookies copies the render session's cookies into httpClient's
// jar, keyed by the fetched URL's host. Best-effort — a cookie read failure
// just means the next plain fetch starts cold (today's behaviour). The jar's
// public-suffix logic drops any cookie whose domain doesn't cover the host.
func harvestRenderCookies(tabCtx context.Context, rawURL string) {
	defer recoverLog("harvestRenderCookies")
	u, err := url.Parse(rawURL)
	if err != nil || httpClient.Jar == nil {
		return
	}
	var cookies []*network.Cookie
	cctx, cancel := context.WithTimeout(tabCtx, 3*time.Second)
	defer cancel()
	if err := chromedp.Run(cctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		cookies, e = network.GetCookies().Do(ctx)
		return e
	})); err != nil || len(cookies) == 0 {
		return
	}
	var jarCookies []*http.Cookie
	for _, c := range cookies {
		jarCookies = append(jarCookies, &http.Cookie{
			Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain,
			Secure: c.Secure, HttpOnly: c.HTTPOnly,
		})
	}
	httpClient.Jar.SetCookies(u, jarCookies)
}

// waitStableHTML polls the DOM until its size stops changing between
// consecutive snapshots — the classic delayed-hydration fix: a fixed sleep is
// always either too short (partial page read as ground truth) or too long
// (every fast page pays the worst case). Returns the last snapshot when the
// budget runs out mid-hydration; the thin-text flags catch a still-empty one.
func waitStableHTML(tabCtx context.Context, budget time.Duration) (string, error) {
	deadline := time.Now().Add(budget)
	var html string
	last := -1
	for {
		if err := chromedp.Run(tabCtx,
			chromedp.Sleep(1200*time.Millisecond),
			chromedp.OuterHTML("html", &html),
		); err != nil {
			return "", err
		}
		if len(html) == last { // two consecutive identical sizes = settled
			return html, nil
		}
		last = len(html)
		if time.Now().After(deadline) {
			return html, nil
		}
	}
}

// tryClearBotWall attempts what a human does at a challenge interstitial:
// click the verification checkbox, then wait for the page to swap in. Cheap
// and legitimate — the checkbox IS the intended interaction. Best-effort by
// design: Turnstile usually sits in a cross-origin iframe this CDP session
// can't reach, and its background checks may fail a headless engine no matter
// what; the caller keeps the honest BotWall flag when this returns !ok.
func tryClearBotWall(tabCtx context.Context, rawURL string) (html string, ok bool) {
	for _, sel := range []string{
		`input[type="checkbox"]`,    // Turnstile / hCaptcha checkbox rendered in-DOM
		`#challenge-stage input`,    // Cloudflare interstitial stage
		`.ctp-checkbox-label input`, // Cloudflare turnstile label variant
		`label.cb-lb input`,         // Turnstile widget markup
	} {
		cctx, cancel := context.WithTimeout(tabCtx, 2*time.Second)
		_ = chromedp.Run(cctx, chromedp.Click(sel, chromedp.ByQuery, chromedp.NodeVisible))
		cancel()
	}
	// Challenges also clear on their own once their JS finishes — poll either
	// way, bounded.
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if err := chromedp.Run(tabCtx,
			chromedp.Sleep(2*time.Second),
			chromedp.OuterHTML("html", &html),
		); err != nil {
			return "", false
		}
		if t, x, err := extractHTML([]byte(html), rawURL, ""); err == nil &&
			len(strings.TrimSpace(x)) > 0 && !isBotWall(t+"\n"+x) {
			return html, true
		}
	}
	return "", false
}
