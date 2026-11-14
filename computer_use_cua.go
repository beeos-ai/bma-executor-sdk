package bmaexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// What the Cua Driver backend adds around single driver calls: the menu
// action, open_app's recovery when the app shows no usable window, typing
// with a read-back and an optional route for non-ASCII text, and driver
// advice rewritten into what the model can call.

// cuaToolActions maps every Cua Driver tool the model reaches through the
// computer use tools to what it calls for it. Driver advice that names a
// tool not in this table (run_steps, get_browser_state, set_value, ...) is
// dropped, because the model cannot call it; advice naming one in it is
// rewritten. Add a row with each new action.
var cuaToolActions = map[string]string{
	"click":             "computer_action click",
	"double_click":      "computer_action click with click_count 2",
	"right_click":       "computer_action click with button right",
	"type_text":         "computer_action type",
	"press_key":         "computer_action key",
	"hotkey":            "computer_action key",
	"scroll":            "computer_action scroll",
	"drag":              "computer_action drag",
	"launch_app":        "computer_action open_app",
	"bring_to_front":    "computer_action open_app",
	"invoke_menu":       "computer_action menu",
	"get_window_state":  "computer_observe with pid and window_id",
	"get_desktop_state": "computer_observe",
	"list_windows":      "computer_observe",
}

// cuaHiddenTerms are driver tools and inputs the model cannot use: a clause
// of driver advice that mentions one is dropped.
var cuaHiddenTerms = []string{
	"run_steps", "run_script", "delivery_mode", "element_token", "get_browser_state", "browser_", "browser tools",
	"`page`", `"page"`, "page tool", "set_value", "verify_state", "clipboard_", "zoom", "start_session", "end_session",
	"set_window_frame", "get_accessibility_tree", "snapshot", "since:", "tree_format", "max_elements", "include_screenshot",
	"target {", "target:{", "kind:", "kind=", "escalat", "move_cursor", "get_guide", "list_apps", "kill_app", "x,y", "x, y",
	"px action", "ax action", "next_call",
}

var cuaToolName = func() *regexp.Regexp {
	names := make([]string, 0, len(cuaToolActions))
	for name := range cuaToolActions {
		names = append(names, regexp.QuoteMeta(name))
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	return regexp.MustCompile("`?\\b(" + strings.Join(names, "|") + ")\\b`?")
}()

// sanitizeCuaAdvice keeps the clauses of driver text that name only what the
// model can call, with driver tool names rewritten to its actions.
func sanitizeCuaAdvice(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		var kept []string
		for _, clause := range cuaClauses(line) {
			lower := strings.ToLower(clause)
			hidden := false
			for _, term := range cuaHiddenTerms {
				if strings.Contains(lower, term) {
					hidden = true
					break
				}
			}
			if hidden {
				continue
			}
			kept = append(kept, cuaToolName.ReplaceAllStringFunc(clause, func(m string) string {
				name := strings.Trim(m, "`")
				if name == m && !strings.Contains(name, "_") {
					return m // a plain word such as "click", not a tool name
				}
				return cuaToolActions[name]
			}))
		}
		if len(kept) > 0 {
			lines = append(lines, strings.Join(kept, " "))
		}
	}
	return strings.Join(lines, "\n")
}

// cuaClauses splits a line after ". ", "; ", "! ", "? " and at " — ".
func cuaClauses(line string) []string {
	var out []string
	for line != "" {
		cut, next := -1, 0
		for _, sep := range []string{". ", "; ", "! ", "? ", " — "} {
			if i := strings.Index(line, sep); i >= 0 && (cut < 0 || i < cut) {
				cut, next = i, i+len(sep)
				if sep != " — " {
					cut++ // keep the punctuation
				}
			}
		}
		if cut < 0 {
			if clause := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "—")); clause != "" {
				out = append(out, clause)
			}
			break
		}
		if clause := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[:cut]), "—")); clause != "" {
			out = append(out, clause)
		}
		line = line[next:]
	}
	return out
}

// cuaErrorAdvice is what the model reads for a driver refusal with a known
// code, in place of the driver's advice (which names tools it cannot call).
func cuaErrorAdvice(code string, result map[string]any, req ComputerActionRequest) string {
	window := req.WindowID
	if id, ok := result["window_id"].(float64); ok && id > 0 {
		window = int(id)
	}
	switch code {
	case "background_unavailable":
		return "the point is over another app or a part of the window that needs the real pointer. Observe the target window, then act with its pid and window_id and pixels from that window's screenshot."
	case "off_space_or_ax_unresolved":
		return fmt.Sprintf("window %d is not on the current desktop (minimized, hidden or on another Space). Bring it back with computer_action open_app, or open a window with computer_action menu (for example the app's Window menu), then observe again.", window)
	case "screenshot_context_missing", "zoom_context_missing":
		return "this window's screenshot is no longer current. Observe it again with computer_observe pid and window_id, then act with pixels from that screenshot."
	case "window_id_not_found":
		return fmt.Sprintf("window %d no longer exists. Observe the screen to find the app's windows.", window)
	case "window_owner_pid_mismatch":
		if owner, ok := result["owner_pid"].(float64); ok {
			return fmt.Sprintf("window %d belongs to pid %d. Use that pid with this window_id.", window, int(owner))
		}
		return fmt.Sprintf("window %d belongs to another process. Observe the screen and use the pid of its window line.", window)
	case "no_focused_text_input":
		return "no text field has the focus. Click the field first, then type."
	case "type_text_incomplete":
		return "the driver could not confirm that all of the text landed; part of it may have. Observe the field before typing again, and do not resend the whole text."
	}
	return ""
}

// cuaRejectedCodes are structured refusals of the driver contract that
// deliver no input; a result with effect "refused" is one as well.
var cuaRejectedCodes = map[string]bool{
	"invalid_arguments": true, "desktop_scope_disabled": true, "stale_element_token": true,
	"screenshot_context_missing": true, "zoom_context_missing": true,
}

// CuaDriverError is a structured driver failure (exit status 1 with a JSON
// result that has a code). Its text is "<code>: <advice>", with the advice
// in terms of the computer use tools. A refusal that delivered no input
// (Is ErrComputerActionRejected) is told apart from a failure that may have.
type CuaDriverError struct {
	Tool, Code string
	rejected   bool
	text       string
}

func (e *CuaDriverError) Error() string { return e.text }

// Is reports ErrComputerActionRejected for a refusal before input.
func (e *CuaDriverError) Is(target error) bool {
	return target == ErrComputerActionRejected && e.rejected
}

// newCuaDriverError builds the error of a structured driver failure; ok is
// false when stdout is not one.
func newCuaDriverError(tool string, stdout []byte, req ComputerActionRequest) (*CuaDriverError, bool) {
	var result map[string]any
	if json.Unmarshal(stdout, &result) != nil {
		return nil, false
	}
	code, _ := result["code"].(string)
	if code == "" {
		if e, _ := result["error"].(map[string]any); e != nil {
			code, _ = e["code"].(string)
		}
	}
	if code == "" {
		return nil, false
	}
	effect, _ := result["effect"].(string)
	advice := cuaErrorAdvice(code, result, req)
	if advice == "" {
		advice = cuaResultText(result, "")
	}
	if advice == "" {
		advice = "the driver refused the action"
	}
	return &CuaDriverError{Tool: tool, Code: code, rejected: cuaRejectedCodes[code] || effect == "refused", text: code + ": " + advice}, true
}

// cuaResultText is a driver result for the model: its summary (or reason or
// message) and hint as advice it can follow, and a next call it can make.
func cuaResultText(result map[string]any, fallback string) string {
	var parts []string
	for _, key := range []string{"summary", "reason", "message"} {
		if s, _ := result[key].(string); s != "" {
			if s = sanitizeCuaAdvice(s); s != "" {
				parts = append(parts, s)
			}
			break
		}
	}
	if e, _ := result["error"].(map[string]any); e != nil && len(parts) == 0 {
		if s, _ := e["message"].(string); s != "" {
			parts = append(parts, sanitizeCuaAdvice(s))
		}
	}
	if s, _ := result["hint"].(string); s != "" {
		if s = sanitizeCuaAdvice(s); s != "" {
			parts = append(parts, s)
		}
	}
	if next, _ := result["next_call"].(map[string]any); next != nil {
		tool, _ := next["tool"].(string)
		if action, ok := cuaToolActions[tool]; ok {
			line := "Next: " + action
			if args, _ := next["arguments"].(map[string]any); args != nil {
				if pid, ok := args["pid"].(float64); ok {
					line += fmt.Sprintf(" pid=%d", int(pid))
				}
				if window, ok := args["window_id"].(float64); ok {
					line += fmt.Sprintf(" window_id=%d", int(window))
				}
			}
			parts = append(parts, line+".")
		}
	}
	if len(parts) == 0 {
		return fallback
	}
	return strings.Join(parts, "\n")
}

// --- menu ---

func (d *CuaDriver) menu(ctx context.Context, req ComputerActionRequest) ([]ContentBlock, error) {
	result, err := d.callAction(ctx, "invoke_menu", map[string]any{
		"pid": req.PID, "window_id": req.WindowID, "path": req.MenuPath, "session": d.windowSession(req.SessionID),
	}, req)
	if err != nil {
		return nil, err
	}
	return []ContentBlock{{Type: ContentBlockTypeText, Text: cuaResultText(result, "ran menu item "+strings.Join(req.MenuPath, " > "))}}, nil
}

// --- open_app ---

// ComputerWindow is a window open_app found on the current desktop.
type ComputerWindow struct {
	PID, WindowID       int
	App, Title          string
	X, Y, Width, Height float64
}

// OpenAppResult is what CuaDriver.OpenApp did.
type OpenAppResult struct {
	PID            int
	BundleID, Name string
	// Front reports that the app was brought to the front; FrontError is
	// the reason it was not.
	Front      bool
	FrontError string
	// Recovery are the steps taken because the app was not brought to the
	// front or showed no usable window, in order.
	Recovery []string
	// Windows are the app's windows on the current desktop: the ones the
	// model can observe and act on. Hidden counts the others (minimized,
	// hidden or on another Space).
	Windows []ComputerWindow
	Hidden  int
}

// OpenApp launches the app (launch_app starts it in the background) and
// brings it to the front. When that fails or the app has no window on the
// current desktop it recovers, in order: it opens the app again (an app that
// runs without a window shows its main window then) and brings it to the
// front, then brings its largest hidden window forward. Only windows on the
// current desktop are returned.
func (d *CuaDriver) OpenApp(ctx context.Context, req ComputerActionRequest) (OpenAppResult, error) {
	args := map[string]any{"name": req.App}
	if looksLikeBundleID(req.App) {
		args = map[string]any{"bundle_id": req.App}
	}
	launched, err := d.callAction(ctx, "launch_app", args, req)
	if err != nil {
		return OpenAppResult{}, err
	}
	pid, _ := launched["pid"].(float64)
	if pid <= 0 {
		return OpenAppResult{}, fmt.Errorf("cua-driver launch_app: no pid for %q", req.App)
	}
	out := OpenAppResult{PID: int(pid)}
	out.Name, _ = launched["name"].(string)
	out.BundleID, _ = launched["bundle_id"].(string)
	front := func(args map[string]any) {
		if _, err := d.callAction(ctx, "bring_to_front", args, req); err != nil {
			out.Front, out.FrontError = false, truncate(err.Error(), 200)
		} else {
			out.Front, out.FrontError = true, ""
		}
	}
	front(map[string]any{"pid": out.PID})
	visible, hidden := splitWindows(launched, out.PID)
	if !out.Front || len(visible) == 0 {
		if _, err := d.callAction(ctx, "launch_app", args, req); err == nil {
			out.Recovery = append(out.Recovery, "opened the app again")
		}
		front(map[string]any{"pid": out.PID})
		if listed, err := d.call(ctx, "list_windows", map[string]any{"pid": out.PID}, ""); err == nil {
			visible, hidden = splitWindows(listed, out.PID)
		}
		if len(visible) == 0 && len(hidden) > 0 {
			sort.SliceStable(hidden, func(i, j int) bool { return hidden[i].Width*hidden[i].Height > hidden[j].Width*hidden[j].Height })
			front(map[string]any{"pid": out.PID, "window_id": hidden[0].WindowID})
			if out.Front {
				out.Recovery = append(out.Recovery, fmt.Sprintf("brought window %d forward", hidden[0].WindowID))
			}
			if listed, err := d.call(ctx, "list_windows", map[string]any{"pid": out.PID}, ""); err == nil {
				visible, hidden = splitWindows(listed, out.PID)
			}
		}
	}
	out.Windows, out.Hidden = visible, len(hidden)
	return out, nil
}

// splitWindows sorts a result's windows of pid into those on the current
// desktop and the others. Only ordinary (layer 0) windows count.
func splitWindows(result map[string]any, pid int) (visible, hidden []ComputerWindow) {
	windows, _ := result["windows"].([]any)
	for _, entry := range windows {
		w, _ := entry.(map[string]any)
		if p, _ := w["pid"].(float64); int(p) != pid {
			continue
		}
		if layer, ok := w["layer"].(float64); ok && layer != 0 {
			continue
		}
		id, _ := w["window_id"].(float64)
		window := ComputerWindow{PID: pid, WindowID: int(id)}
		window.App, _ = w["app_name"].(string)
		window.Title, _ = w["title"].(string)
		if b, _ := w["bounds"].(map[string]any); b != nil {
			window.X, _ = b["x"].(float64)
			window.Y, _ = b["y"].(float64)
			window.Width, _ = b["width"].(float64)
			window.Height, _ = b["height"].(float64)
		}
		if w["is_on_screen"] == false || w["minimized"] == true || w["on_current_space"] == false {
			hidden = append(hidden, window)
		} else {
			visible = append(visible, window)
		}
	}
	return visible, hidden
}

// text renders an OpenAppResult for the model; the first line keeps the
// format "opened <name> (<bundle id>) pid=<pid>, ...".
func (r OpenAppResult) text() string {
	var out strings.Builder
	front := "brought to the front"
	if !r.Front {
		front = "not brought to the front: " + r.FrontError
	}
	fmt.Fprintf(&out, "opened %s (%s) pid=%d, %s\n", r.Name, r.BundleID, r.PID, front)
	if len(r.Recovery) > 0 {
		fmt.Fprintf(&out, "recovered: %s\n", strings.Join(r.Recovery, ", "))
	}
	for _, w := range r.Windows {
		fmt.Fprintf(&out, "window pid=%d window_id=%d app=%s title=%q at (%v,%v) %vx%v\n", w.PID, w.WindowID, w.App, w.Title, w.X, w.Y, w.Width, w.Height)
	}
	if r.Hidden > 0 {
		fmt.Fprintf(&out, "not usable (minimized, hidden or on another Space): %d window(s)\n", r.Hidden)
	}
	return out.String()
}

// --- typing ---

// Values of CuaDriverOptions.NonASCIIText: how text with non-ASCII
// characters (Chinese, emoji, ...) is typed into a named window.
const (
	// NonASCIITextBackground leaves it to the driver: accessibility
	// insertion, else synthesized key events posted to the app in the
	// background (no focus change). Unreliable in Electron and web fields.
	NonASCIITextBackground = "background"
	// NonASCIITextForeground has the driver bring the window to the front
	// briefly, type and give the front back.
	NonASCIITextForeground = "foreground"
	// NonASCIITextPaste (the default) puts the text on the clipboard, pastes
	// it into the window and puts the previous clipboard back. Only when the clipboard
	// holds plain text alone (or nothing), which can be put back exactly;
	// otherwise the text is typed as with NonASCIITextForeground.
	NonASCIITextPaste = "paste"
)

// plainTextTypes are clipboard types that a plain-text write recreates.
var plainTextTypes = map[string]bool{
	"public.utf8-plain-text": true, "public.utf16-plain-text": true, "public.utf16-external-plain-text": true,
	"public.plain-text": true, "NSStringPboardType": true, "text/plain": true, "CF_UNICODETEXT": true, "CF_TEXT": true,
}

func hasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// typeText types req.Text and adds a verdict line: "typed: verified",
// "typed: mismatch — ..." or "typed: unverified — ...". The verdict never
// contains the text or a field's value.
func (d *CuaDriver) typeText(ctx context.Context, req ComputerActionRequest) ([]ContentBlock, error) {
	tool, args := cuaAction(req)
	if req.WindowID != 0 {
		args["session"] = d.windowSession(req.SessionID)
	}
	route := NonASCIITextBackground
	if req.WindowID != 0 && hasNonASCII(req.Text) {
		route = d.nonASCII
	}
	var result map[string]any
	var err error
	var note string
	if route == NonASCIITextPaste {
		var pasted bool
		if result, note, pasted, err = d.paste(ctx, req); !pasted && err == nil {
			route = NonASCIITextForeground
		}
	}
	if route == NonASCIITextForeground {
		args["delivery_mode"] = "foreground"
	}
	if route != NonASCIITextPaste {
		result, err = d.callAction(ctx, tool, args, req)
	}
	var driverErr *CuaDriverError
	incomplete := err != nil && asCuaDriverError(err, &driverErr) && driverErr.Code == "type_text_incomplete"
	if err != nil && !incomplete {
		return nil, err
	}
	verdict := d.typedVerdict(ctx, req, result)
	if incomplete && !strings.HasPrefix(verdict, "verified") {
		return nil, fmt.Errorf("%s\ntyped: %s", driverErr.Error(), verdict)
	}
	text := cuaResultText(result, fmt.Sprintf("typed %d character(s)", len([]rune(req.Text))))
	if incomplete {
		text = "the driver reported an incomplete delivery, but the field shows the text"
	}
	if note != "" {
		text += "\n" + note
	}
	return []ContentBlock{{Type: ContentBlockTypeText, Text: text + "\ntyped: " + verdict}}, nil
}

func asCuaDriverError(err error, target **CuaDriverError) bool {
	e, ok := err.(*CuaDriverError)
	if ok {
		*target = e
	}
	return ok
}

// paste pastes req.Text through the clipboard and puts the previous
// clipboard back. pasted is false (and nothing was changed) when the
// clipboard holds more than plain text, which a plain-text write could not
// put back.
func (d *CuaDriver) paste(ctx context.Context, req ComputerActionRequest) (result map[string]any, note string, pasted bool, err error) {
	previous, err := d.call(ctx, "clipboard_read", map[string]any{"include_text": true}, "")
	if err != nil {
		return nil, "", false, nil
	}
	types, _ := previous["types"].([]any)
	for _, t := range types {
		if name, _ := t.(string); !plainTextTypes[name] {
			return nil, "", false, nil
		}
	}
	before, _ := previous["text"].(string)
	if _, err := d.call(ctx, "clipboard_write", map[string]any{"text": req.Text}, ""); err != nil {
		return nil, "", false, nil
	}
	modifier := "cmd"
	if runtime.GOOS != "darwin" {
		modifier = "ctrl"
	}
	result, err = d.callAction(ctx, "hotkey", map[string]any{"keys": []string{modifier, "v"}, "pid": req.PID, "window_id": req.WindowID,
		"session": d.windowSession(req.SessionID)}, req)
	if _, restoreErr := d.call(ctx, "clipboard_write", map[string]any{"text": before}, ""); restoreErr != nil {
		note = "the clipboard could not be put back: it holds the typed text"
	}
	if err != nil {
		return nil, note, true, err
	}
	result["summary"] = fmt.Sprintf("pasted %d character(s)", len([]rune(req.Text)))
	delete(result, "hint")
	return result, note, true, nil
}

// typedVerdict says whether the text is in the window: verified when the
// driver read it back or the window's accessibility tree shows it in a
// native field, mismatch when a window without web content shows it in no
// field, unverified otherwise (web content's accessibility values do not
// prove that the page received the text, and the screen has no field to read).
func (d *CuaDriver) typedVerdict(ctx context.Context, req ComputerActionRequest, result map[string]any) string {
	if effect, _ := result["effect"].(string); effect == "confirmed" {
		return "verified"
	}
	if req.WindowID == 0 {
		return "unverified — typed into the front app; check the screenshot before typing again"
	}
	if d.noReadback {
		return "unverified — observe the field before typing again"
	}
	state, err := d.call(ctx, "get_window_state", map[string]any{
		"pid": req.PID, "window_id": req.WindowID, "session": d.windowSession(req.SessionID),
		"include_screenshot": false, "tree_format": "elements", "max_elements": 2000,
	}, "")
	if err != nil {
		return "unverified — the field could not be read back; observe it before typing again"
	}
	want := strings.ReplaceAll(req.Text, "\r\n", "\n")
	web, inWeb := false, false
	var closest string // the editable field's value that starts most like the text
	var closestWeb bool
	best := 0
	elements, _ := state["elements"].([]any)
	for _, entry := range elements {
		element, _ := entry.(map[string]any)
		isWeb := element["in_web_content"] == true
		web = web || isWeb
		value, _ := element["value"].(string)
		value = strings.ReplaceAll(value, "\r\n", "\n")
		if value == "" {
			continue
		}
		if strings.Contains(value, want) {
			if !isWeb {
				return "verified"
			}
			inWeb = true
			continue
		}
		if role, _ := element["role"].(string); editableRoles[role] {
			if n := sharedRunes(value, want); n > best {
				best, closest, closestWeb = n, value, isWeb
			}
		}
	}
	switch {
	case inWeb:
		return "unverified — the window's accessibility tree shows the text, but web content cannot prove the app received it; check the screenshot"
	case closest != "":
		note := ""
		if closestWeb {
			note = " (read from the accessibility tree, which can lag behind a web page)"
		}
		return fmt.Sprintf("mismatch — the field shows %q%s; observe it, then fix only what differs, and do not resend the whole text",
			truncateRunes(closest, 200), note)
	case web || state["truncated"] == true || len(elements) == 0:
		return "unverified — the field could not be read back; observe it before typing again"
	}
	return "mismatch — no field of this window shows the text; observe it before typing again, part of it may have landed"
}

// editableRoles are accessibility roles of fields text is typed into.
var editableRoles = map[string]bool{"AXTextField": true, "AXTextArea": true, "AXComboBox": true, "AXSearchField": true, "Edit": true, "Document": true}

// sharedRunes is the length in runes of the longest start of want that value
// contains (how much of the text landed). A value that contains a start
// contains every shorter one, so it is a binary search.
func sharedRunes(value, want string) int {
	w := []rune(want)
	lo, hi := 0, len(w)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if strings.Contains(value, string(w[:mid])) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
