package bmaexecutor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The host socket of one terminal attachment: the executor dials host.url
// with the single-use ticket (Authorization header only), writes attached,
// then streams output and serves input, ack, resize and close frames.

const (
	// TerminalSubprotocol is the WebSocket subprotocol of terminal sockets.
	TerminalSubprotocol      = "bma.terminal.v1"
	terminalHostDialTimeout  = 15 * time.Second
	terminalFrameWriteBudget = 10 * time.Second
)

// terminalFrame is one bma.terminal.v1 frame (TerminalFrame*); the fields a
// type does not use stay empty.
type terminalFrame struct {
	Type        string  `json:"type"`
	Rows        uint16  `json:"rows,omitempty"`
	Cols        uint16  `json:"cols,omitempty"`
	PID         int     `json:"pid,omitempty"`
	WriterEpoch *uint64 `json:"writer_epoch,omitempty"`
	CanWrite    *bool   `json:"can_write,omitempty"`
	EarliestSeq int64   `json:"earliest_seq,omitempty"`
	LatestSeq   *int64  `json:"latest_seq,omitempty"`
	FromSeq     int64   `json:"from_seq,omitempty"`
	EndSeq      *int64  `json:"end_seq,omitempty"`
	AckSeq      int64   `json:"ack_seq,omitempty"`
	Data        string  `json:"data,omitempty"`
	Truncated   bool    `json:"truncated,omitempty"`
	ExitCode    *int    `json:"exit_code,omitempty"`
	ExitReason  string  `json:"exit_reason,omitempty"`
	Exited      bool    `json:"exited,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	Code        string  `json:"code,omitempty"`
	Message     string  `json:"message,omitempty"`
}

func ref[T any](v T) *T { return &v }

// terminalHostConn is the part of a WebSocket an attachment uses.
type terminalHostConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(code websocket.StatusCode, reason string) error
}

type terminalHostDialer func(ctx context.Context, host TerminalHost) (terminalHostConn, error)

func dialTerminalHost(ctx context.Context, host TerminalHost) (terminalHostConn, error) {
	ctx, cancel := context.WithTimeout(ctx, terminalHostDialTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, host.URL, &websocket.DialOptions{
		Subprotocols: []string{TerminalSubprotocol},
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + host.Ticket}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	if conn.Subprotocol() != TerminalSubprotocol {
		_ = conn.Close(websocket.StatusProtocolError, "bma.terminal.v1 required")
		return nil, errors.New("host did not accept bma.terminal.v1")
	}
	conn.SetReadLimit(1 << 20)
	return conn, nil
}

// terminalHost serves one attachment on its socket.
type terminalHost struct {
	conn terminalHostConn
	a    *terminalAttachment
	wmu  sync.Mutex
	stop context.CancelFunc
}

func (h *terminalHost) write(ctx context.Context, f terminalFrame) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	h.wmu.Lock()
	defer h.wmu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, terminalFrameWriteBudget)
	defer cancel()
	return h.conn.Write(ctx, websocket.MessageText, raw)
}

// serveTerminalAttachment writes the attached frame (and gap, when the
// resume point fell out of the buffer) and starts the frame loops. It
// returns once the attached frame is written; the socket then lives until
// either side ends it.
func serveTerminalAttachment(ctx context.Context, conn terminalHostConn, a *terminalAttachment) error {
	t := a.t
	t.mu.Lock()
	att := terminalFrame{Type: TerminalFrameTypeAttached, Rows: t.rows, Cols: t.cols, PID: t.pid,
		WriterEpoch: ref(a.writerEpoch), CanWrite: ref(a.canWrite), EarliestSeq: t.start, LatestSeq: ref(t.end)}
	gap := a.sent < t.start
	if gap {
		a.sent, a.acked = t.start, t.start
	}
	start, end := t.start, t.end
	t.mu.Unlock()
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h := &terminalHost{conn: conn, a: a, stop: cancel}
	err := h.write(ctx, att)
	if err == nil && gap {
		err = h.write(ctx, terminalFrame{Type: TerminalFrameTypeGap, EarliestSeq: start, LatestSeq: ref(end), Truncated: true})
	}
	if err != nil {
		cancel()
		_ = conn.Close(websocket.StatusInternalError, "attach failed")
		return err
	}
	go h.readLoop(ctx)
	go h.writeLoop(ctx)
	return nil
}

// end detaches and closes the socket once (either loop may end first).
func (h *terminalHost) end(code websocket.StatusCode, reason string) {
	h.stop()
	h.a.t.detach(h.a)
	_ = h.conn.Close(code, reason)
}

// writeLoop sends output within the ack window, then exit or closed.
func (h *terminalHost) writeLoop(ctx context.Context) {
	a, t := h.a, h.a.t
	for {
		t.mu.Lock()
		if t.attached != a {
			t.mu.Unlock()
			return
		}
		var out *terminalFrame
		switch {
		case a.sent < t.start: // the consumer fell behind the buffer
			out = &terminalFrame{Type: TerminalFrameTypeGap, EarliestSeq: t.start, LatestSeq: ref(t.end), Truncated: true}
			a.sent, a.acked = t.start, t.start
		case a.sent < t.end && a.sent-a.acked < terminalWindowBytes:
			n := min(terminalChunkBytes, int(terminalWindowBytes-(a.sent-a.acked)))
			data, _ := t.output(a.sent, n)
			out = &terminalFrame{Type: TerminalFrameTypeOutput, FromSeq: a.sent, EndSeq: ref(a.sent + int64(len(data))),
				Data: base64.StdEncoding.EncodeToString(data)}
			a.sent += int64(len(data))
		case a.sent == t.end && t.closed != "":
			out = &terminalFrame{Type: TerminalFrameTypeClosed, Reason: t.closed}
		case a.sent == t.end && t.exited:
			out = &terminalFrame{Type: TerminalFrameTypeExit, EndSeq: ref(t.end), ExitCode: t.exitCode,
				ExitReason: t.exitReason, Exited: true}
		}
		changed := t.changed
		t.mu.Unlock()
		if out != nil {
			if err := h.write(ctx, *out); err != nil {
				h.end(websocket.StatusGoingAway, "write failed")
				return
			}
			if out.Type == TerminalFrameTypeExit || out.Type == TerminalFrameTypeClosed {
				h.end(websocket.StatusNormalClosure, out.Type)
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// readLoop serves the consumer's frames until the socket ends.
func (h *terminalHost) readLoop(ctx context.Context) {
	a, t := h.a, h.a.t
	for {
		typ, raw, err := h.conn.Read(ctx)
		if err != nil {
			h.end(websocket.StatusNormalClosure, "detached")
			return
		}
		var f terminalFrame
		if typ != websocket.MessageText || json.Unmarshal(raw, &f) != nil {
			h.refuse(ctx, terminalCodeInvalidFrame, "terminal frames are JSON text messages")
			continue
		}
		switch f.Type {
		case TerminalFrameTypeInput:
			if code := a.checkInput(f.WriterEpoch); code != "" {
				h.refuse(ctx, code, "input refused: "+code)
				continue
			}
			data, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				h.refuse(ctx, terminalCodeInvalidFrame, "input data is not base64")
				continue
			}
			if _, err := t.pty.Write(data); err != nil {
				h.refuse(ctx, terminalCodeInternal, "terminal input failed")
			}
		case TerminalFrameTypeAck:
			a.ack(f.AckSeq)
		case TerminalFrameTypeResize:
			if f.Rows == 0 || f.Cols == 0 {
				h.refuse(ctx, terminalCodeInvalidFrame, "resize needs rows and cols")
				continue
			}
			if err := t.pty.resize(f.Rows, f.Cols); err != nil {
				h.refuse(ctx, terminalCodeInternal, "resize failed")
				continue
			}
			t.mu.Lock()
			t.rows, t.cols = f.Rows, f.Cols
			t.mu.Unlock()
			_ = h.write(ctx, terminalFrame{Type: TerminalFrameTypeResized, Rows: f.Rows, Cols: f.Cols})
		case TerminalFrameTypeClose:
			reason := f.Reason
			if reason == "" {
				reason = "closed"
			}
			go t.terminate(reason) // the write loop sends closed once the state changed
		default:
			h.refuse(ctx, terminalCodeInvalidFrame, fmt.Sprintf("unsupported terminal frame type %q", f.Type))
		}
	}
}

// refuse answers a frame with error; a socket that cannot take it ends by
// itself (the read loop sees it).
func (h *terminalHost) refuse(ctx context.Context, code, msg string) {
	_ = h.write(ctx, terminalFrame{Type: TerminalFrameTypeError, Code: code, Message: msg})
}
