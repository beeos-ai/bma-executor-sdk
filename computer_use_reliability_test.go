package bmaexecutor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// recordingComputer is a ComputerUseBackend that is also a
// ComputerUseApprover and a ComputerUsePreflighter, and records who was
// asked, in order.
type recordingComputer struct {
	mu        sync.Mutex
	calls     []string
	actions   []ComputerActionRequest
	preflight func(ComputerActionRequest) error
}

func (c *recordingComputer) note(s string) {
	c.mu.Lock()
	c.calls = append(c.calls, s)
	c.mu.Unlock()
}

func (c *recordingComputer) Observe(context.Context, ComputerObserveRequest) ([]ContentBlock, error) {
	c.note("observe")
	return []ContentBlock{{Type: ContentBlockTypeImage, MimeType: "image/png", Data: hostToolPNG}, {Type: ContentBlockTypeText, Text: "screen screenshot 1x1"}}, nil
}

func (c *recordingComputer) Action(_ context.Context, req ComputerActionRequest) ([]ContentBlock, error) {
	c.note("action")
	c.mu.Lock()
	c.actions = append(c.actions, req)
	c.mu.Unlock()
	return []ContentBlock{{Type: ContentBlockTypeText, Text: "clicked"}}, nil
}

func (c *recordingComputer) NeedsObserveApproval(context.Context, ComputerObserveRequest) bool {
	c.note("approve")
	return false
}

func (c *recordingComputer) NeedsActionApproval(context.Context, ComputerActionRequest) bool {
	c.note("approve")
	return false
}

func (c *recordingComputer) PreflightObserve(context.Context, ComputerObserveRequest) error {
	c.note("preflight")
	return nil
}

func (c *recordingComputer) PreflightAction(_ context.Context, req ComputerActionRequest) error {
	c.note("preflight")
	if c.preflight != nil {
		return c.preflight(req)
	}
	return nil
}

func (c *recordingComputer) seen() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := strings.Join(c.calls, ",")
	c.calls = nil
	return out
}

// TestComputerPreflightRefusesBeforeTheBackend: on a platform brain's
// tool.call (approved on the platform before it arrives) the preflighter
// runs after the input checks and before the backend; its refusal is the
// tool result, in "<code>: <message>" form, and nothing runs.
func TestComputerPreflightRefusesBeforeTheBackend(t *testing.T) {
	computer := &recordingComputer{}
	session, _ := computerSession(t, CoreOptions{ComputerUse: computer})
	click := map[string]any{"action": "click", "coordinate": []int{1, 2}}
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"reason error": {NewReasonError(CodeForbidden, "computer_use_denied_app", "Terminal is off limits"), "computer_use_denied_app: Terminal is off limits"},
		"coded text":   {errors.New("front_app_changed: the front app changed"), "front_app_changed: the front app changed"},
		"plain text":   {errors.New("Not now."), "computer_use_refused: Not now."},
	} {
		t.Run(name, func(t *testing.T) {
			computer.preflight = func(ComputerActionRequest) error { return c.err }
			result := callComputer(t, session, ToolNameComputerAction, click)
			if !result.IsError || textOf(result) != c.want {
				t.Fatalf("result = %+v, want %q", result, c.want)
			}
			if got := computer.seen(); got != "preflight" {
				t.Fatalf("calls = %s, want preflight only", got)
			}
		})
	}
	computer.preflight = func(req ComputerActionRequest) error {
		if req.SessionID != "sess_cu" {
			t.Errorf("preflight session = %q", req.SessionID)
		}
		return nil
	}
	if result := callComputer(t, session, ToolNameComputerAction, click); result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if got := computer.seen(); got != "preflight,action" {
		t.Fatalf("calls = %s", got)
	}
	// A bad input is refused before the preflighter.
	if result := callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "click"}); !result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if got := computer.seen(); got != "" {
		t.Fatalf("calls = %s, want none", got)
	}
}

// TestComputerFrameCheck: an action's frame is the session's last
// observation (of its window, when it names one); a click without a window
// after a window observation is refused before the preflighter.
func TestComputerFrameCheck(t *testing.T) {
	computer := &recordingComputer{}
	session, _ := computerSession(t, CoreOptions{ComputerUse: computer})
	act := func(input map[string]any) ToolResultParams {
		return callComputer(t, session, ToolNameComputerAction, input)
	}
	if r := act(map[string]any{"action": "click", "coordinate": []int{1, 2}}); r.IsError {
		t.Fatalf("click before any observation = %+v", r)
	}
	callComputer(t, session, ToolNameComputerObserve, map[string]any{"pid": 7, "window_id": 9})
	computer.seen()
	r := act(map[string]any{"action": "click", "coordinate": []int{500, 300}})
	if !r.IsError || !strings.HasPrefix(textOf(r), "window_frame_required: the last observation was window pid=7 window_id=9") {
		t.Fatalf("untargeted click after a window observation = %+v", r)
	}
	for _, input := range []map[string]any{
		{"action": "scroll", "coordinate": []int{1, 1}, "scroll_direction": "down"},
		{"action": "drag", "start_coordinate": []int{1, 1}, "coordinate": []int{2, 2}},
	} {
		if r := act(input); !r.IsError || !strings.HasPrefix(textOf(r), ComputerErrorWindowFrameRequired) {
			t.Fatalf("%v = %+v", input, r)
		}
	}
	if got := computer.seen(); got != "" {
		t.Fatalf("calls = %s, want none", got)
	}
	// Keys and text have no coordinates; a targeted click uses the window's frame.
	if r := act(map[string]any{"action": "key", "text": "Escape"}); r.IsError {
		t.Fatalf("key = %+v", r)
	}
	if r := act(map[string]any{"action": "click", "coordinate": []int{5, 5}, "pid": 7, "window_id": 9}); r.IsError {
		t.Fatalf("targeted click = %+v", r)
	}
	last := computer.actions[len(computer.actions)-1]
	if last.Frame == nil || *last.Frame != (ComputerFrame{PID: 7, WindowID: 9, Width: 1, Height: 1}) {
		t.Fatalf("frame = %+v", last.Frame)
	}
	// A window never observed has no frame; the screen again is the frame of
	// untargeted actions.
	act(map[string]any{"action": "click", "coordinate": []int{5, 5}, "pid": 7, "window_id": 10})
	if f := computer.actions[len(computer.actions)-1].Frame; f != nil {
		t.Fatalf("frame of an unobserved window = %+v", f)
	}
	callComputer(t, session, ToolNameComputerObserve, map[string]any{})
	if r := act(map[string]any{"action": "click", "coordinate": []int{5, 5}}); r.IsError {
		t.Fatalf("click after a screen observation = %+v", r)
	}
	if f := computer.actions[len(computer.actions)-1].Frame; f == nil || f.WindowID != 0 || f.Width != 1 {
		t.Fatalf("screen frame = %+v", f)
	}
}

// TestComputerMenuAction: menu needs a window and 1-16 labels and runs
// invoke_menu on that window.
func TestComputerMenuAction(t *testing.T) {
	driver, logPath, err := fakeCua(t)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := computerSession(t, CoreOptions{ComputerUse: driver})
	result := callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "menu", "menu_path": []string{" Window ", "Weixin"}, "pid": 1208, "window_id": 983170})
	if result.IsError || !strings.Contains(textOf(result), "did computer_action menu") {
		t.Fatalf("menu = %+v", result)
	}
	raw, _ := os.ReadFile(logPath)
	if !strings.Contains(string(raw), `invoke_menu {"path":["Window","Weixin"],"pid":1208,"session":"bma-`) || !strings.Contains(string(raw), `"window_id":983170}`) {
		t.Fatalf("driver log:\n%s", raw)
	}
	for name, input := range map[string]map[string]any{
		"no window":   {"action": "menu", "menu_path": []string{"File"}},
		"no path":     {"action": "menu", "pid": 1, "window_id": 2},
		"blank label": {"action": "menu", "menu_path": []string{"File", "  "}, "pid": 1, "window_id": 2},
		"long label":  {"action": "menu", "menu_path": []string{strings.Repeat("x", 201)}, "pid": 1, "window_id": 2},
		"too deep":    {"action": "menu", "menu_path": strings.Split(strings.Repeat("a,", 16)+"a", ","), "pid": 1, "window_id": 2},
		"coordinate":  {"action": "menu", "menu_path": []string{"File"}, "pid": 1, "window_id": 2, "coordinate": []int{1, 1}},
	} {
		if r := callComputer(t, session, ToolNameComputerAction, input); !r.IsError {
			t.Errorf("%s = %+v", name, r)
		}
	}
}

// TestCuaAdviceSanitized: driver advice that names tools or inputs the model
// cannot call is dropped; tools it can are rewritten to its actions.
func TestCuaAdviceSanitized(t *testing.T) {
	for in, want := range map[string]string{
		"Three single actions in a row: send the next ones as one run_steps call.": "",
		`📨 Sent (unverified) 10 char(s) via CGEvent (30ms delay). — driver could not confirm the text landed; verify via screenshot, and re-call with delivery_mode:"foreground" if it didn't.`: "📨 Sent (unverified) 10 char(s) via CGEvent (30ms delay). driver could not confirm the text landed;",
		"Run an item with invoke_menu path, or observe with `get_window_state`.":                 "Run an item with computer_action menu path, or observe with computer_observe with pid and window_id.",
		"menus collapsed: run an item with invoke_menu path, or pass query to search menu items": "menus collapsed: run an item with computer_action menu path, or pass query to search menu items",
	} {
		if got := sanitizeCuaAdvice(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCuaDriverFailureAdvice: a structured driver failure reads in terms of
// the computer use tools; a refusal with effect "refused" delivered no input.
func TestCuaDriverFailureAdvice(t *testing.T) {
	background := `{"code":"background_unavailable","escalation":{"reason":"the point is on a background web browser page; the browser tools act on it","recommended":"page"},"hint":"Target the window instead: target {kind:\"window\", pid, window_id}","next_call":{"tool":"get_browser_state","arguments":{"pid":660,"window_id":776}}}`
	offSpace := `{"code":"off_space_or_ax_unresolved","effect":"refused","escalation":{"recommended":"foreground"},"pid":38989,"window_id":133464}`
	driver, _, err := fakeCua(t, "BMA_TEST_CUA_FAIL_TOOL=click", "BMA_TEST_CUA_FAIL_STDOUT="+background)
	if err != nil {
		t.Fatal(err)
	}
	_, err = driver.Action(context.Background(), ComputerActionRequest{Action: "click", Coordinate: &[2]float64{500, 300}})
	var failure *CuaDriverError
	if !errors.As(err, &failure) || failure.Code != "background_unavailable" || errors.Is(err, ErrComputerActionRejected) ||
		!strings.HasPrefix(err.Error(), "background_unavailable: the point is over another app") {
		t.Fatalf("err = %v", err)
	}
	for _, hidden := range []string{"get_browser_state", "page", "target {", "escalation"} {
		if strings.Contains(err.Error(), hidden) {
			t.Fatalf("advice names %q: %v", hidden, err)
		}
	}
	driver, _, err = fakeCua(t, "BMA_TEST_CUA_FAIL_TOOL=press_key", "BMA_TEST_CUA_FAIL_STDOUT="+offSpace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = driver.Action(context.Background(), ComputerActionRequest{Action: "key", Text: "Escape", PID: 38989, WindowID: 133464})
	if !errors.Is(err, ErrComputerActionRejected) || !strings.Contains(err.Error(), "window 133464 is not on the current desktop") ||
		!strings.Contains(err.Error(), "computer_action open_app") || !strings.Contains(err.Error(), "computer_action menu") {
		t.Fatalf("err = %v", err)
	}
	// A successful result's hint and next call are rewritten too.
	driver, _, err = fakeCua(t, "BMA_TEST_CUA_RESULT_TOOL=click",
		`BMA_TEST_CUA_RESULT={"summary":"Clicked.","hint":"Send the next ones as one run_steps call.","next_call":{"tool":"invoke_menu","arguments":{"pid":5,"window_id":6}}}`)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := driver.Action(context.Background(), ComputerActionRequest{Action: "click", Coordinate: &[2]float64{1, 1}})
	if err != nil || blocks[0].Text != "Clicked.\nNext: computer_action menu pid=5 window_id=6." {
		t.Fatalf("result = %+v, %v", blocks, err)
	}
}

// TestOpenAppRecovers: an app whose only window is off the current desktop
// and that cannot be brought to the front is opened again, its window
// brought forward, and only windows on the current desktop are listed.
func TestOpenAppRecovers(t *testing.T) {
	driver, logPath, err := fakeCua(t, "BMA_TEST_CUA_HIDDEN=1")
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := driver.Action(context.Background(), ComputerActionRequest{Action: "open_app", App: "TextEdit"})
	if err != nil {
		t.Fatal(err)
	}
	text := blocks[0].Text
	for _, want := range []string{
		"opened TextEdit (com.apple.TextEdit) pid=4242, brought to the front\n",
		"recovered: opened the app again, brought window 78 forward\n",
		`window pid=4242 window_id=78 app=TextEdit title="Untitled" at (10,20) 600x400`,
		"not usable (minimized, hidden or on another Space): 1 window(s)\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("open_app text lacks %q:\n%s", want, text)
		}
	}
	raw, _ := os.ReadFile(logPath)
	var tools []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		tools = append(tools, line[:strings.LastIndex(line, " telemetry=")])
	}
	want := `launch_app {"name":"TextEdit"},bring_to_front {"pid":4242},launch_app {"name":"TextEdit"},bring_to_front {"pid":4242},` +
		`list_windows {"pid":4242},bring_to_front {"pid":4242,"window_id":78},list_windows {"pid":4242}`
	if got := strings.Join(tools[1:], ","); got != want { // tools[0] is NewCuaDriver's get_screen_size
		t.Fatalf("driver calls:\n%s\nwant\n%s", got, want)
	}
	// The structured result, for a host that checks what was opened.
	driver2, _, err := fakeCua(t, "BMA_TEST_CUA_HIDDEN=1", "BMA_TEST_CUA_LOG="+filepath.Join(t.TempDir(), "other.log"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := driver2.OpenApp(context.Background(), ComputerActionRequest{App: "com.apple.TextEdit"})
	if err != nil || result.PID != 4242 || !result.Front || len(result.Windows) != 1 || result.Windows[0].WindowID != 78 || result.Hidden != 1 {
		t.Fatalf("OpenApp = %+v, %v", result, err)
	}
	// Without recovery (a visible window, front ok) nothing more is called.
	plain, logPlain, err := fakeCua(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.OpenApp(context.Background(), ComputerActionRequest{App: "TextEdit"}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(logPlain); strings.Count(string(raw), "launch_app") != 1 || strings.Contains(string(raw), "list_windows") {
		t.Fatalf("driver calls without recovery:\n%s", raw)
	}
}

// TestTypeReadBack: type reports verified, mismatch or unverified from the
// driver's own confirmation or a read-back of the window's fields.
func TestTypeReadBack(t *testing.T) {
	window := ComputerActionRequest{Action: "type", Text: "hello", PID: 1208, WindowID: 983170}
	for name, c := range map[string]struct {
		env      []string
		req      ComputerActionRequest
		want     string
		wantErr  bool
		readback bool
	}{
		"driver confirmed": {env: []string{"BMA_TEST_CUA_RESULT_TOOL=type_text", `BMA_TEST_CUA_RESULT={"summary":"Inserted.","effect":"confirmed"}`},
			req: window, want: "typed: verified"},
		"native field": {env: []string{`BMA_TEST_CUA_ELEMENTS=[{"role":"AXTextArea","value":"say hello"}]`}, req: window, want: "typed: verified", readback: true},
		"web field": {env: []string{`BMA_TEST_CUA_ELEMENTS=[{"role":"AXTextArea","value":"hello","in_web_content":true}]`}, req: window,
			want: "typed: unverified — the window's accessibility tree shows the text", readback: true},
		"part landed": {env: []string{`BMA_TEST_CUA_ELEMENTS=[{"role":"AXTextField","value":"hel"},{"role":"AXButton","value":"x"}]`}, req: window,
			want: `typed: mismatch — the field shows "hel"`, readback: true},
		"no field": {env: []string{`BMA_TEST_CUA_ELEMENTS=[{"role":"AXButton","value":"Send"}]`}, req: window,
			want: "typed: mismatch — no field of this window shows the text", readback: true},
		"unreadable": {env: []string{`BMA_TEST_CUA_ELEMENTS=[]`}, req: window, want: "typed: unverified — the field could not be read back", readback: true},
		"desktop":    {req: ComputerActionRequest{Action: "type", Text: "hello"}, want: "typed: unverified — typed into the front app"},
		"incomplete, but landed": {env: []string{"BMA_TEST_CUA_FAIL_TOOL=type_text", `BMA_TEST_CUA_FAIL_STDOUT={"code":"type_text_incomplete","effect":"partial"}`,
			`BMA_TEST_CUA_ELEMENTS=[{"role":"AXTextArea","value":"hello"}]`}, req: window, want: "typed: verified", readback: true},
		"incomplete": {env: []string{"BMA_TEST_CUA_FAIL_TOOL=type_text", `BMA_TEST_CUA_FAIL_STDOUT={"code":"type_text_incomplete","effect":"partial"}`,
			`BMA_TEST_CUA_ELEMENTS=[{"role":"AXTextArea","value":"he"}]`}, req: window, wantErr: true,
			want: "type_text_incomplete: the driver could not confirm that all of the text landed", readback: true},
	} {
		t.Run(name, func(t *testing.T) {
			driver, logPath, err := fakeCua(t, c.env...)
			if err != nil {
				t.Fatal(err)
			}
			blocks, err := driver.Action(context.Background(), c.req)
			var text string
			if err != nil {
				text = err.Error()
			} else {
				text = blocks[0].Text
			}
			if (err != nil) != c.wantErr || !strings.Contains(text, c.want) {
				t.Fatalf("result = %q (err %v), want %q", text, err, c.want)
			}
			if c.wantErr && !strings.Contains(text, "\ntyped: mismatch") {
				t.Fatalf("an incomplete delivery lacks its verdict: %q", text)
			}
			raw, _ := os.ReadFile(logPath)
			readback := strings.Contains(string(raw), `"tree_format":"elements"`)
			if readback != c.readback {
				t.Fatalf("read back = %v, want %v:\n%s", readback, c.readback, raw)
			}
			if readback && !strings.Contains(string(raw), `"include_screenshot":false`) {
				t.Fatalf("the read-back took a screenshot:\n%s", raw)
			}
		})
	}
}

// TestTypeRoute: type picks its route by the field. A window without web
// content and ASCII text go the driver's default way (accessibility
// insertion); non-ASCII text into web content is pasted with the clipboard
// put back, or typed in the foreground when the clipboard holds more than
// plain text or the host keeps the clipboard out; an unreadable window counts
// as web content.
func TestTypeRoute(t *testing.T) {
	text := "老婆仔，今天下雨☔️"
	req := ComputerActionRequest{Action: "type", Text: text, PID: 38989, WindowID: 133461}
	modifier := "cmd"
	if runtime.GOOS != "darwin" {
		modifier = "ctrl"
	}
	native := `BMA_TEST_CUA_ELEMENTS=[{"role":"AXTextArea","value":""}]`
	web := `BMA_TEST_CUA_ELEMENTS=[{"role":"AXWebArea","in_web_content":true},{"role":"AXTextArea","value":"","in_web_content":true}]`
	paste := []string{`clipboard_read {"include_text":true}`, `clipboard_write {"text":"` + text + `"}`,
		`hotkey {"keys":["` + modifier + `","v"],"pid":38989,"session":`, `clipboard_write {"text":"CLIPBOARD-PREV"}`}
	for name, c := range map[string]struct {
		noClipboard bool
		env         []string
		req         ComputerActionRequest
		want        []string
		without     []string
		reads       int
	}{
		"native field, non-ASCII": {env: []string{native}, req: req, want: []string{`type_text {"pid":38989`}, without: []string{"clipboard", "delivery_mode"}, reads: 2},
		"web field, non-ASCII":    {env: []string{web}, req: req, want: paste, without: []string{"type_text"}, reads: 2},
		"web field, ASCII": {env: []string{web}, req: ComputerActionRequest{Action: "type", Text: "rain", PID: 1, WindowID: 2},
			want: []string{`type_text {"pid":1`}, without: []string{"clipboard", "delivery_mode"}, reads: 1},
		"rich clipboard": {env: []string{web, `BMA_TEST_CUA_CLIPBOARD_TYPES=["public.html","public.utf8-plain-text"]`}, req: req,
			want: []string{`clipboard_read`, `type_text {"delivery_mode":"foreground"`}, without: []string{"clipboard_write", "hotkey"}, reads: 2},
		"unreadable window": {req: req, want: paste, without: []string{"type_text"}, reads: 2},
		"no clipboard": {noClipboard: true, env: []string{web}, req: req,
			want: []string{`type_text {"delivery_mode":"foreground"`}, without: []string{"clipboard"}, reads: 2},
		"no window": {req: ComputerActionRequest{Action: "type", Text: text}, want: []string{`type_text {"scope":"desktop"`},
			without: []string{"clipboard", "delivery_mode"}},
	} {
		t.Run(name, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(t.TempDir(), "cua.log")
			driver, err := NewCuaDriver(context.Background(), CuaDriverOptions{Path: exe, NoClipboard: c.noClipboard,
				Env: append([]string{"BMA_TEST_HELPER=cua", "BMA_TEST_CUA_LOG=" + logPath}, c.env...)})
			if err != nil {
				t.Fatal(err)
			}
			blocks, err := driver.Action(context.Background(), c.req)
			if err != nil || !strings.Contains(blocks[0].Text, "\ntyped: ") {
				t.Fatalf("type = %+v, %v", blocks, err)
			}
			// The fake driver's summary echoes its input; the paste summary is the SDK's.
			if strings.Contains(blocks[0].Text, "CLIPBOARD-PREV") || (strings.Contains(blocks[0].Text, "pasted") && strings.Contains(blocks[0].Text, text)) {
				t.Fatalf("the result repeats the text or the clipboard: %q", blocks[0].Text)
			}
			raw, _ := os.ReadFile(logPath)
			log := string(raw)
			last := 0
			for _, w := range c.want {
				i := strings.Index(log[last:], w)
				if i < 0 {
					t.Fatalf("driver log lacks %q (in order):\n%s", w, log)
				}
				last += i + len(w)
			}
			for _, w := range c.without {
				if strings.Contains(log, w) {
					t.Fatalf("driver log has %q:\n%s", w, log)
				}
			}
			if got := strings.Count(log, `"tree_format":"elements"`); got != c.reads {
				t.Fatalf("%d accessibility reads, want %d:\n%s", got, c.reads, log)
			}
		})
	}
}

// TestComputerVision: a session whose model reads text only gets no
// screenshot, with a note; unknown and image models get it.
func TestComputerVision(t *testing.T) {
	var asked []ComputerVisionQuery
	vision := ComputerVisionUnknown
	computer := &recordingComputer{}
	session, _ := computerSession(t, CoreOptions{ComputerUse: computer, ComputerVision: func(_ context.Context, q ComputerVisionQuery) ComputerVisionAnswer {
		asked = append(asked, q)
		return ComputerVisionAnswer{Model: "deepseek-flash", Vision: vision}
	}})
	for _, v := range []ComputerVision{ComputerVisionUnknown, ComputerVisionImages} {
		vision = v
		if r := callComputer(t, session, ToolNameComputerObserve, map[string]any{"pid": 3, "window_id": 4}); len(r.Content) != 2 || r.Content[0].Type != ContentBlockTypeImage {
			t.Fatalf("%q: observe = %+v", v, r)
		}
	}
	vision = ComputerVisionTextOnly
	r := callComputer(t, session, ToolNameComputerObserve, map[string]any{"pid": 3, "window_id": 4})
	if r.IsError || len(r.Content) != 2 || r.Content[0].Type != ContentBlockTypeText || r.Content[1].Text != "screen screenshot 1x1" ||
		!strings.HasPrefix(r.Content[0].Text, "The model deepseek-flash cannot read images, so the screenshot was not sent.") {
		t.Fatalf("text-only observe = %+v", r)
	}
	if len(asked) != 3 || asked[0].SessionID != "sess_cu" || asked[0].Model != "" {
		t.Fatalf("asked = %+v", asked)
	}
	// The frame is the screenshot's, sent or not.
	callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "click", "coordinate": []int{1, 1}, "pid": 3, "window_id": 4})
	if f := computer.actions[0].Frame; f == nil || f.Width != 1 || f.Height != 1 {
		t.Fatalf("frame = %+v", f)
	}
}
