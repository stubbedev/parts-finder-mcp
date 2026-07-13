package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// Zero-config headless rendering. Resolution order for a CDP endpoint:
//  1. LIGHTPANDA_URL env (externally managed lightpanda/chrome)
//  2. `lightpanda` binary on PATH — spawned on demand
//  3. cached binary in ~/.cache/parts-finder/ — spawned on demand
//  4. auto-download from lightpanda's GitHub releases, then spawn
// The spawned process lives for the MCP's lifetime and dies with it.
// fetch_content auto-escalates to rendering when a site bot-blocks plain
// HTTP (403/429), so the user never has to configure or request it.

const lightpandaRelease = "https://github.com/lightpanda-io/browser/releases/download/0.3.4/"

var (
	lpMu   sync.Mutex
	lpCmd  *exec.Cmd // spawned lightpanda, if any
	lpBase string    // http CDP base of the endpoint we resolved
)

// ensureRenderer returns the CDP websocket URL, starting or downloading
// lightpanda if needed.
func ensureRenderer(ctx context.Context) (string, error) {
	lpMu.Lock()
	defer lpMu.Unlock()
	// 1. Explicit env always wins.
	if raw := os.Getenv("LIGHTPANDA_URL"); raw != "" {
		return wsFromBase(ctx, raw)
	}
	// Already spawned and still alive?
	if lpBase != "" {
		if ws, err := wsFromBase(ctx, lpBase); err == nil {
			return ws, nil
		}
		lpBase, lpCmd = "", nil // died — respawn below
	}
	bin, err := lightpandaBinary(ctx)
	if err != nil {
		return "", err
	}
	base, cmd, err := spawnLightpanda(ctx, bin)
	if err != nil {
		return "", err
	}
	lpBase, lpCmd = base, cmd
	return wsFromBase(ctx, base)
}

// lightpandaBinary finds or fetches the lightpanda executable.
func lightpandaBinary(ctx context.Context) (string, error) {
	if p, err := exec.LookPath("lightpanda"); err == nil {
		return p, nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(cacheDir, "parts-finder", "lightpanda")
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	osName := map[string]string{"linux": "linux", "darwin": "macos"}[runtime.GOOS]
	if arch == "" || osName == "" {
		return "", fmt.Errorf("no lightpanda build for %s/%s — set LIGHTPANDA_URL to a running instance", runtime.GOOS, runtime.GOARCH)
	}
	u := lightpandaRelease + "lightpanda-" + arch + "-" + osName
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download lightpanda: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download lightpanda: %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	f.Close()
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
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
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start lightpanda: %w", err)
	}
	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	// Readiness: poll the real CDP endpoint, abort early if the process died.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			break
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

// stopRenderer kills a spawned lightpanda. Called when the MCP exits.
func stopRenderer() {
	lpMu.Lock()
	defer lpMu.Unlock()
	if lpCmd != nil && lpCmd.Process != nil {
		lpCmd.Process.Kill()
	}
	lpCmd, lpBase = nil, ""
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

// fetchRendered drives lightpanda over CDP to load a page (spawning or even
// downloading the browser on first use), then extracts readable text.
func fetchRendered(ctx context.Context, rawURL string) (title, text string, err error) {
	ws, err := ensureRenderer(ctx)
	if err != nil {
		return "", "", fmt.Errorf("renderer unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, ws)
	defer cancelAlloc()
	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	defer cancelTab()

	var html string
	if err := chromedp.Run(tabCtx,
		chromedp.Navigate(rawURL),
		chromedp.Sleep(2*time.Second), // let JS settle — eBay et al. hydrate after load
		chromedp.OuterHTML("html", &html),
	); err != nil {
		return "", "", fmt.Errorf("render %s: %w", rawURL, err)
	}
	// Same extraction as the plain fetcher (readability + table preservation):
	// rendering is an implementation detail, never a downgrade.
	return extractHTML([]byte(html), rawURL)
}
