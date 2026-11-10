package bmaexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// LocalToolRoute runs a brain's tool calls on this machine, in a Core: the
// hands are here. It is the counterpart of PlatformToolRoute, and the two are
// held to the same conformance suite (package conformance, cases T1–T11): a
// brain cannot tell which one it is talking to. What the platform's tool
// gateway does for a remote machine (ADR-0005), this route does for itself,
// because the brain is not trusted to have asked:
//
//   - the call must belong to the turn the route was made for: a call after the
//     turn ended or was canceled, or from an older brain process once a newer
//     turn of the session began, is refused (stale_turn);
//   - the tool's permission is re-evaluated from the permission mode and
//     ToolPolicy: deny refuses (permission_denied), and ask needs the user's
//     allow for this very call: the answer the platform gave under the
//     deterministic approval id (ApprovalID), for the same tool and the same
//     input, recorded with RecordApproval. Without one the call is refused
//     (approval_required); with another tool, input or approval it is refused
//     (approval_invalid). One approval is good for one call id;
//   - a call id runs at most once, also across restarts of the executor: the
//     route keeps a small ledger per session under StateDir ("started" before
//     the tool runs, "done" with its result after). A repeated call is answered
//     with the first result (RouteResult.Replayed); one that was started and
//     whose result was lost with the process has an unknown outcome and is
//     not run again (outcome_unknown, ADR-0013). A refused call is not
//     recorded: it did not run and may be asked for again once approved.
//
// Its tools are the CMA agent_toolset (bash, read, write, edit, glob, grep) in
// the session's Workspace, through the Core's file roots. bash waits at most
// its timeout (and 55 s) and leaves a command that outlives the wait running:
// the call then answers with its pid and the file its output goes to
// (RouteResult.Background), like the platform gateway. When the Core has a
// CoreOptions.Launch hook, every bash command starts through it, as a
// process.start does; a command it refuses did not run: the call is
// not_forwarded with the hook's reason as Code (spawn_failed for a plain
// error). That answer is recorded like a result (the same call id is answered
// with it again), as the platform gateway records what the hands answered.
//
// A LocalToolRoute is bound to one turn of one session and is safe for
// concurrent use. ACPAgent uses it for the turns whose hands are this machine.
type LocalToolRoute struct {
	core      *Core
	sessionID string
	turnID    string
	ledger    *acpLedger
	policy    map[string]string
	ctx       context.Context
	completed atomic.Bool
	log       func(LogLevel, string, error)
	// hook, set by tests only, runs at named points of tool execution; it may
	// block until ctx ends to stand in for a crash there.
	hook func(ctx context.Context, point string)
}

// LocalToolRouteOptions configures NewLocalToolRoute.
type LocalToolRouteOptions struct {
	// Core runs the tools. Required. The route opens the session in it
	// (Workspace) and CloseSession closes it.
	Core *Core
	// StateDir holds the turn state: one small file per session, written
	// atomically. Required. Make it survive the executor's restarts to get the
	// at-most-once guarantee across them.
	StateDir string
	// SessionID and TurnID are the platform session and turn the route is for.
	// Required. A route made for a turn after another one of the same session
	// started makes the earlier route stale.
	SessionID string
	TurnID    string
	// Workspace is the tools' working directory: relative paths and bash start
	// there. It must be an existing absolute directory. Required.
	Workspace string
	// PermissionMode is the session's permission mode: "default" (also "") or
	// "accept_edits" (write and edit are allowed without approval).
	PermissionMode string
	// ToolPolicy overrides the permission of tools (tool name to allow, ask or
	// deny). A tool no mode default or override names is ask (fail-closed).
	ToolPolicy map[string]string
	// Context ends with the turn: a canceled Context ends the calls in flight,
	// makes later calls stale_turn and ends commands left in the background
	// (unless Complete was called first). Default: never ends.
	Context context.Context
	// OnLog tells about turn state problems.
	OnLog func(level LogLevel, message string, err error)
}

// NewLocalToolRoute returns the route of one turn to this machine's Core. It
// opens the session in the Core (idempotent) and begins the turn in the
// session's ledger: the same TurnID again (a restart) keeps what the ledger
// knows, another TurnID replaces it.
func NewLocalToolRoute(opts LocalToolRouteOptions) (*LocalToolRoute, error) {
	if opts.Core == nil || opts.StateDir == "" || opts.SessionID == "" || opts.TurnID == "" || opts.Workspace == "" {
		return nil, errors.New("bmaexecutor: LocalToolRouteOptions.Core, StateDir, SessionID, TurnID and Workspace are required")
	}
	if !filepath.IsAbs(opts.Workspace) {
		return nil, fmt.Errorf("bmaexecutor: LocalToolRouteOptions.Workspace must be an absolute path, got %q", opts.Workspace)
	}
	if info, err := os.Stat(opts.Workspace); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("bmaexecutor: LocalToolRouteOptions.Workspace %q is not an existing directory", opts.Workspace)
	}
	mode := opts.PermissionMode
	if mode == "" {
		mode = "default"
	}
	if !slices.Contains(acpPermissionModes, mode) {
		return nil, fmt.Errorf("bmaexecutor: unknown LocalToolRouteOptions.PermissionMode %q", opts.PermissionMode)
	}
	for tool, permission := range opts.ToolPolicy {
		if permission != "allow" && permission != "ask" && permission != "deny" {
			return nil, fmt.Errorf("bmaexecutor: LocalToolRouteOptions.ToolPolicy[%q] must be allow, ask or deny", tool)
		}
	}
	if err := opts.Core.openLocal(acpBrain, opts.SessionID, opts.Workspace); err != nil {
		return nil, err
	}
	ledger, err := opts.Core.local.ledger(ledgerPath(opts.StateDir, opts.SessionID), opts.SessionID)
	if err == nil {
		_, err = ledger.begin(opts.TurnID)
	}
	if err != nil {
		return nil, fmt.Errorf("bmaexecutor: turn state: %w", err)
	}
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return newLocalToolRoute(localRouteConfig{core: opts.Core, sessionID: opts.SessionID, turnID: opts.TurnID, ledger: ledger,
		policy: acpEffectivePolicy(mode, opts.ToolPolicy), ctx: ctx, log: opts.OnLog}), nil
}

// localRouteConfig is what newLocalToolRoute needs; ACPAgent fills it from its
// own state (the session is already open and the ledger begun).
type localRouteConfig struct {
	core      *Core
	sessionID string
	turnID    string
	ledger    *acpLedger
	// policy is the effective permission of every tool (acpEffectivePolicy).
	policy map[string]string
	ctx    context.Context
	log    func(LogLevel, string, error)
	hook   func(ctx context.Context, point string)
}

func newLocalToolRoute(c localRouteConfig) *LocalToolRoute {
	return &LocalToolRoute{core: c.core, sessionID: c.sessionID, turnID: c.turnID, ledger: c.ledger, policy: c.policy,
		ctx: c.ctx, log: c.log, hook: c.hook}
}

// RecordApproval records the platform's answer to the permission interaction of
// a call, which is what the route's gate checks before it runs an ask tool.
// request is what the brain asked the platform (AgentTurn.RequestInteraction)
// and answer what came back: only an allow or a deny is recorded (anything
// else, such as expired, leaves the call without an approval). The answer must
// be for the interaction asked; its id should be ApprovalID(session, turn, call),
// or the call is refused as approval_invalid.
func (r *LocalToolRoute) RecordApproval(callID string, request InteractionRequestParams, answer InteractionResponseParams) error {
	if callID == "" || request.InteractionID == "" {
		return errors.New("bmaexecutor: RecordApproval needs the call id and the interaction id")
	}
	if answer.InteractionID != request.InteractionID {
		return fmt.Errorf("bmaexecutor: the answer is for interaction %q, not %q", answer.InteractionID, request.InteractionID)
	}
	if answer.Outcome != InteractionOutcomeAllow && answer.Outcome != InteractionOutcomeDeny {
		return nil
	}
	return r.ledger.decide(callID, answer.Outcome, request.InteractionID, request.ToolName, request.ToolInput)
}

// Complete says the turn ended without being canceled or running out of time.
// A bash command left in the background then lives on after the turn's Context
// ends (until it exits, until an hour has passed or CloseSession); otherwise it
// is killed with the turn. Call it before canceling the Context.
func (r *LocalToolRoute) Complete() { r.completed.Store(true) }

// CloseSession ends the session on this machine: its running tools and
// background commands stop, and its workspace state and turn state are
// released. It ends the session for every route of it.
func (r *LocalToolRoute) CloseSession() { r.core.closeLocalSession(r.sessionID) }

// Call implements ToolRoute.
func (r *LocalToolRoute) Call(ctx context.Context, c RouteCall) *RouteResult {
	if c.ID == "" || c.Name == "" {
		return refused("invalid_tool_input", "a tool call needs an ID (its idempotency identity) and a Name")
	}
	// The turn's end also ends the call.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.ctx, cancel)()
	return r.runOnce(ctx, c)
}

// openLocal creates (or re-roots) the Core session of a local brain; it has
// no harness client (no session.ready, no artifact upload).
func (c *Core) openLocal(brain, sessionID, workspace string) error {
	key := coreKey{brain, sessionID}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return &RPCError{Code: CodeResourceUnavailable, Message: "executor is shutting down"}
	}
	if s, ok := c.sessions[key]; ok {
		s.mu.Lock()
		s.workspace = workspace
		s.mu.Unlock()
		return nil
	}
	if len(c.sessions) >= c.opts.MaxSessions {
		return busy("executor is at its session capacity")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.sessions[key] = &CoreSession{key: key, workspace: workspace, ctx: ctx, cancel: cancel, values: map[any]any{}}
	return nil
}

// closeLocal stops a local brain's Core session (its running tools end).
func (c *Core) closeLocal(brain, sessionID string) {
	c.stop(coreKey{brain, sessionID})
}

// localState is what the local routes of a Core share: the turn ledgers (one
// in-memory copy per file) and the bash commands left running.
type localState struct {
	mu      sync.Mutex
	ledgers map[string]*acpLedger // by file path; see agent_acp_resume.go
	// background holds the bash commands left running past their calls (toolroute_local_bash.go).
	background acpBackground
}

// closeLocalSession ends a local session: its background commands, its Core
// session and its turn state.
func (c *Core) closeLocalSession(sessionID string) {
	c.local.background.killSession(sessionID)
	if dir := c.bashOutputDir(sessionID); dir != "" {
		_ = os.RemoveAll(dir)
	}
	c.closeLocal(acpBrain, sessionID)
	c.local.dropLedgers(sessionID)
}

func refused(code, detail string) *RouteResult {
	return &RouteResult{Outcome: RouteOutcomeNotForwarded, Code: code, Text: code + ": " + detail, IsError: true}
}

// acpEffectivePolicy is the permission of every tool of a turn: the mode's
// defaults (the same as bma-dsh-acp's) overridden by turn.start
// metadata.tool_policy. A tool it does not name is ask (fail-closed).
func acpEffectivePolicy(mode string, overrides map[string]string) map[string]string {
	edits := "ask"
	if mode == "accept_edits" {
		edits = "allow"
	}
	policy := map[string]string{ToolNameBash: "ask", ToolNameWrite: edits, ToolNameEdit: edits,
		ToolNameRead: "allow", ToolNameGlob: "allow", ToolNameGrep: "allow"}
	for tool, permission := range overrides {
		policy[tool] = permission
	}
	return policy
}

// acpInputHash identifies a tool call's input (map keys are sorted by encoding/json).
func acpInputHash(input map[string]any) string {
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// gate is the executor's own check before a tool runs, the local copy of the
// platform gateway's (ADR-0005): the brain is not trusted to have asked.
// It returns nil when the call may run (see LocalToolRoute for the rules).
func (r *LocalToolRoute) gate(c RouteCall) *RouteResult {
	if r.ctx.Err() != nil || r.ledger.turnID() != r.turnID {
		return refused("stale_turn", "the turn this tool call belongs to is over")
	}
	switch permission := r.policy[c.Name]; permission {
	case "allow":
		return nil
	case "deny":
		return refused("permission_denied", fmt.Sprintf("the %s tool is not allowed in this session", c.Name))
	}
	call := r.ledger.call(c.ID)
	switch {
	case call.Decision == "" && c.ApprovalID != "":
		return refused("approval_invalid", "no answer of the user is recorded for the approval presented")
	case call.Decision == "":
		return refused("approval_required", fmt.Sprintf("the %s tool needs the user's approval and none was given for this call", c.Name))
	case call.Decision != acpDecisionAllow:
		return refused("approval_invalid", "the user did not approve this call")
	case call.InteractionID != ApprovalID(r.sessionID, r.turnID, c.ID) || (c.ApprovalID != "" && c.ApprovalID != call.InteractionID):
		return refused("approval_invalid", "the approval belongs to another session, turn or call")
	case call.ToolName != c.Name || call.InputHash != acpInputHash(c.Input):
		return refused("approval_invalid", "the approval was given for another tool or input")
	}
	return nil
}

// runOnce runs a tool call at most once across executor restarts: the
// ledger says "started" before the tool runs and "done" with its result
// after. A call asked for again is answered from the ledger; one whose
// start was recorded without a result has an unknown outcome and is not run
// again. A call the gate refuses is not recorded: it did not run and may be
// asked for again once approved.
func (r *LocalToolRoute) runOnce(ctx context.Context, c RouteCall) *RouteResult {
	ledger := r.ledger
	stateError := func(err error) *RouteResult {
		return &RouteResult{Outcome: RouteOutcomeUnknown, Code: "turn_state", Text: "turn state: " + err.Error(), IsError: true}
	}
	switch call := ledger.call(c.ID); call.Phase {
	case acpPhaseDone:
		outcome := call.Outcome
		if outcome == "" {
			outcome = RouteOutcomeExecuted
		}
		return &RouteResult{Outcome: outcome, Code: call.Code, Text: call.Text, IsError: call.IsError, Replayed: true}
	case acpPhaseStarted:
		text := "outcome_unknown: the tool call was started before the executor restarted; whether it took effect is unknown, so it was not run again."
		if err := ledger.update(c.ID, func(s *acpCallState) {
			s.Phase, s.Text, s.IsError, s.Outcome, s.Code = acpPhaseDone, text, true, RouteOutcomeUnknown, "outcome_unknown"
		}); err != nil {
			return stateError(err)
		}
		return &RouteResult{Outcome: RouteOutcomeUnknown, Code: "outcome_unknown", Text: text, IsError: true}
	}
	if denied := r.gate(c); denied != nil {
		r.logf(LogWarn, fmt.Sprintf("tool call %s (%s) of session %s refused: %s", c.ID, c.Name, r.sessionID, denied.Text))
		return denied
	}
	if r.hook != nil {
		r.hook(ctx, "tool:before")
		if ctx.Err() != nil {
			return &RouteResult{Outcome: RouteOutcomeNotForwarded, Code: "canceled", Text: "canceled: The tool call was cancelled.", IsError: true}
		}
	}
	// A bash command passes CoreOptions.Launch before it is marked started. A
	// refusal is the hands' answer: it is recorded and replayed, as the
	// platform gateway replays what the hands answered.
	var plan bashPlan
	if c.Name == ToolNameBash {
		var refusal *RouteResult
		if plan, refusal = r.prepareBash(ctx, c.ID, c.Input); refusal != nil {
			r.logf(LogWarn, fmt.Sprintf("tool call %s (%s) of session %s refused by the launch hook: %s", c.ID, c.Name, r.sessionID, refusal.Text))
			if err := ledger.update(c.ID, func(s *acpCallState) {
				s.Phase, s.Text, s.IsError, s.Outcome, s.Code = acpPhaseDone, refusal.Text, true, refusal.Outcome, refusal.Code
			}); err != nil {
				return stateError(err)
			}
			return refusal
		}
	}
	// A file tool passes CoreOptions.FileAccess before it is marked started;
	// a refusal is recorded and replayed like the launch hook's.
	if refusal := r.checkFileAccess(ctx, c); refusal != nil {
		r.logf(LogWarn, fmt.Sprintf("tool call %s (%s) of session %s refused by the file access hook: %s", c.ID, c.Name, r.sessionID, refusal.Text))
		if err := ledger.update(c.ID, func(s *acpCallState) {
			s.Phase, s.Text, s.IsError, s.Outcome, s.Code = acpPhaseDone, refusal.Text, true, refusal.Outcome, refusal.Code
		}); err != nil {
			return stateError(err)
		}
		return refusal
	}
	if err := ledger.update(c.ID, func(s *acpCallState) { s.Phase = acpPhaseStarted }); err != nil {
		return stateError(err)
	}
	var text string
	var isError bool
	var background *RouteBackground
	var images []ContentBlock
	switch {
	case c.Name == ToolNameBash:
		text, isError, background = r.runBash(ctx, c.ID, c.Input, plan)
	case r.core.hasComputerTool(c.Name):
		text, isError, images = r.runComputer(ctx, c.ID, c.Name, c.Input)
	default:
		text, isError = r.runTool(fileAccessChecked(ctx), c.ID, c.Name, c.Input)
	}
	if err := ledger.update(c.ID, func(s *acpCallState) {
		s.Phase, s.Text, s.IsError, s.Outcome, s.Code = acpPhaseDone, text, isError, RouteOutcomeExecuted, ""
	}); err != nil {
		return stateError(err)
	}
	if r.hook != nil {
		r.hook(ctx, "tool:after")
	}
	return &RouteResult{Outcome: RouteOutcomeExecuted, Text: text, IsError: isError, Background: background, Images: images}
}

// checkFileAccess asks CoreOptions.FileAccess about a file tool call; a
// refusal is the call's answer (not_forwarded, the hook's reason as Code).
func (r *LocalToolRoute) checkFileAccess(ctx context.Context, c RouteCall) *RouteResult {
	if r.core.opts.FileAccess == nil || !coreTools[c.Name] || c.Name == ToolNameBash {
		return nil
	}
	session := r.core.session(coreKey{acpBrain, r.sessionID})
	if session == nil {
		return nil
	}
	input := r.toolInput(c.Name, c.Input)
	err := r.core.checkFileAccess(ctx, session, r.sessionID, c.ID, c.Name, input)
	if code, detail, ok := fileAccessRefusal(err); ok {
		return refused(code, detail)
	}
	return nil
}

// runComputer runs computer_observe / computer_action in Core (its
// ComputerUseBackend): the text blocks are the text, the image blocks the
// images. The ledger keeps the text only.
func (r *LocalToolRoute) runComputer(ctx context.Context, callID, name string, in map[string]any) (string, bool, []ContentBlock) {
	raw, _ := json.Marshal(in)
	result := r.core.toolCall(ctx, acpBrain, ToolCallParams{SessionID: r.sessionID, ToolCallID: callID, Tool: name, Input: raw})
	var texts []string
	var images []ContentBlock
	for _, block := range result.Content {
		switch {
		case block.Type == ContentBlockTypeImage && block.Data != "":
			images = append(images, block)
		case block.Text != "":
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n"), result.IsError || result.Outcome != ToolOutcomeSucceeded, images
}

// acpImages are the images of a _bma/tools/call answer (bma-dsh-acp: {mimeType, data}).
func acpImages(blocks []ContentBlock) []map[string]string {
	var out []map[string]string
	for _, b := range blocks {
		out = append(out, map[string]string{"mimeType": b.MimeType, "data": b.Data})
	}
	return out
}

func (r *LocalToolRoute) logf(level LogLevel, message string) {
	if r.log != nil {
		r.log(level, message, nil)
	}
}

// runTool runs a file tool in Core (bash is runBash). Inputs come in the CMA
// agent_toolset shape that bma-dsh-acp and brain-host share (file_path, timeout
// in ms, replace_all, output_mode, case_insensitive) and are mapped to Core's
// ToolInput here.
func (r *LocalToolRoute) runTool(ctx context.Context, callID, name string, in map[string]any) (string, bool) {
	if !coreTools[name] || name == ToolNameBash {
		return fmt.Sprintf("tool %q is not available on this computer", name), true
	}
	str, flag := inputGetters(in)
	input := r.toolInput(name, in)
	switch name {
	case ToolNameEdit:
		if flag("replace_all") {
			return r.replaceAll(input)
		}
	case ToolNameRead:
		if path, ok := r.ownOutputFile(input.Path); ok {
			return r.readOutputFile(path, input)
		}
	}
	raw, _ := json.Marshal(input)
	result := r.core.toolCall(ctx, acpBrain, ToolCallParams{SessionID: r.sessionID, ToolCallID: callID, Tool: name, Input: raw})
	var texts []string
	for _, block := range result.Content {
		if block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	text := strings.Join(texts, "\n")
	isError := result.IsError || result.Outcome != ToolOutcomeSucceeded
	if name == ToolNameGrep && !isError {
		text = grepOutput(text, str("output_mode"))
	}
	return text, isError
}

func inputGetters(in map[string]any) (str func(string) string, flag func(string) bool) {
	str = func(k string) string { v, _ := in[k].(string); return v }
	flag = func(k string) bool { v, _ := in[k].(bool); return v }
	return
}

// toolInput maps the agent_toolset input of a file tool to Core's ToolInput.
func (r *LocalToolRoute) toolInput(name string, in map[string]any) ToolInput {
	str, flag := inputGetters(in)
	num := func(k string) int64 { v, _ := in[k].(float64); return int64(v) }
	input := ToolInput{Path: str("file_path"), Offset: num("offset"), Limit: num("limit"), Content: str("content"),
		OldString: str("old_string"), NewString: str("new_string"), Pattern: str("pattern"), Glob: str("glob")}
	if name == ToolNameGlob || name == ToolNameGrep {
		input.Path = str("path")
		if name == ToolNameGrep && flag("case_insensitive") {
			input.Pattern = "(?i)" + input.Pattern
		}
	}
	return input
}

// replaceAll is edit with replace_all (Core's edit replaces exactly one match).
func (r *LocalToolRoute) replaceAll(input ToolInput) (string, bool) {
	session := r.core.session(coreKey{acpBrain, r.sessionID})
	if session == nil {
		return "session_not_active: the session is not open", true
	}
	if r.core.opts.RunAs != nil {
		return `tool "edit" is unavailable: this executor runs workloads as another OS user; use bash`, true
	}
	if input.OldString == "" {
		return "edit requires old_string and new_string", true
	}
	n, err := r.core.editFile(session, input, true)
	switch {
	case err != nil:
		return err.Error(), true
	case n == 0:
		return "old_string was not found", true
	}
	return fmt.Sprintf("edited %s (%d replacements)", filepath.Base(input.Path), n), false
}

// grepOutput reshapes Core's "file:line:text" matches for output_mode.
func grepOutput(matches, mode string) string {
	if mode == "" || mode == "content" || matches == "" {
		return matches
	}
	var order []string
	counts := map[string]int{}
	for _, line := range strings.Split(matches, "\n") {
		file, _, _ := strings.Cut(line, ":")
		if counts[file] == 0 {
			order = append(order, file)
		}
		counts[file]++
	}
	out := make([]string, len(order))
	for i, file := range order {
		out[i] = file
		if mode == "count" {
			out[i] = fmt.Sprintf("%s:%d", file, counts[file])
		}
	}
	return strings.Join(out, "\n")
}
