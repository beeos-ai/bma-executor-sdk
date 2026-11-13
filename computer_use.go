package bmaexecutor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Computer use (computer_observe / computer_action) is an optional capability
// served by a ComputerUseBackend that CoreOptions.ComputerUse names. The
// executor advertises computer_use only when it has a backend, so a machine
// without one (no desktop session, no Cua Driver) stays honest and the harness
// never sends it a computer_* tool.
//
// The tool inputs follow Anthropic's computer-use tool (coordinate,
// start_coordinate, text, scroll_direction, scroll_amount) over the contract's
// action enum (click, type, key, scroll, drag); coordinates are pixels of the
// screenshot computer_observe returned. pid and window_id (additive, from
// computer_observe's window list) target one window: input is then delivered to
// it in the background and coordinates are window-local.

// ErrComputerUseUnavailable is returned by NewCuaDriver when the driver or its
// interactive-session daemon cannot be used; leave CoreOptions.ComputerUse nil
// then.
var ErrComputerUseUnavailable = errors.New("bmaexecutor: computer use backend is unavailable")

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
// computer use call. No approver, or an input the tool would refuse, is asked.
func (c *Core) computerNeedsApproval(ctx context.Context, sessionID, tool string, raw map[string]any) bool {
	approver, ok := c.opts.ComputerUse.(ComputerUseApprover)
	if !ok {
		return true
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return true
	}
	var input computerInput
	if json.Unmarshal(payload, &input) != nil || (input.PID == 0) != (input.WindowID == 0) || (input.Display != "" && input.Display != "primary") {
		return true
	}
	switch tool {
	case ToolNameComputerObserve:
		if validateObserveFormat(input.ImageFormat) != nil {
			return true
		}
		return approver.NeedsObserveApproval(ctx, ComputerObserveRequest{SessionID: sessionID, PID: input.PID, WindowID: input.WindowID, ImageFormat: input.ImageFormat})
	case ToolNameComputerAction:
		request, err := input.actionRequest()
		if err != nil {
			return true
		}
		request.SessionID = sessionID
		return approver.NeedsActionApproval(ctx, request)
	}
	return true
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
}

func (c *Core) computerTool(ctx context.Context, params ToolCallParams) (ToolResultParams, error) {
	var input computerInput
	if len(params.Input) > 0 {
		if err := json.Unmarshal(params.Input, &input); err != nil {
			return ToolResultParams{}, fmt.Errorf("invalid tool input: %w", err)
		}
	}
	if input.Display != "" && input.Display != "primary" {
		return ToolResultParams{}, fmt.Errorf("display %q is not available: only the primary display is", input.Display)
	}
	if (input.PID == 0) != (input.WindowID == 0) {
		return ToolResultParams{}, errors.New("pid and window_id go together")
	}
	var blocks []ContentBlock
	var err error
	if params.Tool == ToolNameComputerObserve {
		if err := validateObserveFormat(input.ImageFormat); err != nil {
			return ToolResultParams{}, err
		}
		blocks, err = c.opts.ComputerUse.Observe(ctx, ComputerObserveRequest{SessionID: params.SessionID, PID: input.PID, WindowID: input.WindowID, ImageFormat: input.ImageFormat})
	} else {
		var request ComputerActionRequest
		if request, err = input.actionRequest(); err == nil {
			request.SessionID = params.SessionID
			blocks, err = c.opts.ComputerUse.Action(ctx, request)
		}
	}
	if err != nil {
		return ToolResultParams{}, err
	}
	return ToolResultParams{Outcome: ToolOutcomeSucceeded, Content: blocks}, nil
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
	default:
		return request, fmt.Errorf("action %q is not click, type, key, scroll or drag", in.Action)
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
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrComputerUseUnavailable, err)
	}
	driver := &CuaDriver{path: resolved, socket: opts.Socket, env: opts.Env, timeout: opts.Timeout, sessionNamespace: fmt.Sprintf("%x", nonce)}
	if _, err := driver.call(ctx, "get_screen_size", map[string]any{}, ""); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrComputerUseUnavailable, err)
	}
	return driver, nil
}

// call runs one tool. With screenshot set the driver writes the response's
// image there; the JSON result is returned.
func (d *CuaDriver) call(ctx context.Context, tool string, args map[string]any, screenshot string) (map[string]any, error) {
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
		if window["is_on_screen"] == false || window["minimized"] == true {
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

// Action performs one computer_action and returns the driver's summary.
func (d *CuaDriver) Action(ctx context.Context, req ComputerActionRequest) ([]ContentBlock, error) {
	tool, args := cuaAction(req)
	if req.WindowID != 0 {
		args["session"] = d.windowSession(req.SessionID)
	}
	result, err := d.call(ctx, tool, args, "")
	if err != nil {
		return nil, err
	}
	summary, _ := result["summary"].(string)
	if summary == "" {
		raw, _ := json.Marshal(result)
		summary = string(raw)
	}
	if hint, _ := result["hint"].(string); hint != "" {
		summary += "\n" + hint
	}
	return []ContentBlock{{Type: ContentBlockTypeText, Text: summary}}, nil
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
