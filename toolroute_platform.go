package bmaexecutor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PlatformToolRoute sends a brain's tool calls to the platform's execute-only
// tool gateway (ADR-0005), an MCP Streamable HTTP endpoint, instead of running
// them here: the hands are on another machine. The gateway does what the
// local route does for itself (token and epoch, policy re-check, approval
// check, at-most-once by idempotency key) and runs the tool in the session's
// machine; this side only speaks its wire and maps the answer to the same
// RouteResult as the local route.
//
// The tool credential (session x turn x epoch) lives in memory only: it is
// never written to the state directory, to a log, to an error text or by
// String/%v. It is replaced before it expires (toolsCredential).
//
// A brain gets one from AgentTurn.Tools for a turn whose turn.start metadata
// carries tools_mcp_* (the hands are on another machine), with the token
// refreshed through tools_token.refresh; NewPlatformToolRoute builds one from
// a credential the caller manages itself. ACPAgent uses it the same way; any
// other turn gets its local route.
type PlatformToolRoute struct {
	cred        *toolsCredential
	sessionID   string
	turnID      string
	approvalFor func(callID string) string
	syncWait    time.Duration
	client      *http.Client

	mu          sync.Mutex
	endpoint    string // the endpoint the MCP session is open on
	mcpSession  string
	initialized bool
	nextID      atomic.Int64
}

type PlatformToolRouteOptions struct {
	// Endpoint is the gateway's MCP URL.
	Endpoint string
	// Token is the turn's tool credential.
	Token string
	// ExpiresAt is when Token stops being accepted (zero: not said, never refreshed).
	ExpiresAt time.Time
	// Refresh replaces the credential before it expires (tools_token.refresh); nil: never refreshed.
	Refresh func(ctx context.Context) (ToolsTokenRefreshResult, error)
	// OnLog tells about a failed refresh (it is also retried; see toolsCredential).
	OnLog  func(level LogLevel, message string, err error)
	TurnID string
	// SessionID is the platform session of the turn (required by NewPlatformToolRoute).
	SessionID string
	// ApprovalFor returns the interaction id of the user's approval of a call,
	// or "" when there is none (ADR-0005: presented in _meta, checked by the gateway).
	ApprovalFor func(callID string) string
	// SyncWait, when set, is sent as the call's synchronous wait ceiling.
	SyncWait time.Duration
	// HTTPClient defaults to a client without a timeout; ctx ends a call.
	HTTPClient *http.Client
}

// NewPlatformToolRoute returns the route of one turn to the platform's tool
// gateway. Endpoint, Token, SessionID and TurnID are required. Most brains use
// AgentTurn.Tools instead, which fills this from turn.start and keeps the token
// fresh; use this when the credential comes from somewhere else.
func NewPlatformToolRoute(opts PlatformToolRouteOptions) (*PlatformToolRoute, error) {
	if opts.Endpoint == "" || opts.Token == "" || opts.SessionID == "" || opts.TurnID == "" {
		return nil, errors.New("bmaexecutor: PlatformToolRouteOptions.Endpoint, Token, SessionID and TurnID are required")
	}
	u, err := url.Parse(opts.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("bmaexecutor: PlatformToolRouteOptions.Endpoint must be an http(s) URL")
	}
	return newPlatformToolRoute(opts), nil
}

func newPlatformToolRoute(opts PlatformToolRouteOptions) *PlatformToolRoute {
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	cred := &toolsCredential{endpoint: opts.Endpoint, token: opts.Token, expiresAt: opts.ExpiresAt, refresh: opts.Refresh, log: opts.OnLog, now: time.Now}
	return &PlatformToolRoute{cred: cred, endpoint: opts.Endpoint, sessionID: opts.SessionID, turnID: opts.TurnID,
		approvalFor: opts.ApprovalFor, syncWait: opts.SyncWait, client: client}
}

const (
	// A tools token with at most this much time left is replaced through
	// tools_token.refresh before the call that needs it (the token lives 1 h).
	toolsTokenRefreshWindow  = 10 * time.Minute
	toolsTokenRefreshTimeout = 10 * time.Second
	// A failed refresh is tried again by a later call after a backoff
	// doubling from toolsTokenRetryBase to toolsTokenRetryMax.
	toolsTokenRetryBase = 5 * time.Second
	toolsTokenRetryMax  = time.Minute
)

// toolsCredential is the tools endpoint and token of one turn, in memory only.
// A failed refresh is never silent: it is logged with the attempt count and
// the token's expiry, the current token keeps being used, and the next call
// after the backoff tries again. A token that expires meanwhile is refused by
// the gateway (the call is not_forwarded / stale_turn), never replaced by a
// local run.
type toolsCredential struct {
	refresh func(context.Context) (ToolsTokenRefreshResult, error)
	log     func(LogLevel, string, error)
	now     func() time.Time

	mu        sync.Mutex
	endpoint  string
	token     string
	expiresAt time.Time
	failures  int
	retryAt   time.Time
}

// String and GoString keep the token out of %v, %+v, %s and %#v.
func (c *toolsCredential) String() string   { return "toolsCredential" }
func (c *toolsCredential) GoString() string { return "toolsCredential" }

// peek returns what the credential is now, without refreshing.
func (c *toolsCredential) peek() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endpoint, c.token
}

// current returns the endpoint and token to call with, refreshed first when
// at most toolsTokenRefreshWindow is left.
func (c *toolsCredential) current(ctx context.Context) (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.refresh == nil || c.expiresAt.IsZero() || c.expiresAt.Sub(now) > toolsTokenRefreshWindow || now.Before(c.retryAt) {
		return c.endpoint, c.token
	}
	_ = c.refreshLocked(ctx, now) // logged; the current token is used meanwhile
	return c.endpoint, c.token
}

// renew replaces the credential now, whatever its expiry: the gateway refused
// it (stale_turn), as it does once the executor reconnected and the session
// moved to a new epoch while the turn goes on. A nil error means a new
// credential was installed; a refusal (the turn is over) leaves the old one.
func (c *toolsCredential) renew(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refresh == nil {
		return errNoToolsRefresh
	}
	return c.refreshLocked(ctx, c.now())
}

var errNoToolsRefresh = errors.New("bmaexecutor: no tools_token.refresh for this route")

// refreshLocked calls tools_token.refresh once and installs its credential;
// a failure is logged, backs off the next due refresh and is returned.
func (c *toolsCredential) refreshLocked(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, toolsTokenRefreshTimeout)
	defer cancel()
	result, err := c.refresh(ctx)
	var expiresAt time.Time
	if err == nil {
		if expiresAt, err = time.Parse(time.RFC3339, result.ExpiresAt); err != nil || result.ToolsMCPToken == "" || result.ToolsMCPURL == "" {
			err = fmt.Errorf("tools_token.refresh returned an unusable credential (token empty=%t, url empty=%t, expires_at %q)",
				result.ToolsMCPToken == "", result.ToolsMCPURL == "", result.ExpiresAt)
		}
	}
	if err != nil {
		c.failures++
		c.retryAt = now.Add(min(toolsTokenRetryBase<<(c.failures-1), toolsTokenRetryMax))
		if c.log != nil {
			c.log(LogError, fmt.Sprintf("tools token refresh failed (attempt %d, token expires %s); calling with the current token, next try after %s",
				c.failures, c.expiresAt.Format(time.RFC3339), c.retryAt.Format(time.RFC3339)), err)
		}
		return err
	}
	c.endpoint, c.token, c.expiresAt, c.failures, c.retryAt = result.ToolsMCPURL, result.ToolsMCPToken, expiresAt, 0, time.Time{}
	if c.log != nil {
		c.log(LogInfo, "tools token refreshed (expires "+expiresAt.Format(time.RFC3339)+")", nil)
	}
	return nil
}

// prepare refreshes the credential when it is due and reopens the MCP session
// when the endpoint moved.
func (r *PlatformToolRoute) prepare(ctx context.Context) {
	endpoint, _ := r.cred.current(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if endpoint != r.endpoint {
		r.endpoint, r.mcpSession, r.initialized = endpoint, "", false
	}
}

// renew replaces the credential (toolsCredential.renew) and, when it did,
// opens a new MCP session with it.
func (r *PlatformToolRoute) renew(ctx context.Context) error {
	if err := r.cred.renew(ctx); err != nil {
		return err
	}
	endpoint, _ := r.cred.peek()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoint, r.mcpSession, r.initialized = endpoint, "", false
	return nil
}

// String keeps the credential out of %v / %+v / %s.
func (r *PlatformToolRoute) String() string {
	endpoint, _ := r.cred.peek()
	return "PlatformToolRoute{" + endpoint + "}"
}

// GoString keeps the credential out of %#v.
func (r *PlatformToolRoute) GoString() string { return r.String() }

const (
	mcpProtocolVersion = "2025-06-18"

	metaApproval    = "ai.beeos.bma/approval"
	metaToolUseID   = "ai.beeos.bma/tool_use_id"
	metaIdempotency = "ai.beeos.bma/idempotency_key"
	metaSyncWait    = "ai.beeos.bma/sync_wait_ms"

	maxGatewayReply = 64 << 20
)

// platformIdempotencyKey is the call's idempotency key (ADR-0005 decision 6):
// "bma-tool-" and the first 128 bits of sha256 over session, turn and call id.
func platformIdempotencyKey(sessionID, turnID, callID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + turnID + "\x00" + callID))
	return "bma-tool-" + hex.EncodeToString(sum[:16])
}

// A call whose request was sent and whose answer was lost (the brain's
// network dropped, the gateway answered 5xx) or that the gateway reports
// still running (in_progress) is sent again with the same idempotency key
// until its saved reply comes back (ADR-0029 §4.3: replayed=true, never run
// twice), for at most platformRetryWindow, backing off from platformRetryBase
// to platformRetryMax. The window is the default brain_offline_timeout: after
// it the platform has failed the turn anyway. Given up, or the turn ended,
// the answer stays unknown.
var (
	platformRetryWindow = 10 * time.Minute
	platformRetryBase   = time.Second
	platformRetryMax    = 15 * time.Second
)

// Call implements ToolRoute.
func (r *PlatformToolRoute) Call(ctx context.Context, c RouteCall) *RouteResult {
	if c.ID == "" || c.Name == "" {
		return refused("invalid_tool_input", "a tool call needs an ID (its idempotency identity) and a Name")
	}
	var lost *RouteResult // the first answer that left the call's fate open
	deadline := time.Now().Add(platformRetryWindow)
	delay := platformRetryBase
	renewed := false
	for {
		got, sent := r.attempt(ctx, c)
		if got.Code == "stale_turn" && !renewed {
			// Refused before it ran. After a reconnect the session is in a new
			// epoch while the turn goes on: a fresh credential for the turn,
			// then the same call (same key) again, at once. The refusal can
			// arrive while the executor channel is still reconnecting; the
			// refresh then waits for it, within the retry window.
			err := r.renew(ctx)
			if err == nil {
				renewed = true
				continue
			}
			if errors.Is(err, ErrNotConnected) && time.Now().Add(delay).Before(deadline) {
				select {
				case <-ctx.Done():
					if lost != nil {
						return failure(RouteOutcomeUnknown, "canceled", "the call was abandoned before the tool gateway answered; it may have run").result
					}
					return got // refused before it ran
				case <-time.After(delay):
				}
				delay = min(delay*2, platformRetryMax)
				continue
			}
			renewed = true
		}
		switch {
		case lost == nil && !sent:
			return got // nothing of the call was ever sent: certainly not run
		case !retryable(got, sent, lost != nil):
			return got
		}
		if lost == nil {
			lost = got
		}
		if !time.Now().Add(delay).Before(deadline) {
			return lost
		}
		select {
		case <-ctx.Done():
			return failure(RouteOutcomeUnknown, "canceled", "the call was abandoned before the tool gateway answered; it may have run").result
		case <-time.After(delay):
		}
		delay = min(delay*2, platformRetryMax)
	}
}

// retryable reports an answer after which the same call is sent again: its
// request reached the gateway and the reply is not final (transport_error,
// 5xx, in_progress), or, once a request was lost, a request that could not
// leave this time (the call's fate is still open).
func retryable(got *RouteResult, sent, lostBefore bool) bool {
	switch {
	case got.Code == "transport_error" || got.Code == "in_progress":
		return true
	case got.Code == "tools_unavailable" && got.Outcome == RouteOutcomeUnknown: // 5xx
		return true
	}
	return lostBefore && !sent
}

// attempt sends the call once. sent is false when the request never left
// (the handshake failed or the gateway could not be reached).
func (r *PlatformToolRoute) attempt(ctx context.Context, c RouteCall) (result *RouteResult, sent bool) {
	r.prepare(ctx)
	if err := r.ensureSession(ctx); err != nil {
		return err.result, false // nothing of the call was sent
	}
	meta := map[string]any{metaIdempotency: platformIdempotencyKey(r.sessionID, r.turnID, c.ID)}
	if c.ApprovalID != "" {
		meta[metaApproval] = c.ApprovalID
	} else if r.approvalFor != nil {
		if id := r.approvalFor(c.ID); id != "" {
			meta[metaApproval] = id
		}
	}
	if c.ToolUseID != "" {
		meta[metaToolUseID] = c.ToolUseID
	}
	if r.syncWait > 0 {
		meta[metaSyncWait] = r.syncWait.Milliseconds()
	}
	input := c.Input
	if input == nil {
		input = map[string]any{}
	}
	var reply struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
		IsError           bool `json:"isError"`
		StructuredContent struct {
			Outcome    string `json:"outcome"`
			ErrorCode  string `json:"error_code"`
			Replayed   bool   `json:"replayed"`
			Background *struct {
				PID        int    `json:"pid"`
				OutputFile string `json:"output_file"`
			} `json:"background"`
		} `json:"structuredContent"`
	}
	if err := r.rpc(ctx, "tools/call", map[string]any{"name": c.Name, "arguments": input, "_meta": meta}, &reply); err != nil {
		return err.result, !(err.result.Outcome == RouteOutcomeNotForwarded && err.result.Code == "tools_unavailable" && err.dial)
	}
	out := &RouteResult{Outcome: reply.StructuredContent.Outcome, Code: reply.StructuredContent.ErrorCode,
		IsError: reply.IsError, Replayed: reply.StructuredContent.Replayed}
	if bg := reply.StructuredContent.Background; bg != nil {
		out.Background = &RouteBackground{PID: bg.PID, OutputFile: bg.OutputFile}
	}
	var texts []string
	for _, block := range reply.Content {
		switch {
		case block.Type == "text" && block.Text != "":
			texts = append(texts, block.Text)
		case block.Type == "image" && block.Data != "" && !reply.StructuredContent.Replayed:
			// MCP image content (a computer_observe screenshot): for the model, never for an event.
			out.Images = append(out.Images, ContentBlock{Type: ContentBlockTypeImage, Data: block.Data, MimeType: block.MimeType})
		}
	}
	out.Text = strings.Join(texts, "\n")
	switch out.Outcome {
	case RouteOutcomeExecuted:
	case RouteOutcomeNotForwarded, RouteOutcomeUnknown:
		out.IsError = true
	default: // the gateway always says; anything else cannot be trusted to mean "not run"
		return &RouteResult{Outcome: RouteOutcomeUnknown, Code: "bad_gateway_reply", IsError: true,
			Text: "bad_gateway_reply: the tool gateway's answer carries no outcome"}, true
	}
	if out.Code != "" && !strings.HasPrefix(out.Text, out.Code+":") {
		out.Text = out.Code + ": " + out.Text
	}
	return out, true
}

// callError is a failed exchange with the gateway, already as the answer to
// the call. dial: the connection was never made (nothing was sent).
type callError struct {
	result *RouteResult
	dial   bool
}

func (e *callError) Error() string { return e.result.Text }

func failure(outcome, code, detail string) *callError {
	return &callError{result: &RouteResult{Outcome: outcome, Code: code, Text: code + ": " + detail, IsError: true}}
}

// ensureSession runs the MCP handshake once (initialize, notifications/initialized).
func (r *PlatformToolRoute) ensureSession(ctx context.Context) *callError {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.initialized {
		return nil
	}
	params := map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "bma-executor-sdk", "version": "0"}}
	if err := r.rpcLocked(ctx, "initialize", params, nil); err != nil {
		if err.result.Outcome != RouteOutcomeNotForwarded { // a handshake sends no tool call: nothing ran
			err.result.Outcome = RouteOutcomeNotForwarded
		}
		return err
	}
	if err := r.rpcLocked(ctx, "notifications/initialized", nil, nil); err != nil {
		err.result.Outcome = RouteOutcomeNotForwarded
		return err
	}
	r.initialized = true
	return nil
}

func (r *PlatformToolRoute) rpc(ctx context.Context, method string, params, result any) *callError {
	return r.rpcLocked(ctx, method, params, result)
}

// rpcLocked sends one JSON-RPC message. A notification (nil result) expects no reply body.
func (r *PlatformToolRoute) rpcLocked(ctx context.Context, method string, params, result any) *callError {
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	var id int64
	if result != nil || method != "notifications/initialized" {
		id = r.nextID.Add(1)
		message["id"] = id
	}
	if params != nil {
		message["params"] = params
	}
	body, err := json.Marshal(message)
	if err != nil {
		return failure(RouteOutcomeNotForwarded, "invalid_tool_input", err.Error())
	}
	endpoint, token := r.cred.peek()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return failure(RouteOutcomeNotForwarded, "tools_unavailable", "the tool gateway URL is unusable")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if r.initialized || method != "initialize" {
		req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	}
	if r.mcpSession != "" {
		req.Header.Set("Mcp-Session-Id", r.mcpSession)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return r.transportFailure(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// An epoch that is not the turn's current one, or an expired credential: refused before it ran.
		return failure(RouteOutcomeNotForwarded, "stale_turn", "the platform refused this turn's tool credential (an older epoch or an expired token)")
	case resp.StatusCode >= 500:
		return failure(RouteOutcomeUnknown, "tools_unavailable", fmt.Sprintf("the tool gateway answered %d; the call may have run", resp.StatusCode))
	case resp.StatusCode >= 300:
		return failure(RouteOutcomeNotForwarded, "tools_unavailable", fmt.Sprintf("the tool gateway answered %d", resp.StatusCode))
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" && method == "initialize" {
		r.mcpSession = sid
	}
	if id == 0 || (result == nil && method == "initialize") {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxGatewayReply))
		return nil
	}
	raw, err := readRPCReply(resp, id)
	if err != nil {
		return r.transportFailure(ctx, err)
	}
	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return failure(RouteOutcomeUnknown, "bad_gateway_reply", "the tool gateway's answer is not JSON-RPC")
	}
	if reply.Error != nil {
		return failure(RouteOutcomeNotForwarded, "tools_unavailable", fmt.Sprintf("the tool gateway refused the request (%d): %s", reply.Error.Code, reply.Error.Message))
	}
	if result != nil {
		if err := json.Unmarshal(reply.Result, result); err != nil {
			return failure(RouteOutcomeUnknown, "bad_gateway_reply", "the tool gateway's result has an unexpected shape")
		}
	}
	return nil
}

// transportFailure: a connection that was never made proves the call did not
// run; anything after the request was sent leaves its fate unknown.
func (r *PlatformToolRoute) transportFailure(ctx context.Context, err error) *callError {
	var opErr *net.OpError
	switch {
	case errors.As(err, &opErr) && opErr.Op == "dial" && ctx.Err() == nil:
		e := failure(RouteOutcomeNotForwarded, "tools_unavailable", "the tool gateway cannot be reached")
		e.dial = true
		return e
	case ctx.Err() != nil:
		return failure(RouteOutcomeUnknown, "canceled", "the call was abandoned before the tool gateway answered; it may have run")
	default:
		return failure(RouteOutcomeUnknown, "transport_error", "the connection to the tool gateway failed after the call was sent; it may have run")
	}
}

// readRPCReply returns the JSON-RPC message answering request id, from a JSON
// body or a Server-Sent Events stream (both are Streamable HTTP).
func readRPCReply(resp *http.Response, id int64) ([]byte, error) {
	body := io.LimitReader(resp.Body, maxGatewayReply)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return io.ReadAll(body)
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxGatewayReply)
	var data []string
	flush := func() []byte {
		defer func() { data = nil }()
		var head struct {
			ID json.Number `json:"id"`
		}
		raw := []byte(strings.Join(data, "\n"))
		if json.Unmarshal(raw, &head) == nil && head.ID.String() == fmt.Sprint(id) {
			return raw
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if raw := flush(); raw != nil {
				return raw, nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if raw := flush(); raw != nil {
		return raw, nil
	}
	return nil, io.ErrUnexpectedEOF
}
