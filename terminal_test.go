package bmaexecutor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// termHost is the platform side of the host sockets: it accepts what
// terminal.attach dials, checks the ticket and subprotocol, and hands the
// socket to the test.
type termHost struct {
	srv   *httptest.Server
	conns chan *websocket.Conn
}

func newTermHost(t *testing.T) *termHost {
	h := &termHost{conns: make(chan *websocket.Conn, 4)}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tkt-1" || strings.Contains(r.URL.RawQuery, "tkt") {
			http.Error(w, "ticket only in the Authorization header", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{TerminalSubprotocol}})
		if err != nil {
			return
		}
		c.SetReadLimit(4 << 20)
		h.conns <- c
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *termHost) host() TerminalHost {
	return TerminalHost{URL: "ws" + strings.TrimPrefix(h.srv.URL, "http"), Ticket: "tkt-1"}
}

// termStream is the test's end of one attachment.
type termStream struct {
	t    *testing.T
	c    *websocket.Conn
	text string
	next int64
}

func (s *termStream) write(f map[string]any) error {
	raw, err := json.Marshal(f)
	if err != nil {
		s.t.Fatal(err)
	}
	return s.c.Write(context.Background(), websocket.MessageText, raw)
}

func (s *termStream) send(f map[string]any) {
	if err := s.write(f); err != nil {
		s.t.Fatal(err)
	}
}

// until reads frames (acking output) until done says so; it fails the test
// on timeout. A closed socket ends with a frame of type "_closed".
func (s *termStream) until(done func(map[string]any) bool) map[string]any {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, raw, err := s.c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				s.t.Fatalf("timeout; output tail %q", s.text[max(0, len(s.text)-300):])
			}
			f := map[string]any{"type": "_closed", "status": int(websocket.CloseStatus(err))}
			if done(f) {
				return f
			}
			s.t.Fatalf("socket closed: %v", err)
		}
		var f map[string]any
		if err := json.Unmarshal(raw, &f); err != nil {
			s.t.Fatal(err)
		}
		if f["type"] == "output" {
			from, _ := f["from_seq"].(float64)
			if int64(from) != s.next {
				s.t.Fatalf("output from_seq %v, want %d", f["from_seq"], s.next)
			}
			encoded, _ := f["data"].(string)
			data, _ := base64.StdEncoding.DecodeString(encoded)
			s.text += string(data)
			end, _ := f["end_seq"].(float64)
			s.next = int64(end)
			// The ack is best effort: after the final output the host sends exit and
			// closes, so the write may hit a closed socket; the loop then reads that.
			_ = s.write(map[string]any{"type": "ack", "ack_seq": s.next})
		}
		if done(f) {
			return f
		}
	}
}

func (s *termStream) waitText(needle string) {
	s.t.Helper()
	s.until(func(map[string]any) bool { return strings.Contains(s.text, needle) })
}

func termCall(t *testing.T, h Handler, method string, params any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return h(context.Background(), Request{Method: method, Params: raw})
}

func testTerminals(t *testing.T, opts TerminalOptions) *Terminals {
	if opts.Env == nil {
		opts.Env = append(os.Environ(), "PS1=$ ")
	}
	s := NewTerminals(opts)
	t.Cleanup(s.Close)
	return s
}

func termAttach(t *testing.T, s *Terminals, h *termHost, epoch, from int64) (*termStream, TerminalAttachResult) {
	t.Helper()
	res, err := termCall(t, s.attach, MethodTerminalAttach, TerminalAttachParams{TerminalID: "trm_1", Epoch: epoch,
		StreamID: "st", CanWrite: true, FromSeq: from, AckSeq: from, Host: h.host()})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	attached, _ := res.(TerminalAttachResult)
	return &termStream{t: t, c: <-h.conns, next: from}, attached
}

func termInput(text string, epoch int) map[string]any {
	return map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte(text)), "writer_epoch": epoch}
}

func termDetached(s *Terminals) func() bool {
	return func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.byID["trm_1"]
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.attached == nil
	}
}

func termRPCCode(err error) string {
	var rpc *RPCError
	if errors.As(err, &rpc) && rpc.Data != nil {
		return rpc.Data.TerminalCode
	}
	return ""
}

func termWaitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached")
}

func TestTerminalUTF8Locale(t *testing.T) {
	cases := []struct {
		name   string
		env    []string
		want   []string
		absent []string
	}{
		{name: "unset", env: []string{"A=1"}, want: []string{"LANG=C.UTF-8"}},
		{name: "keeps UTF-8 LANG", env: []string{"LANG=en_US.UTF-8"}, want: []string{"LANG=en_US.UTF-8"}},
		{name: "replaces C LANG", env: []string{"LANG=C"}, want: []string{"LANG=C.UTF-8"}, absent: []string{"LANG=C"}},
		{name: "drops POSIX LC_ALL", env: []string{"LC_ALL=POSIX", "LANG=en_US.UTF-8"},
			want: []string{"LANG=C.UTF-8"}, absent: []string{"LC_ALL=POSIX", "LANG=en_US.UTF-8"}},
		{name: "last duplicate wins", env: []string{"LANG=C", "LANG=zh_CN.UTF-8"},
			want: []string{"LANG=C", "LANG=zh_CN.UTF-8"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := withUTF8Locale(slices.Clone(c.env))
			for _, w := range c.want {
				if !slices.Contains(got, w) {
					t.Errorf("lacks %q: %q", w, got)
				}
			}
			for _, a := range c.absent {
				if slices.Contains(got, a) {
					t.Errorf("still has %q: %q", a, got)
				}
			}
		})
	}
}
