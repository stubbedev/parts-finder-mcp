package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	readability "github.com/go-shiori/go-readability"
)

// ponytail: headless rendering is opt-in and only for JS-heavy pages the plain
// fetcher can't read. Default path never touches a browser. Point
// LIGHTPANDA_URL at a running lightpanda (`lightpanda serve`), e.g.
// http://127.0.0.1:9222 or ws://127.0.0.1:9222.

func renderEnabled() bool { return os.Getenv("LIGHTPANDA_URL") != "" }

// wsEndpoint resolves the CDP websocket URL. If LIGHTPANDA_URL is already a ws
// URL, use it; otherwise ask the CDP HTTP endpoint for it.
func wsEndpoint(ctx context.Context) (string, error) {
	raw := os.Getenv("LIGHTPANDA_URL")
	if strings.HasPrefix(raw, "ws://") || strings.HasPrefix(raw, "wss://") {
		return raw, nil
	}
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

// fetchRendered drives lightpanda over CDP to load a JS page, then runs the
// rendered HTML through readability.
func fetchRendered(ctx context.Context, rawURL string) (title, text string, err error) {
	if !renderEnabled() {
		return "", "", fmt.Errorf("render requested but LIGHTPANDA_URL is not set")
	}
	ws, err := wsEndpoint(ctx)
	if err != nil {
		return "", "", fmt.Errorf("lightpanda endpoint: %w", err)
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
		chromedp.OuterHTML("html", &html),
	); err != nil {
		return "", "", fmt.Errorf("render %s: %w", rawURL, err)
	}
	pageURL, _ := url.Parse(rawURL)
	art, err := readability.FromReader(strings.NewReader(html), pageURL)
	if err != nil {
		return "", "", err
	}
	return art.Title, art.TextContent, nil
}
