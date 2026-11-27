//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// Following one turn of a session to its end: what `bma sessions send
// --wait`, `bma sessions launch --wait` and `bma sessions chat` share.

// The exit codes of a turn that did not end with end_turn.
const (
	exitRequiresAction = 7   // the turn waits for the user (a tool confirmation or a custom tool result)
	exitTurnFailed     = 8   // the turn failed (retries exhausted, a session error) or the session ended
	exitTimeout        = 124 // `bma sessions wait --timeout` passed first
)

const (
	stopEndTurn        = "end_turn"
	stopRequiresAction = "requires_action"
	stopTerminated     = "terminated"
	uploadsDir         = "/mnt/session/uploads/"
)

// turnResult is how a turn ended: the agent's text, the events the session
// waits on, and the session's outputs.
type turnResult struct {
	SessionID  string `json:"session_id"`
	StopReason string `json:"stop_reason"`
	Text       string `json:"text"`
	// Pending are the events a requires_action turn waits on.
	Pending []map[string]any `json:"pending,omitempty"`
	Error   string           `json:"error,omitempty"`
	// Outputs are the files the session's agent wrote; bma_delivered marks
	// the ones the final reply names (the deliverables).
	Outputs []any `json:"outputs"`
}

// turnWatcher collects what a turn produces from its events.
type turnWatcher struct {
	res    turnResult
	texts  []string
	byID   map[string]map[string]any
	sent   bool
	errMsg string
}

func newTurnWatcher(sid string) *turnWatcher {
	return &turnWatcher{res: turnResult{SessionID: sid}, byID: map[string]map[string]any{}}
}

// see takes one event and answers whether the turn is over.
func (w *turnWatcher) see(v map[string]any) bool {
	t, _ := v["type"].(string)
	if id, _ := v["id"].(string); id != "" {
		w.byID[id] = v
	}
	switch t {
	case "event_start", "event_delta":
		return false // previews say nothing about the turn
	case "agent.message":
		w.texts = append(w.texts, messageText(v))
	case "session.error":
		if e, ok := v["error"].(map[string]any); ok {
			w.errMsg, _ = e["message"].(string)
		}
	case eventTerminated:
		w.res.StopReason = stopTerminated
		return true
	}
	if t != eventIdle {
		w.sent = true // the turn of our message has started
		return false
	}
	// An idle before any other event is the end of an earlier turn.
	if !w.sent {
		return false
	}
	w.res.StopReason = stopReasonOf(v)
	w.res.Pending = nil
	if w.res.StopReason == stopRequiresAction {
		for _, id := range pendingIDs(v) {
			if ev, ok := w.byID[id]; ok {
				w.res.Pending = append(w.res.Pending, ev)
			} else {
				w.res.Pending = append(w.res.Pending, map[string]any{"id": id})
			}
		}
	}
	return true
}

func (w *turnWatcher) result() turnResult {
	w.res.Text = strings.Join(w.texts, "\n")
	if w.res.StopReason != stopEndTurn && w.res.StopReason != stopRequiresAction {
		w.res.Error = w.errMsg
	}
	return w.res
}

func stopReasonOf(idle map[string]any) string {
	if sr, ok := idle["stop_reason"].(map[string]any); ok {
		if t, _ := sr["type"].(string); t != "" {
			return t
		}
	}
	return stopEndTurn
}

func pendingIDs(idle map[string]any) []string {
	sr, _ := idle["stop_reason"].(map[string]any)
	list, _ := sr["event_ids"].([]any)
	var ids []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			ids = append(ids, s)
		}
	}
	return ids
}

// followTurn opens the session's event stream, calls send once it is open
// (nil: the turn already runs) and follows the turn to its end; each event is
// passed to onEvent when set (with deltas, agent.message previews too).
func followTurn(ctx context.Context, s *session, sid string, send func() error,
	onEvent func(map[string]any) error, deltas bool) (turnResult, error) {
	w := newTurnWatcher(sid)
	if send == nil {
		// Following a turn that runs: once the stream is open, a session
		// that went idle meanwhile has nothing more to deliver.
		w.sent = true
		send = func() error {
			sess, err := getSession(ctx, s, sid)
			if err != nil {
				return err
			}
			if sess["status"] != "running" {
				return errTurnOver
			}
			return nil
		}
	}
	var perr error
	e := &eventStream{s: s, session: sid, opened: send, deltas: deltas}
	err := e.run(ctx, func(v map[string]any) bool {
		if onEvent != nil {
			if perr = onEvent(v); perr != nil {
				return false
			}
		}
		return !w.see(v)
	})
	if errors.Is(err, errTurnOver) {
		return lastTurn(ctx, s, sid)
	}
	if err = errors.Join(err, perr); err != nil {
		return turnResult{}, err
	}
	res := w.result()
	return res, res.withOutputs(ctx, s)
}

// errTurnOver: the turn to follow had ended before the stream opened.
var errTurnOver = errors.New("the turn is over")

// withOutputs lists the session's outputs (GET /v1/files?scope_id=…).
func (r *turnResult) withOutputs(ctx context.Context, s *session) error {
	q := url.Values{"scope_id": {r.SessionID}, "limit": {"1000"}}
	v, err := s.json(ctx, "GET", "v1/files", q, nil)
	if err != nil {
		return err
	}
	page, _ := v.(map[string]any)
	data, _ := page["data"].([]any)
	r.Outputs = []any{}
	for _, f := range data {
		if m, ok := f.(map[string]any); ok {
			if _, out := m["bma_delivered"]; out {
				r.Outputs = append(r.Outputs, m)
			}
		}
	}
	return nil
}

// lastTurn reads how the session's latest turn ended from its event log
// (for a session that is idle already).
func lastTurn(ctx context.Context, s *session, sid string) (turnResult, error) {
	res := turnResult{SessionID: sid}
	idle, err := latestEvent(ctx, s, sid, eventIdle)
	if err != nil {
		return res, err
	}
	if idle == nil {
		res.StopReason = ""
	} else {
		res.StopReason = stopReasonOf(idle)
		if res.StopReason == stopRequiresAction {
			if res.Pending, err = eventsByID(ctx, s, sid, pendingIDs(idle)); err != nil {
				return res, err
			}
		}
	}
	msg, err := latestEvent(ctx, s, sid, "agent.message")
	if err != nil {
		return res, err
	}
	if msg != nil {
		res.Text = messageText(msg)
	}
	return res, res.withOutputs(ctx, s)
}

func eventsPathOf(sid string) string {
	return strings.TrimPrefix(strings.Replace(eventsPath, "{session_id}", url.PathEscape(sid), 1), "/")
}

// latestEvent is the newest event of the type, nil when there is none.
func latestEvent(ctx context.Context, s *session, sid, typ string) (map[string]any, error) {
	v, err := s.json(ctx, "GET", eventsPathOf(sid), url.Values{"order": {"desc"}, "limit": {"1"}, "types": {typ}}, nil)
	if err != nil {
		return nil, err
	}
	page, _ := v.(map[string]any)
	data, _ := page["data"].([]any)
	for _, d := range data {
		if m, ok := d.(map[string]any); ok && m["type"] == typ {
			return m, nil
		}
	}
	return nil, nil
}

// eventsByID finds the events among the session's newest ones.
func eventsByID(ctx context.Context, s *session, sid string, ids []string) ([]map[string]any, error) {
	v, err := s.json(ctx, "GET", eventsPathOf(sid), url.Values{"order": {"desc"}, "limit": {"100"}}, nil)
	if err != nil {
		return nil, err
	}
	page, _ := v.(map[string]any)
	data, _ := page["data"].([]any)
	found := map[string]map[string]any{}
	for _, d := range data {
		if m, ok := d.(map[string]any); ok {
			if id, _ := m["id"].(string); id != "" {
				found[id] = m
			}
		}
	}
	var out []map[string]any
	for _, id := range ids {
		if m, ok := found[id]; ok {
			out = append(out, m)
		} else {
			out = append(out, map[string]any{"id": id})
		}
	}
	return out, nil
}

// printTurn writes the result (the agent's text, then the deliverables; or
// the result object with --format/--query) and answers the exit status.
func printTurn(r turnResult) error {
	if opts.Format != "" || opts.Query != "" {
		if err := printValue(toAny(r), false); err != nil {
			return err
		}
	} else {
		if r.Text != "" {
			fmt.Fprintln(stdout, r.Text)
		}
		var lines []string
		for _, o := range r.Outputs {
			m, _ := o.(map[string]any)
			if m["bma_delivered"] != true {
				continue
			}
			id, _ := m["id"].(string)
			name, _ := m["filename"].(string)
			lines = append(lines, fmt.Sprintf("  %s  %s  (bma files download %s -o %s)", id, name, id, filepath.Base(name)))
		}
		if len(lines) > 0 {
			fmt.Fprintf(stdout, "\nDeliverables:\n%s\n", strings.Join(lines, "\n"))
		}
	}
	return turnStatus(r)
}

// turnStatus is the exit status of a turn, with its reason on stderr.
func turnStatus(r turnResult) error {
	switch r.StopReason {
	case stopEndTurn, "":
		return nil
	case stopRequiresAction:
		fmt.Fprintf(os.Stderr, "session %s waits for input (%d pending); answer with `bma sessions chat %s`\n",
			r.SessionID, len(r.Pending), r.SessionID)
		return exitCode(exitRequiresAction)
	}
	msg := "the turn ended with " + r.StopReason
	if r.Error != "" {
		msg += ": " + r.Error
	}
	fmt.Fprintf(os.Stderr, "error: session %s: %s\n", r.SessionID, msg)
	return exitCode(exitTurnFailed)
}

func toAny(v any) any {
	b, _ := json.Marshal(v)
	out, _ := decodeJSON(b)
	return out
}

// userMessage is a user.message event with text.
func userMessage(text string) map[string]any {
	return map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": text}}}
}

// sendEvents posts events to the session (with an Idempotency-Key when key
// is set).
func sendEvents(ctx context.Context, s *session, sid, key string, events ...any) error {
	payload, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		return err
	}
	var o []option.RequestOption
	if key != "" {
		o = append(o, option.WithHeader("Idempotency-Key", key))
	}
	_, err = s.json(ctx, "POST", eventsPathOf(sid), nil, payload, o...)
	return err
}

// attachment is a local file a message brings: uploaded, then mounted in
// the session at /mnt/session/uploads/<name>.
type attachment struct{ path, mount string }

func attachments(paths []string) ([]attachment, error) {
	seen := map[string]string{}
	var out []attachment
	for _, p := range paths {
		name := filepath.Base(p)
		if prev, dup := seen[name]; dup {
			return nil, usageError(fmt.Sprintf("%s and %s would both mount at %s%s", prev, p, uploadsDir, name))
		}
		seen[name] = p
		out = append(out, attachment{path: p, mount: uploadsDir + name})
	}
	return out, nil
}

// withAttachments adds the mount paths of the attachments to the text.
func withAttachments(text string, att []attachment) string {
	if len(att) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	b.WriteString("\n\nAttached files:")
	for _, a := range att {
		b.WriteString("\n- " + a.mount)
	}
	return b.String()
}

// upload sends one local file to the Files API and answers its ID.
func upload(ctx context.Context, s *session, path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var o []option.RequestOption
	if key != "" {
		o = append(o, option.WithHeader("Idempotency-Key", key))
	}
	meta, err := s.client.Beta.Files.Upload(ctx,
		anthropic.BetaFileUploadParams{File: anthropic.File(f, filepath.Base(path), "")}, o...)
	if err != nil {
		return "", fmt.Errorf("uploading %s: %w", path, err)
	}
	return meta.ID, nil
}

// waitState is the state `bma sessions wait` and `--status` know: the
// session's status, with requires_action for an idle session whose turn
// waits for the user.
func waitState(ctx context.Context, s *session, sess map[string]any) (string, error) {
	st, _ := sess["status"].(string)
	if st != "idle" {
		return st, nil
	}
	id, _ := sess["id"].(string)
	idle, err := latestEvent(ctx, s, id, eventIdle)
	if err != nil {
		return "", err
	}
	if idle != nil && stopReasonOf(idle) == stopRequiresAction {
		return stopRequiresAction, nil
	}
	return st, nil
}

func sessionPath(sid string) string { return "v1/sessions/" + url.PathEscape(sid) }

func getSession(ctx context.Context, s *session, sid string) (map[string]any, error) {
	v, err := s.json(ctx, "GET", sessionPath(sid), nil, nil)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return m, nil
}

// pollEvery is the interval of `bma sessions wait`.
var pollEvery = 2 * time.Second
