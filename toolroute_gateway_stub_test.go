//go:build !windows

package bmaexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stubGateway is a contract double of the platform's execute-only tool
// gateway (ADR-0005) and of the hands behind it: an MCP Streamable HTTP
// endpoint with the gateway's checks (token and epoch, policy, one-time
// approval, idempotency key, three outcomes, background bash) and a machine
// that runs the tool (a Core, plus a bash runner that can leave the command
// running). It only exists for the conformance suite: it is not the platform.
type stubGateway struct {
	t         *testing.T
	server    *httptest.Server
	workspace string
	policy    map[string]string
	agent     *ACPAgent // maps CMA agent_toolset inputs to the hands' Core

	turnCtx    context.Context
	cancelTurn context.CancelFunc

	mu           sync.Mutex
	epoch        int
	tokens       map[string]int
	interactions map[string]*stubInteraction
	keys         map[string]*stubKey
	offline      bool
	sse          bool
	requests     []stubRequest
	bashSeq      int
}

type stubInteraction struct {
	tool, hash, decision, consumedBy string
}

type stubKey struct {
	tool, hash string
	done       chan struct{} // nil for a call whose runner is gone (seeded)
	finished   bool
	reply      stubReply
}

type stubRequest struct {
	Header http.Header
	Method string
	Params json.RawMessage
}

type stubReply struct {
	Text       string
	IsError    bool
	Outcome    string
	Code       string
	Replayed   bool
	Background *RouteBackground
}

const stubSession = "sesn_t1"

func newStubGateway(t *testing.T, policy map[string]string) *stubGateway {
	t.Helper()
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	workspace := filepath.Join(core.opts.WorkRoot, "hands", stubSession)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	noErr(t, core.openLocal(acpBrain, stubSession, workspace))
	g := &stubGateway{t: t, workspace: workspace, policy: policy, agent: &ACPAgent{opts: ACPAgentOptions{Core: core}},
		epoch: 1, tokens: map[string]int{"tok-1": 1}, interactions: map[string]*stubInteraction{}, keys: map[string]*stubKey{}}
	g.turnCtx, g.cancelTurn = context.WithCancel(context.Background())
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(func() { g.cancelTurn(); g.server.Close() })
	return g
}

// newEpoch makes the previous token stale and returns the token of the new epoch.
func (g *stubGateway) newEpoch() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.epoch++
	token := fmt.Sprintf("tok-%d", g.epoch)
	g.tokens[token] = g.epoch
	return token
}

func (g *stubGateway) setOffline(v bool) { g.mu.Lock(); g.offline = v; g.mu.Unlock() }

// answer records the user's answer to the permission interaction of a call.
func (g *stubGateway) answer(id, tool string, input map[string]any, decision string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.interactions[id] = &stubInteraction{tool: tool, hash: acpInputHash(input), decision: decision}
}

// seedStarted stands for a call whose runner died after the gateway marked it submitted.
func (g *stubGateway) seedStarted(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.keys[key] = &stubKey{}
}

func (g *stubGateway) recorded() []stubRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]stubRequest(nil), g.requests...)
}

func (g *stubGateway) serve(w http.ResponseWriter, r *http.Request) {
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
	g.requests = append(g.requests, stubRequest{Header: r.Header.Clone(), Method: req.Method, Params: req.Params})
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
		out, gone := g.toolsCall(r.Context(), p.Name, p.Arguments, p.Meta)
		if gone {
			return // the brain went away; the call keeps running
		}
		structured := map[string]any{"outcome": out.Outcome}
		if out.Code != "" {
			structured["error_code"] = out.Code
		}
		if out.Replayed {
			structured["replayed"] = true
		}
		if out.Background != nil {
			structured["background"] = map[string]any{"pid": out.Background.PID, "output_file": out.Background.OutputFile}
		}
		reply(map[string]any{"content": []map[string]any{{"type": "text", "text": out.Text}}, "isError": out.IsError, "structuredContent": structured})
	default:
		http.Error(w, "unknown method", http.StatusNotFound)
	}
}

func stubRefused(code, detail string) stubReply {
	return stubReply{Outcome: RouteOutcomeNotForwarded, Code: code, Text: code + ": " + detail, IsError: true}
}

// toolsCall is the gateway: policy re-check, approval check, idempotency, then the hands.
func (g *stubGateway) toolsCall(reqCtx context.Context, name string, args, meta map[string]any) (stubReply, bool) {
	key, _ := meta["ai.beeos.bma/idempotency_key"].(string)
	approval, _ := meta["ai.beeos.bma/approval"].(string)
	hash := acpInputHash(args)
	g.mu.Lock()
	if k := g.keys[key]; key != "" && k != nil {
		switch {
		case k.tool != "" && (k.tool != name || k.hash != hash):
			g.mu.Unlock()
			return stubRefused("idempotency_key_reused", "the key belongs to another call"), false
		case k.finished:
			reply := k.reply
			g.mu.Unlock()
			reply.Replayed = true
			return reply, false
		case k.done == nil:
			g.mu.Unlock()
			text := "outcome_unknown: the call was submitted to the machine and its end was not observed; it was not run again."
			return stubReply{Outcome: RouteOutcomeUnknown, Code: "outcome_unknown", Text: text, IsError: true}, false
		default:
			done := k.done
			g.mu.Unlock()
			return g.await(reqCtx, done, k), false
		}
	}
	switch g.policy[name] {
	case "allow":
	case "deny":
		g.mu.Unlock()
		return stubRefused("permission_denied", fmt.Sprintf("the %s tool is not allowed in this session", name)), false
	default: // ask, and an unlisted tool
		if approval == "" {
			g.mu.Unlock()
			return stubRefused("approval_required", fmt.Sprintf("the %s tool needs the user's approval", name)), false
		}
		rec := g.interactions[approval]
		if rec == nil || rec.decision != acpDecisionAllow || rec.tool != name || rec.hash != hash || (rec.consumedBy != "" && rec.consumedBy != key) {
			g.mu.Unlock()
			return stubRefused("approval_invalid", "the approval does not match this call"), false
		}
		rec.consumedBy = key
	}
	if g.offline {
		g.mu.Unlock()
		return stubRefused("environment_unavailable", "the machine is not connected"), false
	}
	k := &stubKey{tool: name, hash: hash, done: make(chan struct{})}
	if key != "" {
		g.keys[key] = k
	}
	g.mu.Unlock()
	go func() {
		reply := g.execute(name, args, meta)
		g.mu.Lock()
		k.reply, k.finished = reply, true
		g.mu.Unlock()
		close(k.done)
	}()
	return g.await(reqCtx, k.done, k), false
}

func (g *stubGateway) await(reqCtx context.Context, done chan struct{}, k *stubKey) stubReply {
	select {
	case <-done:
		g.mu.Lock()
		defer g.mu.Unlock()
		return k.reply
	case <-reqCtx.Done():
		return stubReply{Outcome: RouteOutcomeUnknown, Code: "in_progress", Text: "in_progress: the call is still running", IsError: true}
	}
}

// execute runs the tool on the hands. It is tied to the turn, not to the
// brain's connection: a brain that goes away does not kill the process.
func (g *stubGateway) execute(name string, args, meta map[string]any) stubReply {
	if name == ToolNameBash {
		return g.bash(args, meta)
	}
	text, isError := g.agent.runTool(g.turnCtx, stubSession, "gw", name, args)
	return stubReply{Outcome: RouteOutcomeExecuted, Text: text, IsError: isError}
}

// bash runs the command in its own process group; past the synchronous wait
// (min of timeout and 55 s by default) it is left running and the call
// returns its pid and output file (ADR-0005 decision 4).
func (g *stubGateway) bash(args, meta map[string]any) stubReply {
	command, _ := args["command"].(string)
	wait := 55 * time.Second
	if ms, ok := args["timeout"].(float64); ok && ms > 0 {
		wait = min(wait, time.Duration(ms)*time.Millisecond)
	}
	if ms, ok := meta["ai.beeos.bma/sync_wait_ms"].(float64); ok && ms > 0 {
		wait = min(wait, time.Duration(ms)*time.Millisecond)
	}
	g.mu.Lock()
	g.bashSeq++
	outFile := filepath.Join(g.workspace, fmt.Sprintf(".bash-%d.out", g.bashSeq))
	g.mu.Unlock()
	out, err := os.Create(outFile)
	if err != nil {
		return stubReply{Outcome: RouteOutcomeNotForwarded, Code: "spawn_failed", Text: "spawn_failed: " + err.Error(), IsError: true}
	}
	defer func() { _ = out.Close() }()
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir, cmd.Stdout, cmd.Stderr = g.workspace, out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return stubReply{Outcome: RouteOutcomeNotForwarded, Code: "spawn_failed", Text: "spawn_failed: " + err.Error(), IsError: true}
	}
	pgid := cmd.Process.Pid
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	go func() { // the turn's end kills the process group, in the background too
		select {
		case <-g.turnCtx.Done():
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		case <-done:
		}
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		if g.turnCtx.Err() != nil {
			return stubReply{Outcome: RouteOutcomeExecuted, Text: "tool was canceled", IsError: true}
		}
		return g.bashReply(outFile, waitErr)
	case <-timer.C:
		return stubReply{Outcome: RouteOutcomeExecuted, Text: fmt.Sprintf("running in the background (pid %d), output in %s", pgid, outFile),
			Background: &RouteBackground{PID: pgid, OutputFile: outFile}}
	}
}

func (g *stubGateway) bashReply(outFile string, err error) stubReply {
	raw, _ := os.ReadFile(outFile)
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	}
	text := fmt.Sprintf("exit_code: %d", code)
	if len(raw) > 0 {
		text = string(raw) + "\n" + text
	}
	return stubReply{Outcome: RouteOutcomeExecuted, Text: text}
}
