package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// resetRendererPool clears the pool globals between tests.
func resetRendererPool() {
	rendMu.Lock()
	defer rendMu.Unlock()
	rendProcs, rendIdle = nil, nil
}

func TestRendererPoolConcurrency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"webSocketDebuggerUrl":"ws://fake"}`))
	}))
	defer srv.Close()
	t.Setenv("RENDERER_URL", srv.URL)
	resetRendererPool()
	defer resetRendererPool()

	ctx := context.Background()
	// All maxRenderers tokens check out concurrently.
	var procs []*rendProc
	for i := 0; i < maxRenderers; i++ {
		p, err := acquireRenderer(ctx)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		procs = append(procs, p)
	}
	// Pool exhausted: the next acquire must block until release or ctx end.
	shortCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := acquireRenderer(shortCtx); err == nil {
		t.Fatal("acquire beyond pool size must block, not hand out a 4th renderer")
	}
	// A release frees a slot for the next acquire.
	releaseRenderer(procs[0])
	p, err := acquireRenderer(ctx)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if p != procs[0] {
		t.Error("released renderer should be the one handed back out")
	}
}

func TestReleaseRendererAfterStop(t *testing.T) {
	resetRendererPool()
	// stopRenderer nils the pool; a late release must not block or panic.
	done := make(chan struct{})
	go func() {
		releaseRenderer(&rendProc{base: "http://gone"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("releaseRenderer blocked on a stopped pool")
	}
}

// The managed browser must always have somewhere to live: an explicit
// override, the OS cache dir, or a temp dir when the environment has no home
// at all (a bare container). Failing to resolve one would leave the user with
// no renderer and nothing to fix by hand.
func TestRendererCacheDir(t *testing.T) {
	t.Setenv("PARTS_CACHE", "/tmp/pf-cache-override")
	if got := rendererCacheDir(); got != "/tmp/pf-cache-override" {
		t.Errorf("PARTS_CACHE must win, got %q", got)
	}
	t.Setenv("PARTS_CACHE", "")
	t.Setenv("XDG_CACHE_HOME", "/tmp/pf-xdg")
	if got := rendererCacheDir(); got != "/tmp/pf-xdg/parts-finder" {
		t.Errorf("XDG cache dir: got %q", got)
	}
	// No home, no XDG: a temp dir, never an empty path.
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	got := rendererCacheDir()
	if got == "" || got == "parts-finder" || !filepath.IsAbs(got) {
		t.Errorf("homeless environment must still resolve an absolute dir, got %q", got)
	}
}
