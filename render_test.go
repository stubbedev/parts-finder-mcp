package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
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
	// Default: a subdir of whatever this OS calls the user cache dir
	// (XDG_CACHE_HOME on Linux, ~/Library/Caches on macOS — os.UserCacheDir
	// decides, this test does not).
	t.Setenv("PARTS_CACHE", "")
	if want, err := os.UserCacheDir(); err == nil {
		if got := rendererCacheDir(); got != filepath.Join(want, "parts-finder") {
			t.Errorf("default cache dir: got %q, want %q", got, filepath.Join(want, "parts-finder"))
		}
	}
	// No home at all: a temp dir, never an empty path.
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	got := rendererCacheDir()
	if got == "" || got == "parts-finder" || !filepath.IsAbs(got) {
		t.Errorf("homeless environment must still resolve an absolute dir, got %q", got)
	}
}

// tarGz builds an in-memory tar.gz with the given flat member names.
func tarGz(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o755, Size: int64(len(n))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Extraction must never follow an entry name out of the staging dir
// (zip-slip), and a well-formed flat tarball must still unpack.
func TestExtractTarGzRejectsEscapes(t *testing.T) {
	for _, name := range []string{
		"../obscura",
		"../../bin/sh",
		"sub/obscura",
		"/etc/passwd",
		".",
	} {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "evil.tar.gz")
			if err := os.WriteFile(src, tarGz(t, name), 0o644); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(t.TempDir(), "stage")
			if err := extractTarGz(src, stage); err == nil {
				t.Errorf("entry %q must be rejected", name)
			}
		})
	}

	// The happy path: a flat tarball unpacks under the staging dir.
	src := filepath.Join(t.TempDir(), "ok.tar.gz")
	if err := os.WriteFile(src, tarGz(t, "obscura", "README.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(t.TempDir(), "stage")
	if err := extractTarGz(src, stage); err != nil {
		t.Fatalf("flat tarball must unpack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stage, "obscura")); err != nil {
		t.Errorf("obscura not unpacked: %v", err)
	}
}
