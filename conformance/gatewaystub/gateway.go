//go:build !windows

package gatewaystub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Outcomes of a call, as the gateway says them (ADR-0005).
const (
	OutcomeExecuted     = "executed"
	OutcomeNotForwarded = "not_forwarded"
	OutcomeUnknown      = "unknown"
)

// InitialToken is the tool credential of the turn's first epoch; NewEpoch makes
// it stale.
const InitialToken = "tok-1"

// Call is a tool call that passed the gateway's checks, on its way to the hands.
type Call struct {
	// Key is the call's idempotency key; it is unique per call.
	Key string
	// Name is the tool: bash, read, write, edit, glob or grep.
	Name string
	// Input is the tool's input in the CMA agent_toolset shape.
	Input map[string]any
	// SyncWait, when set, is the call's synchronous wait ceiling
	// (_meta ai.beeos.bma/sync_wait_ms).
	SyncWait time.Duration
}

// Reply is what the hands answer to a Call.
type Reply struct {
	// Outcome defaults to executed.
	Outcome string
	// Code is the reason of a refusal or an unclear outcome.
	Code string
	// Text is the tool's output.
	Text string
	// IsError: the tool failed (or the call was refused).
	IsError bool
	// PID and OutputFile are set for a command left running in the background.
	PID        int
	OutputFile string
}

// Hands is the machine behind the gateway: it runs a call that passed the
// gateway's checks. ctx ends when the turn is canceled (Gateway.CancelTurn), not
// when the brain's connection ends: a brain that goes away does not kill the
// process.
type Hands interface {
	Run(ctx context.Context, call Call) Reply
}

// Options configures New.
type Options struct {
	// Policy is the permission of each tool (allow, ask or deny); a tool it does
	// not name is ask.
	Policy map[string]string
	// Hands makes the machine behind the gateway; turn ends when the turn is
	// canceled. Nil: ShellHands(Workspace), which runs bash only.
	Hands func(turn context.Context) Hands
	// Workspace is where the default hands run; default: a temporary directory.
	Workspace string
}

// Gateway is one running double. Create it with New; it stops with the test.
type Gateway struct {
	t         testing.TB
	server    *httptest.Server
	workspace string
	policy    map[string]string
	hands     Hands

	turnCtx    context.Context
	cancelTurn context.CancelFunc

	mu           sync.Mutex
	epoch        int
	tokens       map[string]int
	interactions map[string]*interaction
	keys         map[string]*key
	offline      bool
	sse          bool
	requests     []Request
}

type interaction struct {
	tool, hash, decision, consumedBy string
}

type key struct {
	tool, hash string
	done       chan struct{} // nil for a call whose runner is gone (seeded)
	finished   bool
	reply      callReply
}

// Request is one HTTP request the gateway received.
type Request struct {
	Header http.Header
	Method string
	Params json.RawMessage
}

type callReply struct {
	Reply
	Replayed bool
}

// New starts a gateway double with one epoch (token InitialToken).
func New(t testing.TB, opts Options) *Gateway {
	t.Helper()
	g := &Gateway{t: t, workspace: opts.Workspace, policy: opts.Policy, epoch: 1,
		tokens: map[string]int{InitialToken: 1}, interactions: map[string]*interaction{}, keys: map[string]*key{}}
	g.turnCtx, g.cancelTurn = context.WithCancel(context.Background())
	switch {
	case opts.Hands != nil:
		g.hands = opts.Hands(g.turnCtx)
	default:
		if g.workspace == "" {
			g.workspace = t.TempDir()
		}
		g.hands = ShellHands(g.turnCtx, g.workspace)
	}
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(func() { g.cancelTurn(); g.server.Close() })
	return g
}

// URL is the endpoint to put in tools_mcp_url.
func (g *Gateway) URL() string { return g.server.URL }

// Workspace is the directory the default hands run in (empty with custom hands
// unless Options.Workspace was given).
func (g *Gateway) Workspace() string { return g.workspace }

// Close stops serving. New registers it with t.Cleanup; calling it earlier
// simulates a gateway that went away.
func (g *Gateway) Close() { g.server.Close() }

// CancelTurn is the platform's cancel of the turn: running calls end.
func (g *Gateway) CancelTurn() { g.cancelTurn() }

// NewEpoch makes the previous token stale and returns the token of the new epoch.
func (g *Gateway) NewEpoch() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.epoch++
	token := fmt.Sprintf("tok-%d", g.epoch)
	g.tokens[token] = g.epoch
	return token
}

// SetOffline makes the hands unreachable: a call that passed the checks is
// refused with environment_unavailable and not recorded.
func (g *Gateway) SetOffline(offline bool) { g.mu.Lock(); g.offline = offline; g.mu.Unlock() }

// SetSSE answers with a Server-Sent Events stream instead of a JSON body.
func (g *Gateway) SetSSE(sse bool) { g.mu.Lock(); g.sse = sse; g.mu.Unlock() }

// Answer records the user's answer ("allow" or "deny") to the permission
// interaction id, as given for tool and input.
func (g *Gateway) Answer(id, tool string, input map[string]any, decision string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.interactions[id] = &interaction{tool: tool, hash: inputHash(input), decision: decision}
}

// SeedStarted stands for a call whose runner died after the gateway marked it
// submitted: the key's outcome is unknown and it is never run again.
func (g *Gateway) SeedStarted(idempotencyKey string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.keys[idempotencyKey] = &key{}
}

// Recorded returns every request received so far.
func (g *Gateway) Recorded() []Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Request(nil), g.requests...)
}

// IdempotencyKey is the key of a call (ADR-0005 decision 6): "bma-tool-" and
// the first 128 bits of sha256 over session, turn and call id, NUL separated.
func IdempotencyKey(sessionID, turnID, callID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + turnID + "\x00" + callID))
	return "bma-tool-" + hex.EncodeToString(sum[:16])
}

// inputHash identifies a call's input (encoding/json sorts map keys).
func inputHash(input map[string]any) string {
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.requests = append(g.requests, Request{Header: r.Header.Clone(), Method: req.Method, Params: req.Params})
	epoch, known := g.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	current, sse := g.epoch, g.sse
	g.mu.Unlock()
	if !known || epoch != current {
		http.Error(w, "stale or unknown tool credential", http.StatusUnauthorized)
		return
	}
	reply := func(result any) {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "mcp-session-1")
		reply(map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "stub-gateway", "version": "0"}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := g.toolsCall(r.Context(), p.Name, p.Arguments, p.Meta)
		structured := map[string]any{"outcome": out.Outcome}
		if out.Code != "" {
			structured["error_code"] = out.Code
		}
		if out.Replayed {
			structured["replayed"] = true
		}
		if out.PID > 0 {
			structured["background"] = map[string]any{"pid": out.PID, "output_file": out.OutputFile}
		}
		reply(map[string]any{"content": []map[string]any{{"type": "text", "text": out.Text}}, "isError": out.IsError, "structuredContent": structured})
	default:
		http.Error(w, "unknown method", http.StatusNotFound)
	}
}

func refused(code, detail string) callReply {
	return callReply{Reply: Reply{Outcome: OutcomeNotForwarded, Code: code, Text: code + ": " + detail, IsError: true}}
}

// toolsCall is the gateway: idempotency, policy re-check, approval check, then the hands.
func (g *Gateway) toolsCall(reqCtx context.Context, name string, args, meta map[string]any) callReply {
	idemKey, _ := meta["ai.beeos.bma/idempotency_key"].(string)
	approval, _ := meta["ai.beeos.bma/approval"].(string)
	hash := inputHash(args)
	g.mu.Lock()
	if k := g.keys[idemKey]; idemKey != "" && k != nil {
		switch {
		case k.tool != "" && (k.tool != name || k.hash != hash):
			g.mu.Unlock()
			return refused("idempotency_key_reused", "the key belongs to another call")
		case k.finished:
			reply := k.reply
			g.mu.Unlock()
			reply.Replayed = true
			return reply
		case k.done == nil:
			g.mu.Unlock()
			text := "outcome_unknown: the call was submitted to the machine and its end was not observed; it was not run again."
			return callReply{Reply: Reply{Outcome: OutcomeUnknown, Code: "outcome_unknown", Text: text, IsError: true}}
		default:
			done := k.done
			g.mu.Unlock()
			return g.await(reqCtx, done, k)
		}
	}
	switch g.policy[name] {
	case "allow":
	case "deny":
		g.mu.Unlock()
		return refused("permission_denied", fmt.Sprintf("the %s tool is not allowed in this session", name))
	default: // ask, and an unlisted tool
		if approval == "" {
			g.mu.Unlock()
			return refused("approval_required", fmt.Sprintf("the %s tool needs the user's approval", name))
		}
		rec := g.interactions[approval]
		if rec == nil || rec.decision != "allow" || rec.tool != name || rec.hash != hash || (rec.consumedBy != "" && rec.consumedBy != idemKey) {
			g.mu.Unlock()
			return refused("approval_invalid", "the approval does not match this call")
		}
		rec.consumedBy = idemKey
	}
	if g.offline {
		g.mu.Unlock()
		return refused("environment_unavailable", "the machine is not connected")
	}
	k := &key{tool: name, hash: hash, done: make(chan struct{})}
	if idemKey != "" {
		g.keys[idemKey] = k
	}
	g.mu.Unlock()
	call := Call{Key: idemKey, Name: name, Input: args}
	if ms, ok := meta["ai.beeos.bma/sync_wait_ms"].(float64); ok && ms > 0 {
		call.SyncWait = time.Duration(ms) * time.Millisecond
	}
	go func() {
		reply := g.hands.Run(g.turnCtx, call)
		if reply.Outcome == "" {
			reply.Outcome = OutcomeExecuted
		}
		g.mu.Lock()
		k.reply, k.finished = callReply{Reply: reply}, true
		g.mu.Unlock()
		close(k.done)
	}()
	return g.await(reqCtx, k.done, k)
}

func (g *Gateway) await(reqCtx context.Context, done chan struct{}, k *key) callReply {
	select {
	case <-done:
		g.mu.Lock()
		defer g.mu.Unlock()
		return k.reply
	case <-reqCtx.Done():
		return callReply{Reply: Reply{Outcome: OutcomeUnknown, Code: "in_progress", Text: "in_progress: the call is still running", IsError: true}}
	}
}
