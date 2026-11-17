package bmaexecutor

// ScreenLive is an opt-in control-plane extension. It never registers a model
// tool: only the authenticated platform connection can request screen capture.
// The host owns capture permissions, the short lease and retired generations.
import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const ScreenLiveMethod = "screen.live"

type ScreenLiveCommand struct {
	Type              string `json:"type"`
	RequestID         string `json:"request_id"`
	SessionID         string `json:"session_id"`
	MachineID         string `json:"machine_id"`
	Generation        int64  `json:"generation"`
	Room              string `json:"room"`
	URL               string `json:"url,omitempty"`
	Token             string `json:"token,omitempty"`
	PublisherIdentity string `json:"publisher_identity,omitempty"`
	ExpiresAtMS       int64  `json:"expires_at_ms"`
	// Width x Height is the desktop size the session asked for, on
	// screen_live_start only; both are zero when the session left it to the machine.
	Width  uint16 `json:"width,omitempty"`
	Height uint16 `json:"height,omitempty"`
}
type ScreenLiveResult struct {
	Published bool `json:"published"`
	Accepted  bool `json:"accepted"`
}
type ScreenLiveHandler func(context.Context, ScreenLiveCommand) (ScreenLiveResult, error)

// ServeScreenLive must be configured before Start. It is deliberately limited
// to a machine-scoped client, not a harness's workspace session.
func (c *Client) ServeScreenLive(handler ScreenLiveHandler) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if handler == nil || c.options.MachineID == "" || c.options.EnvironmentID != "" || c.state != ConnectionStateDisconnected {
		return errors.New("screen_live_requires_machine_host")
	}
	var display map[string]any
	if len(c.options.Capabilities.Display) > 0 && string(c.options.Capabilities.Display) != "null" {
		if json.Unmarshal(c.options.Capabilities.Display, &display) != nil {
			return errors.New("screen_live_requires_display_object")
		}
	}
	if display == nil {
		display = map[string]any{}
	}
	display["screen_live"] = true
	c.options.Capabilities.Display, _ = json.Marshal(display)
	c.requestHandlers[ScreenLiveMethod] = decodeThen(func(ctx context.Context, p ScreenLiveCommand) (ScreenLiveResult, error) {
		if p.MachineID != c.options.MachineID || p.SessionID == "" || p.Room == "" || p.Generation < 1 ||
			(p.Width == 0) != (p.Height == 0) || p.Width != 0 && p.Type != "screen_live_start" {
			return ScreenLiveResult{}, &RPCError{Code: CodeInvalidParams, Message: "invalid_screen_binding"}
		}
		switch p.Type {
		case "screen_live_start", "screen_live_renew", "screen_live_stop":
		default:
			return ScreenLiveResult{}, &RPCError{Code: CodeInvalidParams, Message: "invalid_screen_command"}
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return handler(ctx, p)
	})
	return nil
}

// ScreenLivePipe multiplexes bounded, private parent IPC over stdout/stdin.
// Diagnostics remain on stderr. EOF cancels pending calls and lets the parent
// lifeline stop the executor. Tokens must never be copied into diagnostic logs.
type ScreenLivePipe struct {
	parentDone   chan struct{}
	writer       io.WriteCloser
	writeMu      sync.Mutex
	mu           sync.Mutex
	pending      map[string]chan bool
	done         chan struct{}
	once         sync.Once
	sequence     uint64
	inputHandler func(context.Context, DesktopInputMessage) error
	slots        chan struct{}
	inputCancel  context.CancelFunc
}

type screenPipeInput struct {
	Type    string              `json:"type"`
	ID      string              `json:"id"`
	Message DesktopInputMessage `json:"message"`
}

func NewScreenLivePipe(reader io.Reader, writer io.WriteCloser) *ScreenLivePipe {
	inputCtx, cancel := context.WithCancel(context.Background())
	p := &ScreenLivePipe{writer: writer, pending: map[string]chan bool{}, done: make(chan struct{}),
		parentDone: make(chan struct{}), slots: make(chan struct{}, 32), inputCancel: cancel}
	inputs, heartbeats := make(chan screenPipeInput, 32), make(chan screenPipeInput, 32)
	var workers sync.WaitGroup
	for _, queue := range []<-chan screenPipeInput{inputs, heartbeats} {
		workers.Add(1)
		go func() { defer workers.Done(); p.runInputs(inputCtx, queue) }()
	}
	go func() {
		defer func() { p.close(); workers.Wait(); close(p.parentDone) }()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 65536)
		for scanner.Scan() {
			var input screenPipeInput
			if json.Unmarshal(scanner.Bytes(), &input) == nil && input.Type == "screen_input" {
				queue := inputs
				if len(input.Message.Packet) == 0 && input.Message.Event.Type == "heartbeat" {
					queue = heartbeats
				}
				select {
				case <-inputCtx.Done():
				case queue <- input:
				default:
					p.inputResult(input.ID, ErrDesktopControl)
				}
				continue
			}
			var reply struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				OK   bool   `json:"ok"`
			}
			if json.Unmarshal(scanner.Bytes(), &reply) != nil || reply.Type != "screen_live_result" {
				continue
			}
			p.mu.Lock()
			ch := p.pending[reply.ID]
			p.mu.Unlock()
			if ch != nil {
				select {
				case ch <- reply.OK:
				default:
				}
			}
		}
		// Media failure cancels pending native work, but only actual parent EOF ends tasks.
		p.close()
		_, _ = io.Copy(io.Discard, reader)
	}()
	return p
}

func (p *ScreenLivePipe) runInputs(parent context.Context, queue <-chan screenPipeInput) {
	for {
		var input screenPipeInput
		select {
		case <-parent.Done():
			return
		case input = <-queue:
		}
		if parent.Err() != nil {
			return
		}
		p.mu.Lock()
		handler := p.inputHandler
		p.mu.Unlock()
		err := ErrDesktopControl
		if handler != nil {
			timeout := time.Second
			// Type only selects a bounded execution budget. The gate still verifies
			// the full signature and authority before dispatching any native operation.
			var packet DesktopSignedInput
			if input.Message.Event.Type == "resize" || json.Unmarshal(input.Message.Packet, &packet) == nil && packet.Input.Type == "resize" {
				timeout = DesktopResizeTimeout
			}
			ctx, cancel := context.WithTimeout(parent, timeout)
			err = handler(ctx, input.Message)
			cancel()
		}
		p.inputResult(input.ID, err)
	}
}

func (p *ScreenLivePipe) inputResult(id string, err error) {
	code := ""
	if errors.Is(err, ErrDesktopResizeUnsupported) {
		code = "resize_unsupported"
	}
	p.writeMu.Lock()
	writeErr := json.NewEncoder(p.writer).Encode(struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		OK   bool   `json:"ok"`
		Code string `json:"code,omitempty"`
	}{"screen_input_result", id, err == nil, code})
	p.writeMu.Unlock()
	if writeErr != nil {
		p.close()
	}
}

// ParentDone is the supervisor lifeline. Media timeout alone must not end Agent tasks.
func (p *ScreenLivePipe) ParentDone() <-chan struct{} { return p.parentDone }
func (p *ScreenLivePipe) Done() <-chan struct{}       { return p.done }
func (p *ScreenLivePipe) close() {
	p.once.Do(func() {
		p.inputCancel()
		close(p.done)
		_ = p.writer.Close()
	})
}
func (p *ScreenLivePipe) Call(ctx context.Context, command ScreenLiveCommand) (ScreenLiveResult, error) {
	if err := ctx.Err(); err != nil {
		return ScreenLiveResult{}, err
	}
	select {
	case <-p.done:
		return ScreenLiveResult{}, errors.New("screen_host_disconnected")
	default:
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return ScreenLiveResult{}, errors.New("screen_host_busy")
	}
	result := make(chan error, 1)
	writeStarted := make(chan struct{})
	writeFinished := make(chan struct{})
	onTimeout := func() {
		// A complete frame can still be followed by a fenced stop. The lease
		// bounds capture if no stop arrives. A partial frame cannot be followed
		// safely: close stdout so the parent disposes the publisher.
		select {
		case <-writeStarted:
			select {
			case <-writeFinished:
			default:
				p.close()
			}
		default:
		}
	}
	go func() {
		defer func() { <-p.slots }()
		// Assign IDs in write order, not caller scheduling order.
		p.writeMu.Lock()
		if ctx.Err() != nil {
			p.writeMu.Unlock()
			result <- ctx.Err()
			return
		}
		select {
		case <-p.done:
			p.writeMu.Unlock()
			result <- errors.New("screen_host_disconnected")
			return
		default:
		}
		p.sequence++
		id := fmt.Sprint(p.sequence)
		ch := make(chan bool, 1)
		p.mu.Lock()
		p.pending[id] = ch
		p.mu.Unlock()
		defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
		row := struct {
			Type    string            `json:"type"`
			ID      string            `json:"id"`
			Command ScreenLiveCommand `json:"command"`
		}{"screen_live_command", id, command}
		raw, err := json.Marshal(row)
		if err == nil && len(raw) >= 65535 {
			err = errors.New("screen_command_too_large")
		}
		if err == nil {
			close(writeStarted)
			if ctx.Err() != nil {
				p.writeMu.Unlock()
				result <- ctx.Err()
				return
			}
			_, err = p.writer.Write(append(raw, '\n'))
			if err == nil {
				close(writeFinished)
			}
		}
		p.writeMu.Unlock()
		if err != nil {
			p.close()
			result <- errors.New("screen_host_unavailable")
			return
		}
		select {
		case ok := <-ch:
			if !ok {
				result <- errors.New("screen_host_rejected")
			} else {
				result <- nil
			}
		case <-ctx.Done():
			result <- ctx.Err()
		case <-p.done:
			result <- errors.New("screen_host_disconnected")
		}
	}()
	select {
	case err := <-result:
		if ctx.Err() != nil {
			onTimeout()
			return ScreenLiveResult{}, ctx.Err()
		}
		if err != nil {
			return ScreenLiveResult{}, err
		}
		return ScreenLiveResult{Accepted: true, Published: command.Type == "screen_live_start"}, nil
	case <-ctx.Done():
		onTimeout()
		return ScreenLiveResult{}, ctx.Err()
	case <-p.done:
		return ScreenLiveResult{}, errors.New("screen_host_disconnected")
	}
}

// SetInputHandler is a private parent lane, not a renderer-authorized grant.
func (p *ScreenLivePipe) SetInputHandler(handler func(context.Context, DesktopInputMessage) error) {
	p.mu.Lock()
	p.inputHandler = handler
	p.mu.Unlock()
}
