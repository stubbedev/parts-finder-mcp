package main

import (
	"archive/tar"
	"compress/gzip"
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Zero-config headless rendering, backed by obscura — a Rust CDP browser with
// a real V8, fingerprint randomization and TLS impersonation (the "stealth"
// build). Resolution order for a CDP endpoint:
//  1. RENDERER_URL env (LIGHTPANDA_URL is still honoured as a legacy alias)
//     for an externally managed CDP browser
//  2. cached build in ~/.cache/parts-finder/ (per release tag) — spawned on demand
//  3. auto-download of the LATEST GitHub release (sha256-verified), then spawn
//
// No PATH lookup: self-managed builds stay on the tracked latest version.
// The spawned process lives for the MCP's lifetime and dies with it.
// fetch_content auto-escalates to rendering when a site bot-blocks plain
// HTTP (403/429), and a bot-walled SEARCH results page re-runs through the
// renderer too (searchViaRender) — clearing walls is what the stealth engine
// is for, and a throttled engine chain is worth 30s of rendering.
//
// lightpanda was the previous engine and is gone: obscura ships a SMALLER
// download (86MB vs 171MB), covers every platform lightpanda did, runs real
// JS instead of a partial DOM, and its stealth build is the only one of the
// two that gets past a Cloudflare fingerprint check.

// obscuraAPI resolves the newest release: tag, per-platform asset URLs, and
// each asset's sha256 digest. Always tracking latest means trusting obscura's
// release channel — the digest (same API) still catches transit/CDN
// corruption, and a mismatching archive is discarded, never unpacked.
const obscuraAPI = "https://api.github.com/repos/h4ckf0r0day/obscura/releases/latest"

// obscuraAsset names the release asset for a platform. Of the four build
// flavours obscura publishes, only "-stealth" (render + stealth features) can
// both paint a page and defeat a fingerprint wall — the others would be a
// silent downgrade of the one job this renderer has.
func obscuraAsset(platform string) string { return "obscura-" + platform + "-stealth.tar.gz" }

type ghAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"` // "sha256:<hex>"
}

// latestObscura asks GitHub for the newest release and picks this platform's
// asset.
func latestObscura(ctx context.Context, platform string) (tag string, asset ghAsset, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, obscuraAPI, nil)
	if err != nil {
		return "", ghAsset{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", ghAsset{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ghAsset{}, fmt.Errorf("github releases API: %s", resp.Status)
	}
	var body struct {
		TagName string    `json:"tag_name"`
		Assets  []ghAsset `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", ghAsset{}, err
	}
	want := obscuraAsset(platform)
	for _, a := range body.Assets {
		if a.Name == want {
			return body.TagName, a, nil
		}
	}
	return "", ghAsset{}, fmt.Errorf("release %s has no asset %s", body.TagName, want)
}

// Renderer POOL: concurrent fetches (shop_spec fans out, searches escalate)
// each get their own renderer process instead of interleaving CDP sessions on
// one. The pool grows on demand up to maxRenderers; a process lives for the
// MCP's lifetime.
const maxRenderers = 3

type rendProc struct {
	base string
	cmd  *exec.Cmd // nil for an external RENDERER_URL endpoint
}

var (
	rendMu    sync.Mutex
	rendProcs []*rendProc    // every process/token ever pooled
	rendIdle  chan *rendProc // checked-in renderers, buffered maxRenderers
)

// rendererURL is the externally managed CDP endpoint, if any. LIGHTPANDA_URL
// predates the engine swap and still works — anyone pointing us at their own
// Chrome/lightpanda keeps working without editing their config.
func rendererURL() string {
	if u := os.Getenv("RENDERER_URL"); u != "" {
		return u
	}
	return os.Getenv("LIGHTPANDA_URL")
}

// acquireRenderer checks a renderer out of the pool, growing it up to
// maxRenderers on demand and blocking when all are busy. Unhealthy checkouts
// are killed and respawned. Callers MUST releaseRenderer(p).
func acquireRenderer(ctx context.Context) (*rendProc, error) {
	rendMu.Lock()
	if rendIdle == nil {
		rendIdle = make(chan *rendProc, maxRenderers)
		if raw := rendererURL(); raw != "" {
			// External endpoint: it multiplexes sessions itself; the tokens
			// just cap our concurrency at the same limit as the local pool.
			for range maxRenderers {
				p := &rendProc{base: raw}
				rendProcs = append(rendProcs, p)
				rendIdle <- p
			}
		}
	}
	select {
	case p := <-rendIdle:
		rendMu.Unlock()
		return checkoutHealthy(ctx, p)
	default:
	}
	if len(rendProcs) < maxRenderers {
		bin, err := obscuraBinary(ctx)
		if err != nil {
			rendMu.Unlock()
			return nil, err
		}
		base, cmd, err := spawnObscura(ctx, bin)
		if err != nil {
			rendMu.Unlock()
			return nil, err
		}
		p := &rendProc{base: base, cmd: cmd}
		rendProcs = append(rendProcs, p)
		rendMu.Unlock()
		return p, nil
	}
	rendMu.Unlock()
	select {
	case p := <-rendIdle:
		return checkoutHealthy(ctx, p)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// checkoutHealthy verifies a checked-out renderer answers CDP; a dead local
// process is killed (alive-but-stuck must never be orphaned) and respawned in
// place. On respawn failure the token goes BACK to the pool — a dead token
// that vanished would shrink the pool forever.
func checkoutHealthy(ctx context.Context, p *rendProc) (*rendProc, error) {
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
	bin, err := obscuraBinary(ctx)
	if err != nil {
		releaseRenderer(p)
		return nil, err
	}
	base, cmd, err := spawnObscura(ctx, bin)
	if err != nil {
		releaseRenderer(p)
		return nil, err
	}
	p.base, p.cmd = base, cmd
	return p, nil
}

// releaseRenderer checks a renderer back in. Non-blocking and nil-safe so a
// release racing stopRenderer can never hang a fetch goroutine.
func releaseRenderer(p *rendProc) {
	rendMu.Lock()
	ch := rendIdle
	rendMu.Unlock()
	if ch != nil {
		select {
		case ch <- p:
		default:
		}
	}
}

// obscuraMu serializes the resolve/download/unpack of the managed browser.
var obscuraMu sync.Mutex

// rendererCacheDir is where the managed browser lives, so the user never
// installs anything by hand. PARTS_CACHE overrides it; otherwise the OS cache
// dir (~/.cache/parts-finder on Linux, ~/Library/Caches/… on macOS), with a
// temp dir as the last resort — a container without HOME must not cost the
// user their renderer, it just re-downloads per boot (mount the cache to
// avoid that).
func rendererCacheDir() string {
	if d := os.Getenv("PARTS_CACHE"); d != "" {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "parts-finder")
	}
	return filepath.Join(os.TempDir(), "parts-finder")
}

// prewarmRenderer downloads the managed browser in the background at startup,
// so the first bot-walled page doesn't pay 86MB mid-request. Cheap when a
// build is already cached (one release-API call), skipped entirely when an
// external CDP endpoint is configured.
func prewarmRenderer() {
	defer recoverLog("prewarmRenderer")
	if rendererURL() != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if _, err := obscuraBinary(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "parts-finder: renderer prefetch failed (%v) — a bot-walled fetch will retry it\n", err)
	}
}

// obscuraBinary fetches and manages the obscura executable: the newest
// release, resolved live and cached per version so a new release is picked up
// on the next cold start, falling back to any cached build when the release
// API is unreachable — a slightly old renderer beats none. Deliberately no
// PATH lookup: a system binary would be whatever version happens to be
// installed, outside our update/cleanup management (RENDERER_URL covers
// running your own).
func obscuraBinary(ctx context.Context) (string, error) {
	// One download/unpack at a time: the startup prefetch and a first
	// bot-walled fetch can arrive together, and two writers on the same
	// staging paths would corrupt each other's archive.
	obscuraMu.Lock()
	defer obscuraMu.Unlock()
	dir := rendererCacheDir()
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	osName := map[string]string{"linux": "linux", "darwin": "macos"}[runtime.GOOS]
	if arch == "" || osName == "" {
		return "", fmt.Errorf("no obscura build for %s/%s — set RENDERER_URL to a running CDP endpoint", runtime.GOOS, runtime.GOARCH)
	}
	tag, asset, err := latestObscura(ctx, arch+"-"+osName)
	if err != nil {
		if p := newestCachedObscura(dir); p != "" {
			fmt.Fprintf(os.Stderr, "parts-finder: obscura release lookup failed (%v) — using cached %s\n", err, filepath.Base(filepath.Dir(p)))
			return p, nil
		}
		return "", fmt.Errorf("resolve latest obscura: %w", err)
	}
	// One directory per release: the stealth build ships the launcher AND its
	// worker binary side by side, and the launcher execs the sibling.
	destDir := filepath.Join(dir, "obscura-"+tag)
	bin := filepath.Join(destDir, "obscura")
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if err := downloadObscura(ctx, asset, destDir); err != nil {
		return "", err
	}
	cleanupOldObscura(dir, destDir)
	return bin, nil
}

// downloadObscura downloads the release tarball, verifies its digest, unpacks
// it into a staging dir and only then moves it into place — a half-unpacked
// destDir must never look like a usable install.
func downloadObscura(ctx context.Context, asset ghAsset, destDir string) error {
	tgz := destDir + ".tar.gz"
	if err := downloadVerified(ctx, asset, tgz); err != nil {
		return err
	}
	defer os.Remove(tgz)
	stage := destDir + ".unpack"
	os.RemoveAll(stage)
	if err := extractTarGz(tgz, stage); err != nil {
		os.RemoveAll(stage)
		return fmt.Errorf("unpack %s: %w", asset.Name, err)
	}
	if _, err := os.Stat(filepath.Join(stage, "obscura")); err != nil {
		os.RemoveAll(stage)
		return fmt.Errorf("%s contains no obscura binary", asset.Name)
	}
	os.RemoveAll(destDir)
	return os.Rename(stage, destDir)
}

// extractTarGz unpacks a flat release tarball into dir. Flat is enforced, not
// assumed: an entry with a path separator or ".." is rejected rather than
// written (a tarball is remote input, and zip-slip writes anywhere).
func extractTarGz(src, dir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			continue // symlinks/devices: obscura ships none, and we won't follow one
		}
		name := filepath.Base(h.Name)
		if name != h.Name || name == "." || name == ".." {
			return fmt.Errorf("unexpected path %q in archive", h.Name)
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
}

// cleanupOldObscura deletes superseded cached builds once a new one is
// verified in place — only after, so a failed download never leaves us with
// nothing. Also sweeps the pre-obscura lightpanda binaries, which are dead
// weight now (171MB each). Best-effort.
func cleanupOldObscura(dir, keep string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "obscura-*"))
	stale, _ := filepath.Glob(filepath.Join(dir, "lightpanda*"))
	for _, m := range append(matches, stale...) {
		if m != keep {
			os.RemoveAll(m)
		}
	}
}

// newestCachedObscura returns the obscura binary of the most recently
// downloaded cached build ("" if none). Mod time, not tag order — version
// strings don't sort.
func newestCachedObscura(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "obscura-*"))
	best, bestAt := "", time.Time{}
	for _, m := range matches {
		bin := filepath.Join(m, "obscura")
		fi, err := os.Stat(bin)
		if err != nil {
			continue // a stray tarball or an interrupted unpack
		}
		if fi.ModTime().After(bestAt) {
			best, bestAt = bin, fi.ModTime()
		}
	}
	return best
}

// downloadVerified streams an asset to dest, checking its sha256 against the
// release digest. What we download gets EXECUTED — a mismatch is deleted,
// never unpacked or run.
func downloadVerified(ctx context.Context, asset ghAsset, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("download obscura: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download obscura: %s", resp.Status)
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
		return fmt.Errorf("obscura %s checksum mismatch: got %s want %s", asset.Name, got, want)
	}
	return os.Rename(tmp, dest)
}

// spawnObscura starts `obscura serve --stealth` on a free port and waits for
// the CDP endpoint to answer. --stealth is not optional: a consistent
// fingerprint plus TLS impersonation is the reason we render at all.
func spawnObscura(ctx context.Context, bin string) (base string, cmd *exec.Cmd, err error) {
	port, err := freePort()
	if err != nil {
		return "", nil, err
	}
	cmd = exec.Command(bin, "serve", "--host", "127.0.0.1",
		"--port", fmt.Sprint(port), "--stealth", "--quiet")
	cmd.Stdout, cmd.Stderr = nil, nil // MCP protocol runs on our stdio — keep it clean
	dieWithParent(cmd)                // never orphan a renderer, even if we're SIGKILLed
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start obscura: %w", err)
	}
	// Reap on exit (no zombie) and expose "process died" to the poll loop —
	// cmd.ProcessState is only set by Wait.
	died := make(chan struct{})
	go func() {
		defer recoverLog("obscura-reaper")
		cmd.Wait()
		close(died)
	}()
	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	// Readiness: poll the real CDP endpoint, abort early if the process died.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-died:
			return "", nil, fmt.Errorf("obscura exited during startup")
		default:
		}
		if _, err := wsFromBase(ctx, base); err == nil {
			return base, cmd, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	cmd.Process.Kill()
	return "", nil, fmt.Errorf("obscura did not become ready on %s", base)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// stopRenderer kills every spawned renderer. Called when the MCP exits.
func stopRenderer() {
	rendMu.Lock()
	defer rendMu.Unlock()
	for _, p := range rendProcs {
		if p.cmd != nil && p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
	}
	rendProcs, rendIdle = nil, nil
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

// fetchRendered renders a page and extracts readable text — the escalation
// path for anything plain HTTP can't get (bot walls, JS-only pages).
func fetchRendered(ctx context.Context, rawURL string) (title, text string, err error) {
	_, title, text, err = renderHTML(ctx, rawURL)
	return title, text, err
}

// renderHTML drives a pooled obscura over CDP to load a page (spawning or even
// downloading the browser on first use), waits out delayed hydration, gives
// challenge interstitials a human-like nudge, then extracts readable text.
// Returns the settled HTML too, for callers that parse the markup themselves
// (search-result pages). Safe to call concurrently — each call checks out its
// own renderer.
func renderHTML(ctx context.Context, rawURL string) (html, title, text string, err error) {
	p, err := acquireRenderer(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("renderer unavailable: %w", err)
	}
	defer releaseRenderer(p)
	ws, err := wsFromBase(ctx, p.base)
	if err != nil {
		return "", "", "", fmt.Errorf("renderer unavailable: %w", err)
	}
	// Budget covers navigation + hydration polling + one challenge attempt.
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, ws)
	defer cancelAlloc()
	// obscura auto-attaches its own CDP session on top of the one chromedp
	// opens, and chromedp logs "executor for … doesn't exist" for every event
	// on it. Harmless, but our stderr is the user's log — drop that one line
	// and keep everything else.
	tabCtx, cancelTab := chromedp.NewContext(allocCtx, chromedp.WithErrorf(logRenderError))
	defer cancelTab()

	// network.Enable lets us read the cookie store afterwards (below).
	if err := chromedp.Run(tabCtx, network.Enable(), navigate(rawURL)); err != nil {
		return "", "", "", fmt.Errorf("render %s: %w", rawURL, err)
	}
	html, err = waitStableHTML(tabCtx, 10*time.Second)
	if err != nil {
		return "", "", "", fmt.Errorf("render %s: %w", rawURL, err)
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
			html = cleared
			t, x, exErr = extractHTML([]byte(cleared), rawURL, "")
		}
	}
	// Harvest the browser's cookies into the shared jar so the cheap HTTP path
	// rides whatever the render earned — most importantly a WAF clearance
	// cookie (cf_clearance et al.): once obscura passes the wall, plain
	// fetches to this host stop hitting it, instead of re-rendering every time.
	harvestRenderCookies(tabCtx, rawURL)
	return html, t, x, exErr
}

// navigate is a bare Page.navigate. chromedp.Navigate would be the obvious
// call, but it blocks until a Page.lifecycleEvent named "init" arrives and
// obscura never emits one — it reports commit/DOMContentLoaded/load/networkIdle
// only, so chromedp.Navigate waits out the whole budget on a page that has
// long since loaded. waitStableHTML is the load wait here, and it polls the
// DOM itself.
func navigate(rawURL string) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		_, _, errText, _, err := page.Navigate(rawURL).Do(ctx)
		if err != nil {
			return err
		}
		if errText != "" {
			return fmt.Errorf("page load error %s", errText)
		}
		return nil
	}
}

// outerHTML reads the live DOM through Runtime.evaluate. chromedp's OuterHTML
// goes through its DOM-node query layer, which needs DOM events obscura
// doesn't emit; one evaluate is both portable and cheaper.
func outerHTML(html *string) chromedp.Action {
	return chromedp.Evaluate("document.documentElement.outerHTML", html)
}

// logRenderError forwards chromedp's internal errors to stderr, minus the
// unknown-session noise obscura's extra auto-attached session produces.
func logRenderError(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if strings.Contains(msg, "executor for") && strings.Contains(msg, "doesn't exist") {
		return
	}
	fmt.Fprintf(os.Stderr, "parts-finder: renderer: %s\n", strings.TrimSpace(msg))
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
			outerHTML(&html),
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
		// A JS click, not chromedp.Click: the DOM-query path needs events
		// obscura doesn't emit, and a missing selector must be a no-op here,
		// not a wait.
		var clicked bool
		_ = chromedp.Run(cctx, chromedp.Evaluate(
			`(() => { const el = document.querySelector(`+strconv.Quote(sel)+`); if (!el) return false; el.click(); return true; })()`,
			&clicked))
		cancel()
	}
	// Challenges also clear on their own once their JS finishes — poll either
	// way, bounded.
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if err := chromedp.Run(tabCtx,
			chromedp.Sleep(2*time.Second),
			outerHTML(&html),
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
