//go:build darwin || linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `bma sessions chat`, `bma sessions interrupt`, `bma sessions wait` and the
// --status filter of `bma sessions list`.

// chatSignals delivers Ctrl-C to a chat (a variable for the tests).
var chatSignals = defaultChatSignals

func defaultChatSignals() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	return ch, func() { signal.Stop(ch) }
}

func chatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "chat <session_id>",
		Short: "Talk to the session in the terminal: streamed replies, answers to tool approvals, Ctrl-C interrupts",
		Long: `Talk to the session: type a message, the agent's reply streams in. When the
turn waits for you (requires_action), each pending tool call is shown and
answered here: a tool approval with y (allow) or n / a reason (deny), a
custom tool with its result text. Ctrl-C during a turn interrupts it
(user.interrupt); Ctrl-C, Ctrl-D, /exit or /quit at the prompt ends the chat
and leaves the session as it is.`,
		Args: args(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			s, err := apiSession()
			if err != nil {
				return err
			}
			return chat(cmd.Context(), s, a[0], nil)
		},
	}
}

// chat runs the conversation; first, when set, sends the first message.
func chat(ctx context.Context, s *session, sid string, first func() error) error {
	// Ctrl-C belongs to the chat: it interrupts the turn, it does not end bma.
	ctx = context.WithoutCancel(ctx)
	sig, stop := chatSignals()
	defer stop()
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	ask := func(prompt string) (string, bool) {
		fmt.Fprint(os.Stderr, prompt)
		select {
		case l, ok := <-lines:
			return strings.TrimSpace(l), ok
		case <-sig:
			fmt.Fprintln(os.Stderr)
			return "", false
		}
	}
	p := &chatPrinter{previewed: map[string]bool{}}
	turn := func(send func() error) (turnResult, error) {
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-sig:
					fmt.Fprintln(os.Stderr, "\n(interrupting the turn…)")
					if err := sendEvents(ctx, s, sid, "", map[string]any{"type": "user.interrupt"}); err != nil {
						fmt.Fprintln(os.Stderr, "error: interrupt:", err)
					}
				case <-done:
					return
				}
			}
		}()
		return followTurn(ctx, s, sid, send, p.event, true)
	}

	var res turnResult
	var err error
	switch {
	case first != nil:
		res, err = turn(first)
	default:
		sess, gerr := getSession(ctx, s, sid)
		if gerr != nil {
			return gerr
		}
		if sess["status"] == "running" {
			res, err = turn(nil)
		} else {
			res, err = lastTurn(ctx, s, sid)
		}
	}
	for {
		if err != nil {
			return err
		}
		if res.StopReason == stopTerminated {
			fmt.Fprintf(os.Stderr, "session %s has ended\n", sid)
			return exitCode(exitTurnFailed)
		}
		if res.StopReason == stopRequiresAction {
			answers, ok := answerPending(res.Pending, ask)
			if !ok {
				return nil
			}
			res, err = turn(func() error { return sendEvents(ctx, s, sid, "", answers...) })
			continue
		}
		if res.StopReason != "" && res.StopReason != stopEndTurn {
			fmt.Fprintf(os.Stderr, "(the turn ended with %s%s)\n", res.StopReason, prefixed(": ", res.Error))
		}
		printDeliverables(res)
		var text string
		for text == "" {
			var ok bool
			if text, ok = ask("› "); !ok {
				return nil
			}
		}
		if text == "/exit" || text == "/quit" {
			return nil
		}
		msg := userMessage(text)
		res, err = turn(func() error { return sendEvents(ctx, s, sid, "", msg) })
	}
}

func prefixed(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

func printDeliverables(r turnResult) {
	for _, o := range r.Outputs {
		if m, _ := o.(map[string]any); m["bma_delivered"] == true {
			fmt.Fprintf(os.Stderr, "  deliverable %s  %s\n", m["id"], m["filename"])
		}
	}
}

// answerPending asks for the answer of every event the turn waits on.
func answerPending(pending []map[string]any, ask func(string) (string, bool)) ([]any, bool) {
	var answers []any
	for _, ev := range pending {
		id, _ := ev["id"].(string)
		name, _ := ev["name"].(string)
		input := compactJSON(ev["input"])
		switch ev["type"] {
		case "agent.tool_use", "agent.mcp_tool_use":
			if server, _ := ev["mcp_server_name"].(string); server != "" {
				name = server + "/" + name
			}
			fmt.Fprintf(os.Stderr, "\nThe agent asks to run %s %s\n", name, input)
			a, ok := ask("Allow? [y/N, or a reason to deny] ")
			if !ok {
				return nil, false
			}
			ans := map[string]any{"type": "user.tool_confirmation", "tool_use_id": id, "result": "deny"}
			switch strings.ToLower(a) {
			case "y", "yes":
				ans["result"] = "allow"
			case "", "n", "no":
			default:
				ans["deny_message"] = a
			}
			answers = append(answers, ans)
		case "agent.custom_tool_use":
			fmt.Fprintf(os.Stderr, "\nThe agent calls %s %s\n", name, input)
			a, ok := ask("Result: ")
			if !ok {
				return nil, false
			}
			answers = append(answers, map[string]any{"type": "user.custom_tool_result", "custom_tool_use_id": id,
				"content": []any{map[string]any{"type": "text", "text": a}}})
		default:
			fmt.Fprintf(os.Stderr, "the turn waits on event %s, which bma cannot answer here\n", id)
			return nil, false
		}
	}
	return answers, true
}

func compactJSON(v any) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// chatPrinter shows a turn: the agent's text on stdout as it streams, tool
// calls and errors on stderr.
type chatPrinter struct {
	previewed map[string]bool
	open      bool // a previewed message is being written
}

func (p *chatPrinter) event(v map[string]any) error {
	switch v["type"] {
	case "event_delta":
		d, _ := v["delta"].(map[string]any)
		c, _ := d["content"].(map[string]any)
		if t, _ := c["text"].(string); t != "" {
			id, _ := v["event_id"].(string)
			p.previewed[id], p.open = true, true
			_, err := fmt.Fprint(stdout, t)
			return err
		}
	case "agent.message":
		id, _ := v["id"].(string)
		if p.previewed[id] {
			p.open = false
			_, err := fmt.Fprintln(stdout)
			return err
		}
		p.endLine()
		_, err := fmt.Fprintln(stdout, messageText(v))
		return err
	case "agent.tool_use", "agent.mcp_tool_use", "agent.custom_tool_use":
		p.endLine()
		fmt.Fprintf(os.Stderr, "· %s %s\n", v["name"], truncate(compactJSON(v["input"]), 160))
	case "session.error":
		p.endLine()
		e, _ := v["error"].(map[string]any)
		fmt.Fprintf(os.Stderr, "! %v\n", e["message"])
	}
	return nil
}

func (p *chatPrinter) endLine() {
	if p.open {
		fmt.Fprintln(stdout)
		p.open = false
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func interruptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "interrupt <session_id>",
		Short: "Stop the session's current turn (user.interrupt); safe to repeat",
		Long: "Send a user.interrupt: the open turn is cancelled and the session goes idle. Without an open turn " +
			"it is recorded with bma_interrupt_outcome no_active_turn, so repeating it is harmless. Prints the " +
			"event as stored.\n\nPOST " + eventsPath,
		Args: args(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			s, err := apiSession()
			if err != nil {
				return err
			}
			payload, _ := json.Marshal(map[string]any{"events": []any{map[string]any{"type": "user.interrupt"}}})
			v, err := s.json(cmd.Context(), "POST", eventsPathOf(a[0]), nil, payload)
			if err != nil || v == nil {
				return err
			}
			return printValue(v, false)
		},
	}
}

// sessionStates are the states `bma sessions wait --state` and `bma
// sessions list --status` know.
var sessionStates = []string{"idle", "running", stopRequiresAction, stopTerminated}

func waitCmd() *cobra.Command {
	var state string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <session_id>",
		Short: "Wait until the session is idle (or --state), then print it",
		Long: "Poll the session every 2 seconds until it is in --state, then print it as `bma sessions get` " +
			"does. idle matches every idle session, requires_action only one whose turn waits for the user. " +
			"Exit status 124 when --timeout passes first; 8 when the session ends (terminated) while another " +
			"state is awaited.\n\nGET /v1/sessions/{session_id}",
		Args: args(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			if !contains(sessionStates, state) {
				return usageError("--state must be one of " + strings.Join(sessionStates, ", "))
			}
			s, err := apiSession()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var deadline <-chan time.Time
			if timeout > 0 {
				deadline = time.After(timeout)
			}
			for {
				sess, err := getSession(ctx, s, a[0])
				if err != nil {
					return err
				}
				cur, err := waitState(ctx, s, sess)
				if err != nil {
					return err
				}
				if cur == state || state == "idle" && cur == stopRequiresAction {
					return printValue(sess, false)
				}
				if cur == stopTerminated {
					fmt.Fprintf(os.Stderr, "error: session %s ended while waiting for %s\n", a[0], state)
					return exitCode(exitTurnFailed)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-deadline:
					fmt.Fprintf(os.Stderr, "error: session %s is %s after %s\n", a[0], cur, timeout)
					return exitCode(exitTimeout)
				case <-time.After(pollEvery):
				}
			}
		},
	}
	cmd.Flags().StringVar(&state, "state", "idle", "the state to wait for: "+strings.Join(sessionStates, ", "))
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long, e.g. 90s or 10m (default: no limit)")
	return cmd
}

// addStatusFilter gives the generated `bma sessions list` a --status flag:
// idle, running and terminated go to the API's statuses filter;
// requires_action lists idle sessions and keeps those whose turn waits for
// the user (one event read per idle session).
func addStatusFilter(list *cobra.Command) {
	var status []string
	list.Flags().StringSliceVar(&status, "status", nil,
		"keep the sessions in these states (repeatable or comma separated): "+strings.Join(sessionStates, ", ")+
			"; requires_action reads each idle session's last turn")
	generated := list.RunE
	list.RunE = func(cmd *cobra.Command, a []string) error {
		if len(status) == 0 {
			return generated(cmd, a)
		}
		if cmd.Flags().Changed("statuses") {
			return usageError("--status and --statuses exclude each other")
		}
		api := map[string]bool{}
		needAction := false
		for _, st := range status {
			if !contains(sessionStates, st) {
				return usageError("--status must be one of " + strings.Join(sessionStates, ", "))
			}
			if st == stopRequiresAction {
				needAction = true
				st = "idle"
			}
			api[st] = true
		}
		var statuses []string
		for st := range api {
			statuses = append(statuses, st)
		}
		if !needAction {
			return errors.Join(cmd.Flags().Set("statuses", compactJSON(statuses)), generated(cmd, a))
		}
		s, err := apiSession()
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		q := url.Values{"statuses": statuses}
		for _, f := range []string{"agent-id", "agent-version", "limit", "include-archived", "order"} {
			if fl := cmd.Flags().Lookup(f); fl != nil && fl.Changed {
				q.Set(strings.ReplaceAll(f, "-", "_"), fl.Value.String())
			}
		}
		all, _ := cmd.Flags().GetBool("all")
		var items []map[string]any
		if all {
			if items, err = listAll(ctx, s, "v1/sessions", q); err != nil {
				return err
			}
		} else {
			v, err := s.json(ctx, "GET", "v1/sessions", q, nil)
			if err != nil {
				return err
			}
			page, _ := v.(map[string]any)
			data, _ := page["data"].([]any)
			for _, d := range data {
				if m, ok := d.(map[string]any); ok {
					items = append(items, m)
				}
			}
		}
		out := []any{}
		for _, it := range items {
			st, err := waitState(ctx, s, it)
			if err != nil {
				return err
			}
			if contains(status, st) || st == "idle" && contains(status, "idle") ||
				st == stopRequiresAction && contains(status, "idle") {
				out = append(out, it)
			}
		}
		return printValue(out, true)
	}
}
