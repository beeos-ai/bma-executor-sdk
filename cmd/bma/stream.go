//go:build darwin || linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/spf13/cobra"
)

// The session event stream (server-sent events) and `bma sessions send`.

const (
	eventsPath       = "/v1/sessions/{session_id}/events"
	eventStreamPath  = "/v1/sessions/{session_id}/events/stream"
	eventIdle        = "session.status_idle"
	eventTerminated  = "session.status_terminated"
	streamMaxBackoff = 30 * time.Second
)

// sseEvent is one server-sent event.
type sseEvent struct {
	ID, Event string
	Data      []byte
}

// readSSE calls fn for every event of r until r ends or fn answers false.
func readSSE(r io.Reader, fn func(sseEvent) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var ev sseEvent
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				ev.Data = []byte(strings.Join(data, "\n"))
				if !fn(ev) {
					return nil
				}
			}
			ev, data = sseEvent{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			ev.ID = value
		case "event":
			ev.Event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF // the server never ends a session stream by itself
}

// eventStream follows the session's event stream, reconnecting with
// Last-Event-ID after a dropped connection, and calls fn for every event
// until fn answers false or ctx ends. open, when set, is called once the
// first connection is established (before any event is read).
type eventStream struct {
	s       *session
	session string
	lastID  string
	opened  func() error
	// deltas asks for agent.message previews (event_start, event_delta).
	deltas bool
}

func (e *eventStream) connect(ctx context.Context) (*http.Response, error) {
	path := strings.TrimPrefix(strings.Replace(eventStreamPath, "{session_id}", url.PathEscape(e.session), 1), "/")
	o := []option.RequestOption{option.WithHeader("Accept", "text/event-stream")}
	if e.lastID != "" {
		o = append(o, option.WithHeader("Last-Event-ID", e.lastID))
	}
	var q url.Values
	if e.deltas {
		q = url.Values{"event_deltas": {"agent.message"}}
	}
	return e.s.do(ctx, "GET", path, q, nil, o...)
}

// permanent: an API refusal other than rate limiting or a server error.
func permanent(err error) bool {
	var api *anthropic.Error
	return errors.As(err, &api) && api.StatusCode < 500 && api.StatusCode != http.StatusTooManyRequests
}

func (e *eventStream) run(ctx context.Context, fn func(v map[string]any) bool) error {
	backoff := time.Second
	for {
		resp, err := e.connect(ctx)
		if err == nil {
			if e.opened != nil {
				open := e.opened
				e.opened = nil
				if err := open(); err != nil {
					_ = resp.Body.Close()
					return err
				}
			}
			stop := false
			err = readSSE(resp.Body, func(ev sseEvent) bool {
				if ev.ID != "" {
					e.lastID = ev.ID
				}
				backoff = time.Second
				v, derr := decodeJSON(ev.Data)
				if derr != nil {
					return true // keep-alives and non-JSON data carry no event
				}
				m, ok := v.(map[string]any)
				if !ok {
					return true
				}
				stop = !fn(m)
				return !stop
			})
			_ = resp.Body.Close()
			if stop {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if permanent(err) || e.opened != nil {
			return err // a refusal, or the first connection never opened
		}
		fmt.Fprintf(os.Stderr, "(stream dropped: %v; reconnecting in %s after event %s)\n", err, backoff, orDash(e.lastID))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, streamMaxBackoff)
	}
}

// printEvent writes one event: a JSON line unless --format/--query say otherwise.
func printEvent(v any) error {
	if opts.Format == "" && opts.Query == "" {
		return write(normalize(v), "jsonl")
	}
	return printValue(v, false)
}

// turnOver: the session went idle or ended.
func turnOver(v map[string]any) bool {
	t, _ := v["type"].(string)
	return t == eventIdle || t == eventTerminated
}

func streamCmd() *cobra.Command {
	var untilIdle bool
	cmd := &cobra.Command{
		Use:   "stream <session_id>",
		Short: "Follow the session's events (server-sent events); resumes after a dropped connection",
		Long: "Follow the session's events as JSON lines until interrupted (or --until-idle). A dropped " +
			"connection is resumed with Last-Event-ID, so no event is lost or repeated.\n\nGET " + eventStreamPath,
		Args:        args(1),
		Annotations: map[string]string{"operation": operationOf("GET", eventStreamPath)},
		RunE: func(cmd *cobra.Command, a []string) error {
			s, err := apiSession()
			if err != nil {
				return err
			}
			var perr error
			e := &eventStream{s: s, session: a[0]}
			err = e.run(cmd.Context(), func(v map[string]any) bool {
				if perr = printEvent(v); perr != nil {
					return false
				}
				return !(untilIdle && turnOver(v))
			})
			return errors.Join(err, perr)
		},
	}
	cmd.Flags().BoolVar(&untilIdle, "until-idle", false, "stop when the session goes idle or ends")
	return cmd
}

func sendCmd() *cobra.Command {
	var stream, wait bool
	var files []string
	var key string
	cmd := &cobra.Command{
		Use:   "send <session_id> <text|->",
		Short: "Send a user message (with --file attachments) to the session; --stream or --wait follow its turn",
		Long: "Send a user message (\"-\" reads it from stdin). Each --file is uploaded and mounted in the session " +
			"at " + uploadsDir + "<name>, and the message lists those paths. Without --stream or --wait, print the " +
			"answer of the send. --wait prints the agent's reply and the deliverables (the outputs the reply names) " +
			"when the turn ends; --stream prints every event of the turn as JSON lines. With either, the exit status " +
			"is the turn's: 0 end_turn, 7 waiting for input (requires_action), 8 failed.\n\nPOST " + eventsPath,
		Args: args(2),
		RunE: func(cmd *cobra.Command, a []string) error {
			if stream && wait {
				return usageError("--stream and --wait exclude each other")
			}
			text := a[1]
			if text == "-" {
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return err
				}
				text = string(b)
			}
			att, err := attachments(files)
			if err != nil {
				return err
			}
			s, err := apiSession()
			if err != nil {
				return err
			}
			ctx, sid := cmd.Context(), a[0]
			if err := mountAttachments(ctx, s, sid, att); err != nil {
				return err
			}
			msg := userMessage(withAttachments(text, att))
			if !stream && !wait {
				payload, err := json.Marshal(map[string]any{"events": []any{msg}})
				if err != nil {
					return err
				}
				var o []option.RequestOption
				if key != "" {
					o = append(o, option.WithHeader("Idempotency-Key", key))
				}
				v, err := s.json(ctx, "POST", eventsPathOf(sid), nil, payload, o...)
				if err != nil || v == nil {
					return err
				}
				return printValue(v, false)
			}
			var onEvent func(map[string]any) error
			if stream {
				onEvent = func(v map[string]any) error { return printEvent(v) }
			}
			res, err := followTurn(ctx, s, sid, func() error { return sendEvents(ctx, s, sid, key, msg) }, onEvent, false)
			if err != nil {
				return err
			}
			if stream {
				return turnStatus(res)
			}
			return printTurn(res)
		},
	}
	cmd.Flags().BoolVar(&stream, "stream", false, "print every event until the turn ends")
	cmd.Flags().BoolVar(&wait, "wait", false, "print the agent's reply and the deliverables when the turn ends")
	cmd.Flags().StringArrayVar(&files, "file", nil, "attach a local file (repeatable): uploaded and mounted at "+uploadsDir+"<name>")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Idempotency-Key of the send; repeating it within 24 hours does not send the message twice")
	return cmd
}

// mountAttachments uploads the files and mounts them in the session.
func mountAttachments(ctx context.Context, s *session, sid string, att []attachment) error {
	for _, at := range att {
		id, err := upload(ctx, s, at.path, "")
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{"file_id": id, "mount_path": at.mount})
		if err != nil {
			return err
		}
		if _, err := s.json(ctx, "POST", sessionPath(sid)+"/resources", nil, payload); err != nil {
			return fmt.Errorf("mounting %s: %w", at.path, err)
		}
	}
	return nil
}

// messageText joins the text blocks of a message event.
func messageText(v map[string]any) string {
	blocks, _ := v["content"].([]any)
	var parts []string
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && m["type"] == "text" {
			if t, ok := m["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "")
}
