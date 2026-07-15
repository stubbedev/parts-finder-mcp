package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// resetRendererPool clears the pool globals between tests.
func resetRendererPool() {
	lpMu.Lock()
	defer lpMu.Unlock()
	lpProcs, lpIdle = nil, nil
}

func TestRendererPoolConcurrency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"webSocketDebuggerUrl":"ws://fake"}`))
	}))
	defer srv.Close()
	t.Setenv("LIGHTPANDA_URL", srv.URL)
	resetRendererPool()
	defer resetRendererPool()

	ctx := context.Background()
	// All maxRenderers tokens check out concurrently.
	var procs []*lpProc
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
		releaseRenderer(&lpProc{base: "http://gone"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("releaseRenderer blocked on a stopped pool")
	}
}
