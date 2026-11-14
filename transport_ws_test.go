package bmaexecutor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A write whose ctx has ended fails alone: the connection stays open for the
// other calls on it (coder/websocket closes the connection when a write's ctx
// ends during the write).
func TestWebSocketConnWriteWithEndedContextKeepsTheConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn := NewWebSocketConn(ws)
		for {
			frame, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), frame); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := NewWebSocketConn(ws)
	defer conn.Close(1000, "test done")
	ended, end := context.WithCancel(ctx)
	end()
	for i := range 50 {
		if err := conn.Write(ended, []byte(`"canceled"`)); !errors.Is(err, context.Canceled) {
			t.Fatalf("write %d with an ended ctx = %v, want context.Canceled", i, err)
		}
		if err := conn.Write(ctx, []byte(`"ping"`)); err != nil {
			t.Fatalf("write %d after it: %v", i, err)
		}
		if frame, err := conn.Read(ctx); err != nil || string(frame) != `"ping"` {
			t.Fatalf("read %d after it = %q %v", i, frame, err)
		}
	}
}
