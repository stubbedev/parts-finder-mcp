package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The stateless HTTP transport must serve a complete request from a single
// POST: no session to resume, so two independent clients (or one that
// restarted) both work against the same process. This is the check that the
// server holds no per-session state — if a tool ever starts to, it breaks here.
func TestStatelessHTTP(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	old := store
	store = st
	t.Cleanup(func() { store = old })

	s := mcp.NewServer(&mcp.Implementation{Name: "parts-finder", Version: "test"}, nil)
	registerTools(s)
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx := context.Background()
	for _, name := range []string{"first", "second"} {
		c := mcp.NewClient(&mcp.Implementation{Name: name, Version: "test"}, nil)
		// Stateless servers answer GET with 405, so don't ask for the
		// standalone SSE stream — request/response is all this transport needs.
		sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:             srv.URL,
			DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			t.Fatalf("%s: connect: %v", name, err)
		}
		tools, err := sess.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("%s: list tools: %v", name, err)
		}
		if len(tools.Tools) == 0 {
			t.Fatalf("%s: no tools listed", name)
		}
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "list_rules"})
		if err != nil {
			t.Fatalf("%s: call list_rules: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s: list_rules returned an error result: %+v", name, res.Content)
		}
		sess.Close()
	}
}
