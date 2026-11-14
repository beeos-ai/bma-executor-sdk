package bmaexecutor

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Computer use (computer_observe / computer_action) is an optional capability
// served by a ComputerUseBackend that CoreOptions.ComputerUse names. The
// executor advertises computer_use only when it has a backend, so a machine
// without one (no desktop session, no Cua Driver) stays honest and the harness
// never sends it a computer_* tool.
//
// The tool inputs follow Anthropic's computer-use tool (coordinate,
// start_coordinate, text, scroll_direction, scroll_amount) over the contract's
// action enum (click, type, key, scroll, drag), plus open_app for a local
// brain's host tool; coordinates are pixels of the
// screenshot computer_observe returned. pid and window_id (additive, from
// computer_observe's window list) target one window: input is then delivered to
// it in the background and coordinates are window-local.

// ErrComputerUseUnavailable is returned by NewCuaDriver when the driver or its
// interactive-session daemon cannot be used; leave CoreOptions.ComputerUse nil
// then.
var ErrComputerUseUnavailable = errors.New("bmaexecutor: computer use backend is unavailable")

// ComputerActionOpenApp opens an app (ComputerActionRequest.App: a name or a
// bundle id) and brings it to the front. It is an action of the host tool
// computer_action of a local brain, not of the contract's ComputerAction enum.
const ComputerActionOpenApp = "open_app"

// ComputerActionMenu runs an item of an app's menu bar by its labels
// (ComputerActionRequest.MenuPath, for example ["Window", "Weixin"]) in the
// window PID/WindowID names. Like open_app it is an action of the host tool
// only.
const ComputerActionMenu = "menu"

// maxAppName bounds ComputerActionRequest.App; maxMenuItems and maxMenuLabel
// bound ComputerActionRequest.MenuPath.
const (
	maxAppName   = 256
	maxMenuItems = 16
	maxMenuLabel = 200
)

// ComputerErrorWindowFrameRequired is the error code of a click, scroll or
// drag without pid and window_id while the session's last observation was a
// window: its coordinates would be read as screen pixels although the model
// looked at window pixels.
const ComputerErrorWindowFrameRequired = "window_frame_required"

// ComputerFrame is a screenshot a computer use call's coordinates refer to:
// the session's last successful observation of the screen (PID and WindowID
// zero) or of one window. Width and Height are the pixels of the screenshot
// the model was given (zero when the observation returned no readable image).
type ComputerFrame struct {
	PID, WindowID int
	Width, Height int
}

// ComputerVision is whether a model takes images (CoreOptions.ComputerVision).
type ComputerVision string

const (
	// ComputerVisionUnknown: not known; screenshots are sent.
	ComputerVisionUnknown ComputerVision = ""
	// ComputerVisionImages: the model reads images; screenshots are sent.
	ComputerVisionImages ComputerVision = "images"
	// ComputerVisionTextOnly: the model reads text only; screenshots are
	// not sent.
	ComputerVisionTextOnly ComputerVision = "text_only"
)

// ComputerVisionQuery asks CoreOptions.ComputerVision about one session's
// model. Model is the turn's model for a local brain's turn (BrainHost),
// empty for a platform brain's tool.call, whose model the host knows from
// the platform.
type ComputerVisionQuery struct {
	SessionID string
	Model     string
}

// ComputerVisionAnswer is the host's answer. Model names the model in the
// note the model reads (empty: the query's model, else "this model").
type ComputerVisionAnswer struct {
	Model  string
	Vision ComputerVision
}

// computerModelKey carries a local brain turn's model to Core.
type computerModelKey struct{}

// withoutImages drops an observation's image blocks for a model that reads
// text only, with a note in their place.
func (c *Core) withoutImages(ctx context.Context, sessionID string, blocks []ContentBlock) []ContentBlock {
	if c.opts.ComputerVision == nil {
		return blocks
	}
	model, _ := ctx.Value(computerModelKey{}).(string)
	answer := c.opts.ComputerVision(ctx, ComputerVisionQuery{SessionID: sessionID, Model: model})
	if answer.Vision != ComputerVisionTextOnly {
		return blocks
	}
	name := cmp.Or(answer.Model, model, "this model")
	kept := []ContentBlock{{Type: ContentBlockTypeText, Text: fmt.Sprintf(
		"The model %s cannot read images, so the screenshot was not sent. Find what to act on from the window list and the accessibility tree below; coordinates are still pixels of the screenshot, whose size is given.", name)}}
	for _, block := range blocks {
		if block.Type != ContentBlockTypeImage {
			kept = append(kept, block)
		}
	}
	return kept
}

// ComputerUsePreflighter is optionally implemented by a ComputerUseBackend
// whose host can refuse a computer use call before anyone is asked to approve
// it, for example a click that lands on another app or a window on another
// Space. Core calls it after checking the input and before approval
// (ComputerUseApprover or the platform's policy) and execution. An error
// refuses the call: nobody is asked, the backend does not run, and the tool
// result is an error whose text is "<reason>: <message>" for an *RPCError
// with a data reason (NewReasonError) and the error's text otherwise. For a
// platform brain's tool.call the approval happened on the platform before the
// call arrived, so there the order is check, preflight, execution.
type ComputerUsePreflighter interface {
	PreflightObserve(ctx context.Context, req ComputerObserveRequest) error
	PreflightAction(ctx context.Context, req ComputerActionRequest) error
}

// ErrComputerActionRejected means the driver explicitly refused the action
// before input. Transport errors and incomplete/undecodable results never use
// this classification, even if their text mentions a known rejection.
var ErrComputerActionRejected = errors.New("computer_action_rejected")

// cuaFailure is the error of a driver call that exited with status 1 and a
// structured result (CuaDriverError), or nil. Only structured pre-dispatch
// refusals of the driver contract (cuaRejectedCodes, effect "refused") are
// ErrComputerActionRejected; arbitrary stderr, partial-action failures and
// unknown codes are not.
func cuaFailure(tool string, stdout []byte, runErr error, req ComputerActionRequest) error {
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) || exit.ExitCode() != 1 {
		return nil
	}
	if failure, ok := newCuaDriverError(tool, stdout, req); ok {
		return failure
	}
	return nil
}

// ComputerObserveRequest is a computer_observe call.
type ComputerObserveRequest struct {
	// SessionID is assigned by Core, not taken from model tool arguments.
	SessionID string
	// PID and WindowID name one window; both zero observes the whole screen.
	PID, WindowID int
	// ImageFormat optionally requests JPEG for a viewer. Empty keeps lossless PNG.
	// Encoding never changes the screenshot dimensions or coordinate space.
	ImageFormat string
}

// ComputerActionRequest is a computer_action call. Coordinates are pixels of
// the last observation: screen pixels, or window-local ones with PID/WindowID.
type ComputerActionRequest struct {
	// SessionID keeps observation and action in the same driver context.
	SessionID     string
	Action        string
	PID, WindowID int
	Coordinate    *[2]float64
	Start         *[2]float64
	Text          string
	Button        string
	ClickCount    int
	ScrollDir     string
	ScrollAmount  int
	// App is the app open_app opens: a name such as "TextEdit" or a bundle
	// id such as "com.apple.TextEdit".
	App string
	// MenuPath are the menu labels of a menu action, from the menu bar to
	// the item (1-16 labels, each 1-200 characters, trimmed).
	MenuPath []string
	// Frame is the screenshot the coordinates refer to, filled by Core: for
	// an action with PID/WindowID the session's last observation of that
	// window, otherwise its last observation. Nil when there was none.
	Frame *ComputerFrame
}

// ComputerUseBackend performs computer use on the machine's interactive
// desktop. Results are tool result content blocks (an image block carries the
// screenshot as base64).
type ComputerUseBackend interface {
	Observe(ctx context.Context, req ComputerObserveRequest) ([]ContentBlock, error)
	Action(ctx context.Context, req ComputerActionRequest) ([]ContentBlock, error)
}

// ComputerUseApprover is optionally implemented by a ComputerUseBackend whose
// host decides which computer use calls the user must approve, for example
// only actions that may send, submit, pay or delete. Without it every call is
// asked (the tools' declared default). The platform's turn tool_policy and
// ACPAgentOptions.ToolPolicy still win: a call is let through without asking
// only while the ask comes from that default, never when either names the tool.
type ComputerUseApprover interface {
	// NeedsObserveApproval and NeedsActionApproval report whether the user
	// must approve the call. A host that cannot tell should say true.
	NeedsObserveApproval(ctx context.Context, req ComputerObserveRequest) bool
	NeedsActionApproval(ctx context.Context, req ComputerActionRequest) bool
}

// computerNeedsApproval asks the backend's ComputerUseApprover about one
// computer use call that passed its checks (computerCheck). No approver, or an
// input the tool would refuse, is asked.
func (c *Core) computerNeedsApproval(ctx context.Context, sessionID, tool string, raw map[string]any) bool {
	approver, ok := c.opts.ComputerUse.(ComputerUseApprover)
	if !ok {
		return true
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return true
	}
	call, err := c.prepareComputer(coreKey{acpBrain, sessionID}, tool, payload)
	if err != nil {
		return true
	}
	if call.tool == ToolNameComputerObserve {
		return approver.NeedsObserveApproval(ctx, call.observe)
	}
	return approver.NeedsActionApproval(ctx, call.action)
}

// computerInput is the superset input of computer_observe and computer_action
// (contract: additionalProperties true).
type computerInput struct {
	ImageFormat     string    `json:"image_format"`
	Action          string    `json:"action"`
	Display         string    `json:"display"`
	Coordinate      []float64 `json:"coordinate"`
	StartCoordinate []float64 `json:"start_coordinate"`
	Text            string    `json:"text"`
	Button          string    `json:"button"`
	Count           int       `json:"click_count"`
	ScrollDirection string    `json:"scroll_direction"`
	ScrollAmount    int       `json:"scroll_amount"`
	PID             int       `json:"pid"`
	WindowID        int       `json:"window_id"`
	App             string    `json:"app"`
	MenuPath        []string  `json:"menu_path"`
}

// computerCall is one checked computer use call: its tool and the request the
// preflighter, the approver and the backend see.
type computerCall struct {
	tool    string
	observe ComputerObserveRequest
	action  ComputerActionRequest
}

// computerRefusal is a computer use call refused before it ran: by its
// checks (window_frame_required) or by the host's ComputerUsePreflighter.
type computerRefusal struct{ code, message string }

func (e *computerRefusal) Error() string { return e.code + ": " + e.message }

// asComputerRefusal gives a preflighter's error its code: the data reason of
// an *RPCError, the "<code>: " prefix of its text, else computer_use_refused.
func asComputerRefusal(err error) *computerRefusal {
	var refusal *computerRefusal
	if errors.As(err, &refusal) {
		return refusal
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) && rpcErr.Data != nil && rpcErr.Data.Reason != "" {
		return &computerRefusal{rpcErr.Data.Reason, rpcErr.Message}
	}
	text := err.Error()
	if code, rest, ok := strings.Cut(text, ": "); ok && isErrorCode(code) {
		return &computerRefusal{code, rest}
	}
	return &computerRefusal{"computer_use_refused", text}
}

// isErrorCode reports whether s looks like a machine-readable code
// (lower-case letters, digits and underscores, starting with a letter).
func isErrorCode(s string) bool {
	if s == "" || len(s) > 64 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// prepareComputer checks a call's input and its coordinate frame and returns
// the request for it, with the session's frame filled in.
func (c *Core) prepareComputer(key coreKey, tool string, raw []byte) (computerCall, error) {
	var input computerInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &input); err != nil {
			return computerCall{}, fmt.Errorf("invalid tool input: %w", err)
		}
	}
	if input.Display != "" && input.Display != "primary" {
		return computerCall{}, fmt.Errorf("display %q is not available: only the primary display is", input.Display)
	}
	if (input.PID == 0) != (input.WindowID == 0) {
		return computerCall{}, errors.New("pid and window_id go together")
	}
	call := computerCall{tool: tool}
	if tool == ToolNameComputerObserve {
		if err := validateObserveFormat(input.ImageFormat); err != nil {
			return computerCall{}, err
		}
		call.observe = ComputerObserveRequest{SessionID: key.session, PID: input.PID, WindowID: input.WindowID, ImageFormat: input.ImageFormat}
		return call, nil
	}
	request, err := input.actionRequest()
	if err != nil {
		return computerCall{}, err
	}
	request.SessionID = key.session
	request.Frame = c.frames.frameFor(key, request.PID, request.WindowID)
	if err := checkActionFrame(c.frames.last(key), request); err != nil {
		return computerCall{}, err
	}
	call.action = request
	return call, nil
}

// checkActionFrame refuses a click, scroll or drag without a window while the
// session's last observation was a window: the model read window pixels, the
// driver would read screen pixels. It is refused rather than converted: a
// converted screen point can land on another window in front of this one.
func checkActionFrame(last *ComputerFrame, req ComputerActionRequest) error {
	switch req.Action {
	case ComputerActionClick, ComputerActionScroll, ComputerActionDrag:
	default:
		return nil
	}
	if req.WindowID != 0 || last == nil || last.WindowID == 0 {
		return nil
	}
	return &computerRefusal{ComputerErrorWindowFrameRequired, fmt.Sprintf(
		"the last observation was window pid=%d window_id=%d; pass that pid and window_id with pixels from its screenshot, or observe the screen first",
		last.PID, last.WindowID)}
}

// preflightComputer asks the backend's ComputerUsePreflighter about a checked call.
func (c *Core) preflightComputer(ctx context.Context, call computerCall) error {
	preflighter, ok := c.opts.ComputerUse.(ComputerUsePreflighter)
	if !ok {
		return nil
	}
	var err error
	if call.tool == ToolNameComputerObserve {
		err = preflighter.PreflightObserve(ctx, call.observe)
	} else {
		err = preflighter.PreflightAction(ctx, call.action)
	}
	if err != nil {
		return asComputerRefusal(err)
	}
	return nil
}

// computerChecked marks a ctx whose call already passed its checks and the
// preflighter (a local brain's turn checks before it asks for approval).
type computerCheckedKey struct{}

// computerCheck checks a call and asks the preflighter, and returns the
// refusal a tool result carries, or nil when the call may go on.
func (c *Core) computerCheck(ctx context.Context, key coreKey, tool string, input map[string]any) *RouteResult {
	raw, err := json.Marshal(input)
	if err != nil {
		return refused("invalid_tool_input", err.Error())
	}
	call, err := c.prepareComputer(key, tool, raw)
	if err == nil {
		err = c.preflightComputer(ctx, call)
	}
	if err == nil {
		return nil
	}
	var refusal *computerRefusal
	if errors.As(err, &refusal) {
		return refused(refusal.code, refusal.message)
	}
	return refused("invalid_tool_input", err.Error())
}

func (c *Core) computerTool(ctx context.Context, key coreKey, params ToolCallParams) (ToolResultParams, error) {
	call, err := c.prepareComputer(key, params.Tool, params.Input)
	if err != nil {
		return ToolResultParams{}, err
	}
	if ctx.Value(computerCheckedKey{}) == nil {
		if err := c.preflightComputer(ctx, call); err != nil {
			return ToolResultParams{}, err
		}
	}
	var blocks []ContentBlock
	if call.tool == ToolNameComputerObserve {
		if blocks, err = c.opts.ComputerUse.Observe(ctx, call.observe); err == nil {
			c.frames.record(key, call.observe, blocks)
			blocks = c.withoutImages(ctx, key.session, blocks)
		}
	} else {
		blocks, err = c.opts.ComputerUse.Action(ctx, call.action)
	}
	if err != nil {
		return ToolResultParams{}, err
	}
	return ToolResultParams{Outcome: ToolOutcomeSucceeded, Content: blocks}, nil
}

// computerFrames are the sessions' last observations: the last one of each
// session and the last one of each window it observed.
type computerFrames struct {
	mu      sync.Mutex
	latest  map[coreKey]ComputerFrame
	windows map[coreKey]map[[2]int]ComputerFrame
}

// record keeps a successful observation's frame; the screenshot size is read
// from the image's header.
func (f *computerFrames) record(key coreKey, req ComputerObserveRequest, blocks []ContentBlock) {
	frame := ComputerFrame{PID: req.PID, WindowID: req.WindowID}
	frame.Width, frame.Height = blocksImageSize(blocks)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.latest == nil {
		f.latest, f.windows = map[coreKey]ComputerFrame{}, map[coreKey]map[[2]int]ComputerFrame{}
	}
	f.latest[key] = frame
	if req.WindowID != 0 {
		if f.windows[key] == nil {
			f.windows[key] = map[[2]int]ComputerFrame{}
		}
		f.windows[key][[2]int{req.PID, req.WindowID}] = frame
	}
}

// last is the session's last observation, nil without one.
func (f *computerFrames) last(key coreKey) *ComputerFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	frame, ok := f.latest[key]
	if !ok {
		return nil
	}
	return &frame
}

// frameFor is the frame of an action: the window's last observation, or the
// session's last one for an action without a window.
func (f *computerFrames) frameFor(key coreKey, pid, windowID int) *ComputerFrame {
	if windowID == 0 {
		return f.last(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	frame, ok := f.windows[key][[2]int{pid, windowID}]
	if !ok {
		return nil
	}
	return &frame
}

func (f *computerFrames) forget(key coreKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.latest, key)
	delete(f.windows, key)
}

// blocksImageSize is the pixel size of the first image block (0, 0 without one).
func blocksImageSize(blocks []ContentBlock) (int, int) {
	for _, block := range blocks {
		if block.Type != ContentBlockTypeImage || block.Data == "" {
			continue
		}
		cfg, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(block.Data)))
		if err != nil {
			return 0, 0
		}
		return cfg.Width, cfg.Height
	}
	return 0, 0
}

func (in computerInput) actionRequest() (ComputerActionRequest, error) {
	request := ComputerActionRequest{
		Action: in.Action, PID: in.PID, WindowID: in.WindowID, Text: in.Text, Button: in.Button,
		ClickCount: in.Count, ScrollDir: in.ScrollDirection, ScrollAmount: in.ScrollAmount,
	}
	point := func(name string, values []float64) (*[2]float64, error) {
		if len(values) != 2 {
			return nil, fmt.Errorf("%s must be [x, y]", name)
		}
		return &[2]float64{values[0], values[1]}, nil
	}
	var err error
	switch in.Action {
	case ComputerActionClick:
		if request.Coordinate, err = point("coordinate", in.Coordinate); err != nil {
			return request, err
		}
		switch in.Button {
		case "", "left", "right", "middle":
		default:
			return request, fmt.Errorf("button %q is not left, right or middle", in.Button)
		}
		if in.Count < 0 || in.Count > 3 {
			return request, errors.New("click_count must be 1-3")
		}
	case ComputerActionType:
		if in.Text == "" {
			return request, errors.New("text is required")
		}
	case ComputerActionKey:
		if strings.TrimSpace(in.Text) == "" {
			return request, errors.New(`text is required: a key or chord such as "Return" or "ctrl+s"`)
		}
	case ComputerActionScroll:
		if request.Coordinate, err = point("coordinate", in.Coordinate); err != nil {
			return request, err
		}
		switch in.ScrollDirection {
		case "up", "down", "left", "right":
		default:
			return request, errors.New("scroll_direction must be up, down, left or right")
		}
		if in.ScrollAmount < 0 || in.ScrollAmount > 50 {
			return request, errors.New("scroll_amount must be 1-50")
		}
	case ComputerActionDrag:
		if request.Start, err = point("start_coordinate", in.StartCoordinate); err != nil {
			return request, err
		}
		if request.Coordinate, err = point("coordinate", in.Coordinate); err != nil {
			return request, err
		}
	case ComputerActionOpenApp:
		request.App = strings.TrimSpace(in.App)
		if request.App == "" || len(request.App) > maxAppName {
			return request, errors.New(`app is required: an app name such as "TextEdit" or a bundle id such as "com.apple.TextEdit"`)
		}
		if in.PID != 0 || in.WindowID != 0 {
			return request, errors.New("open_app takes no pid or window_id")
		}
	case ComputerActionMenu:
		if in.PID == 0 || in.WindowID == 0 {
			return request, errors.New("menu needs the pid and window_id of a window of the app whose menu it runs")
		}
		if len(in.MenuPath) == 0 || len(in.MenuPath) > maxMenuItems {
			return request, fmt.Errorf(`menu_path must be 1-%d menu labels from the menu bar to the item, for example ["Window", "Weixin"]`, maxMenuItems)
		}
		for _, label := range in.MenuPath {
			label = strings.TrimSpace(label)
			if label == "" || utf8.RuneCountInString(label) > maxMenuLabel {
				return request, fmt.Errorf("every menu_path label must be 1-%d characters", maxMenuLabel)
			}
			request.MenuPath = append(request.MenuPath, label)
		}
		if len(in.Coordinate) > 0 || in.Text != "" {
			return request, errors.New("menu takes no coordinate or text")
		}
	default:
		return request, fmt.Errorf("action %q is not click, type, key, scroll, drag, open_app or menu", in.Action)
	}
	return request, nil
}

// --- Cua Driver backend ---

// CuaDriverOptions configures NewCuaDriver.
type CuaDriverOptions struct {
	// Path is the cua-driver executable (default: cua-driver on PATH).
	Path string
	// Socket overrides the daemon's socket or named pipe (default: the
	// driver's own, \\.\pipe\cua-driver on Windows).
	Socket string
	// Env is added to the driver's environment (CUA_TELEMETRY=0 is always set).
	Env []string
	// Timeout bounds one driver call (default 30 s).
	Timeout time.Duration
	// NonASCIIText is how text with non-ASCII characters (Chinese, emoji,
	// ...) is typed into a named window: NonASCIITextPaste (empty: the
	// default), NonASCIITextForeground or NonASCIITextBackground. ASCII
	// text and text typed without a window always go the driver's default
	// way. The default is paste because synthesized key events posted in the
	// background garble such text in Electron and web fields (2026-11-14,
	// Lark: 13 incomplete deliveries in one task, some with the first
	// character moved to the end); a paste is one key chord. It uses the
	// clipboard only when it holds plain text alone (or nothing), which is
	// put back right after; other content is never replaced and the text is
	// typed with NonASCIITextForeground instead. A clipboard history app may
	// record the pasted text: choose foreground or background for text that
	// must not reach the clipboard.
	NonASCIIText string
	// SkipTypeReadback leaves out the read-back of the window after typing
	// (one accessibility read, no screenshot); the result then says
	// unverified unless the driver itself confirmed the text.
	SkipTypeReadback bool
}

// CuaDriver is the ComputerUseBackend on Cua Driver
// (https://github.com/trycua/cua, MIT; only the MIT driver is used, not its
// AGPL perception extension or its FSL space daemon): every call is
// `cua-driver call <tool> <json>` against the driver daemon that runs in the
// interactive session (Windows: a logon scheduled task, `cua-driver autostart
// enable`). The executor itself may run in session 0 or session 1; the daemon
// owns the desktop. Input is delivered in the background when a window is
// named; screenshots and the accessibility tree come from the same daemon.
type CuaDriver struct {
	path, socket     string
	sessionNamespace string
	env              []string
	timeout          time.Duration
	nonASCII         string
	noReadback       bool
}

// NewCuaDriver checks that the driver and its daemon work (a screen-size call)
// and returns the backend, or an error wrapping ErrComputerUseUnavailable.
func NewCuaDriver(ctx context.Context, opts CuaDriverOptions) (*CuaDriver, error) {
	path := opts.Path
	if path == "" {
		path = "cua-driver"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrComputerUseUnavailable, err)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	switch opts.NonASCIIText {
	case "":
		opts.NonASCIIText = NonASCIITextPaste
	case NonASCIITextBackground, NonASCIITextForeground, NonASCIITextPaste:
	default:
		return nil, fmt.Errorf("bmaexecutor: CuaDriverOptions.NonASCIIText %q is not background, foreground or paste", opts.NonASCIIText)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrComputerUseUnavailable, err)
	}
	driver := &CuaDriver{path: resolved, socket: opts.Socket, env: opts.Env, timeout: opts.Timeout, sessionNamespace: fmt.Sprintf("%x", nonce),
		nonASCII: opts.NonASCIIText, noReadback: opts.SkipTypeReadback}
	if _, err := driver.call(ctx, "get_screen_size", map[string]any{}, ""); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrComputerUseUnavailable, err)
	}
	return driver, nil
}

// call runs one tool. With screenshot set the driver writes the response's
// image there; the JSON result is returned.
func (d *CuaDriver) call(ctx context.Context, tool string, args map[string]any, screenshot string) (map[string]any, error) {
	return d.run(ctx, tool, args, screenshot, ComputerActionRequest{})
}

// callAction runs one tool of an action: a structured failure's advice
// names the action's window.
func (d *CuaDriver) callAction(ctx context.Context, tool string, args map[string]any, req ComputerActionRequest) (map[string]any, error) {
	return d.run(ctx, tool, args, "", req)
}

func (d *CuaDriver) run(ctx context.Context, tool string, args map[string]any, screenshot string, req ComputerActionRequest) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	payload, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	argv := []string{"call", tool, string(payload)}
	if screenshot != "" {
		argv = append(argv, "--screenshot-out-file", screenshot)
	}
	if d.socket != "" {
		argv = append(argv, "--socket", d.socket)
	}
	cmd := exec.CommandContext(ctx, d.path, argv...)
	setProcessGroup(cmd)
	cmd.Env = append(append(os.Environ(), "CUA_TELEMETRY=0"), d.env...)
	var stdout, stderr cappedBuffer
	stdout.limit, stderr.limit = 8*1024*1024, 16*1024
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := runCommand(cmd)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("cua-driver %s: %w", tool, ctx.Err())
	}
	if runErr != nil {
		if failure := cuaFailure(tool, bytes.TrimSpace(stdout.buf.Bytes()), runErr, req); failure != nil {
			return nil, failure
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("cua-driver %s: %v: %s", tool, runErr, truncate(message, 1024))
	}
	result := map[string]any{}
	if text := bytes.TrimSpace(stdout.buf.Bytes()); len(text) > 0 {
		if err := json.Unmarshal(text, &result); err != nil {
			return nil, fmt.Errorf("cua-driver %s: undecodable result: %v", tool, err)
		}
	}
	return result, nil
}

func (d *CuaDriver) screenshot(ctx context.Context, tool string, args map[string]any) (map[string]any, ContentBlock, error) {
	dir, err := os.MkdirTemp("", "bma-cua-")
	if err != nil {
		return nil, ContentBlock{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "screen.png")
	result, err := d.call(ctx, tool, args, file)
	if err != nil {
		return nil, ContentBlock{}, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, ContentBlock{}, fmt.Errorf("cua-driver %s returned no screenshot: %w", tool, err)
	}
	return result, ContentBlock{Type: ContentBlockTypeImage, MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(raw)}, nil
}

// Observe returns a screenshot with a text description: the screen size and
// the window list, or one window's accessibility tree (UIA on Windows).
func (d *CuaDriver) Observe(ctx context.Context, req ComputerObserveRequest) ([]ContentBlock, error) {
	if err := validateObserveFormat(req.ImageFormat); err != nil {
		return nil, err
	}
	if req.WindowID != 0 {
		state, image, err := d.observeScreenshot(ctx, req.ImageFormat, "get_window_state", map[string]any{
			// tree_markdown is part of the default response. Do not send
			// tree_format: bundled macOS 0.34.0 rejects that newer option.
			"pid": req.PID, "window_id": req.WindowID, "session": d.windowSession(req.SessionID),
		})
		if err != nil {
			return nil, err
		}
		tree, _ := state["tree_markdown"].(string)
		return []ContentBlock{image, {Type: ContentBlockTypeText, Text: fmt.Sprintf(
			"window pid=%d window_id=%d screenshot %vx%v (coordinates are window-local)\n%s",
			req.PID, req.WindowID, state["screenshot_width"], state["screenshot_height"], tree)}}, nil
	}
	state, image, err := d.observeScreenshot(ctx, req.ImageFormat, "get_desktop_state", map[string]any{})
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "screen screenshot %vx%v (coordinates are screen pixels)\n", state["screenshot_width"], state["screenshot_height"])
	if windows, err := d.call(ctx, "list_windows", map[string]any{}, ""); err == nil {
		text.WriteString(windowList(windows))
	}
	return []ContentBlock{image, {Type: ContentBlockTypeText, Text: text.String()}}, nil
}

// windowList renders list_windows' windows, one per line, for the agent to
// pick a pid and window_id from.
func windowList(result map[string]any) string {
	windows, _ := result["windows"].([]any)
	var out strings.Builder
	for _, entry := range windows {
		window, _ := entry.(map[string]any)
		if window["is_on_screen"] == false || window["minimized"] == true || window["on_current_space"] == false {
			continue
		}
		bounds, _ := window["bounds"].(map[string]any)
		fmt.Fprintf(&out, "window pid=%v window_id=%v app=%v title=%q at (%v,%v) %vx%v\n",
			window["pid"], window["window_id"], window["app_name"], window["title"],
			bounds["x"], bounds["y"], bounds["width"], bounds["height"])
	}
	return out.String()
}

// Each one-shot CLI call discards its anonymous snapshot context. A stable,
// non-secret label keeps the window pixel frame alive for the next action.
// Hash the Core session ID and namespace it per driver to isolate sessions.
func (d *CuaDriver) windowSession(sessionID string) string {
	sum := sha256.Sum256([]byte(d.sessionNamespace + "\x00" + sessionID))
	return fmt.Sprintf("bma-%x", sum[:16])
}

// Action performs one computer_action and returns the driver's summary,
// with the driver's advice in terms of the computer use tools.
func (d *CuaDriver) Action(ctx context.Context, req ComputerActionRequest) ([]ContentBlock, error) {
	switch req.Action {
	case ComputerActionOpenApp:
		result, err := d.OpenApp(ctx, req)
		if err != nil {
			return nil, err
		}
		return []ContentBlock{{Type: ContentBlockTypeText, Text: result.text()}}, nil
	case ComputerActionMenu:
		return d.menu(ctx, req)
	case ComputerActionType:
		return d.typeText(ctx, req)
	}
	tool, args := cuaAction(req)
	if req.WindowID != 0 {
		args["session"] = d.windowSession(req.SessionID)
	}
	result, err := d.callAction(ctx, tool, args, req)
	if err != nil {
		return nil, err
	}
	fallback := map[string]any{}
	for key, value := range result {
		switch key {
		case "hint", "next_call", "escalation":
		default:
			fallback[key] = value
		}
	}
	raw, _ := json.Marshal(fallback)
	return []ContentBlock{{Type: ContentBlockTypeText, Text: cuaResultText(result, string(raw))}}, nil
}

// looksLikeBundleID tells a bundle id ("com.apple.TextEdit") from an app name.
func looksLikeBundleID(app string) bool {
	return strings.Contains(app, ".") && !strings.ContainsAny(app, " /") && !strings.HasSuffix(strings.ToLower(app), ".app")
}

// cuaAction maps a computer_action to the driver tool and arguments. Without a
// window the action uses the desktop scope (screen pixels, focused app).
func cuaAction(req ComputerActionRequest) (string, map[string]any) {
	args := map[string]any{}
	if req.WindowID != 0 {
		args["pid"], args["window_id"] = req.PID, req.WindowID
	} else {
		args["scope"] = "desktop"
	}
	switch req.Action {
	case ComputerActionClick:
		args["x"], args["y"] = req.Coordinate[0], req.Coordinate[1]
		if req.Button != "" {
			args["button"] = req.Button
		}
		if req.ClickCount > 1 {
			args["count"] = req.ClickCount
		}
		return "click", args
	case ComputerActionType:
		args["text"] = req.Text
		return "type_text", args
	case ComputerActionKey:
		keys := strings.Split(strings.ToLower(strings.ReplaceAll(strings.TrimSpace(req.Text), " ", "")), "+")
		if len(keys) == 1 {
			args["key"] = keys[0]
			return "press_key", args
		}
		args["keys"] = keys
		return "hotkey", args
	case ComputerActionScroll:
		args["x"], args["y"] = req.Coordinate[0], req.Coordinate[1]
		args["direction"] = req.ScrollDir
		if req.ScrollAmount > 0 {
			args["amount"] = req.ScrollAmount
		}
		return "scroll", args
	default: // drag
		args["from_x"], args["from_y"] = req.Start[0], req.Start[1]
		args["to_x"], args["to_y"] = req.Coordinate[0], req.Coordinate[1]
		return "drag", args
	}
}

// Viewer encoding is opt-in; ordinary model observations remain unchanged.
func validateObserveFormat(format string) error {
	switch format {
	case "", "png", "jpeg":
		return nil
	}
	return errors.New("image_format must be png or jpeg")
}

func (d *CuaDriver) observeScreenshot(ctx context.Context, format, tool string, args map[string]any) (map[string]any, ContentBlock, error) {
	state, block, err := d.screenshot(ctx, tool, args)
	if err != nil {
		return nil, ContentBlock{}, err
	}
	block, err = encodeObservationImage(block, format)
	return state, block, err
}

func encodeObservationImage(block ContentBlock, format string) (ContentBlock, error) {
	if err := validateObserveFormat(format); err != nil {
		return ContentBlock{}, err
	}
	if format != "jpeg" {
		return block, nil
	}
	if block.MimeType != "image/png" || len(block.Data) > 24*1024*1024 {
		return ContentBlock{}, errors.New("invalid observation PNG")
	}
	raw, err := base64.StdEncoding.DecodeString(block.Data)
	if err != nil {
		return ContentBlock{}, errors.New("invalid observation encoding")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 16384 || cfg.Height > 16384 || int64(cfg.Width)*int64(cfg.Height) > 64*1024*1024 {
		return ContentBlock{}, errors.New("invalid observation dimensions")
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return ContentBlock{}, errors.New("invalid observation PNG")
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 75}); err != nil {
		return ContentBlock{}, err
	}
	block.MimeType = "image/jpeg"
	block.Data = base64.StdEncoding.EncodeToString(out.Bytes())
	return block, nil
}
