package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fakeCua(t *testing.T, extraEnv ...string) (*CuaDriver, string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "cua.log")
	driver, err := NewCuaDriver(context.Background(), CuaDriverOptions{
		Path: exe, Env: append([]string{"BMA_TEST_HELPER=cua", "BMA_TEST_CUA_LOG=" + log}, extraEnv...),
	})
	return driver, log, err
}

func computerSession(t *testing.T, opts CoreOptions) (*Session, *Core) {
	t.Helper()
	core := newTestCore(t, opts)
	return readySession(t, attachBrain(t, core, "local"), SessionAssignParams{SessionID: "sess_cu", Lease: Lease{Token: "lease"}}), core
}

func callComputer(t *testing.T, session *Session, tool string, input map[string]any) ToolResultParams {
	t.Helper()
	result, err := session.CallTool(testContext(t), ToolCallParams{Tool: tool, Input: mustJSON(t, input)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestComputerUseWithCuaDriver(t *testing.T) {
	driver, logPath, err := fakeCua(t)
	if err != nil {
		t.Fatal(err)
	}
	session, core := computerSession(t, CoreOptions{ComputerUse: driver})
	if !core.Capabilities().ComputerUse {
		t.Fatal("computer_use is not advertised")
	}

	observe := callComputer(t, session, ToolNameComputerObserve, map[string]any{})
	if observe.IsError || len(observe.Content) != 2 || observe.Content[0].Type != ContentBlockTypeImage ||
		observe.Content[0].MimeType != "image/png" || observe.Content[0].Data == "" {
		t.Fatalf("observe = %+v", observe)
	}
	if text := observe.Content[1].Text; !strings.Contains(text, "1024x768") ||
		!strings.Contains(text, "window pid=1208 window_id=983170") || strings.Contains(text, "hidden.exe") {
		t.Fatalf("observe text = %q", text)
	}
	window := callComputer(t, session, ToolNameComputerObserve, map[string]any{"pid": 1208, "window_id": 983170})
	if window.IsError || !strings.Contains(window.Content[1].Text, `[0] Edit "Text Editor"`) {
		t.Fatalf("window observe = %+v", window)
	}

	for _, input := range []map[string]any{
		{"action": "click", "coordinate": []int{10, 20}, "button": "right", "click_count": 2},
		{"action": "type", "text": "hello"},
		{"action": "key", "text": "Ctrl+S"},
		{"action": "key", "text": "Return"},
		{"action": "scroll", "coordinate": []int{5, 6}, "scroll_direction": "down", "scroll_amount": 4},
		{"action": "drag", "start_coordinate": []int{1, 2}, "coordinate": []int{3, 4}},
		{"action": "click", "coordinate": []int{7, 8}, "pid": 1208, "window_id": 983170},
	} {
		if result := callComputer(t, session, ToolNameComputerAction, input); result.IsError || !strings.Contains(textOf(result), "did ") {
			t.Fatalf("action %v = %+v", input, result)
		}
	}
	raw, _ := os.ReadFile(logPath)
	log := string(raw)
	for _, want := range []string{
		`click {"button":"right","count":2,"scope":"desktop","x":10,"y":20}`,
		`type_text {"scope":"desktop","text":"hello"}`,
		`hotkey {"keys":["ctrl","s"],"scope":"desktop"}`,
		`press_key {"key":"return","scope":"desktop"}`,
		`scroll {"amount":4,"direction":"down","scope":"desktop","x":5,"y":6}`,
		`drag {"from_x":1,"from_y":2,"scope":"desktop","to_x":3,"to_y":4}`,
		`"window_id":983170,"x":7,"y":8}`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("driver log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "telemetry=1") || !strings.Contains(log, "telemetry=0") {
		t.Errorf("driver telemetry is not disabled:\n%s", log)
	}
}

func TestComputerUseRejectsBadInput(t *testing.T) {
	driver, _, err := fakeCua(t)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := computerSession(t, CoreOptions{ComputerUse: driver})
	for name, input := range map[string]map[string]any{
		"click without coordinate": {"action": "click"},
		"unknown action":           {"action": "hover"},
		"type without text":        {"action": "type"},
		"bad scroll direction":     {"action": "scroll", "coordinate": []int{1, 1}, "scroll_direction": "sideways"},
		"pid without window":       {"action": "click", "coordinate": []int{1, 1}, "pid": 5},
		"second display":           {"action": "type", "text": "x", "display": "HDMI-2"},
		"open_app without app":     {"action": "open_app", "app": "  "},
		"open_app with a window":   {"action": "open_app", "app": "TextEdit", "pid": 5, "window_id": 6},
	} {
		if result := callComputer(t, session, ToolNameComputerAction, input); !result.IsError || result.Outcome != ToolOutcomeFailed {
			t.Errorf("%s = %+v", name, result)
		}
	}
}

// TestComputerOpenApp: open_app launches by bundle id or name, brings the app
// to the front and returns its pid and windows; description is accepted and
// not needed.
func TestComputerOpenApp(t *testing.T) {
	driver, logPath, err := fakeCua(t)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := computerSession(t, CoreOptions{ComputerUse: driver})
	result := callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "open_app", "app": "com.apple.TextEdit", "description": "打开文本编辑"})
	if result.IsError || !strings.Contains(textOf(result), "pid=4242, brought to the front") ||
		!strings.Contains(textOf(result), "window pid=4242 window_id=77 app=TextEdit") {
		t.Fatalf("open_app = %+v", result)
	}
	if result := callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "open_app", "app": "Text Edit"}); result.IsError {
		t.Fatalf("open_app by name = %+v", result)
	}
	raw, _ := os.ReadFile(logPath)
	for _, want := range []string{
		`launch_app {"bundle_id":"com.apple.TextEdit"}`,
		`bring_to_front {"pid":4242}`,
		`launch_app {"name":"Text Edit"}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("driver log lacks %q:\n%s", want, raw)
		}
	}
}

func TestLooksLikeBundleID(t *testing.T) {
	for app, want := range map[string]bool{
		"com.apple.TextEdit": true, "com.larksuite.larkApp": true,
		"TextEdit": false, "Lark.app": false, "/Applications/Lark.app": false, "Visual Studio Code": false, "飞书": false,
	} {
		if got := looksLikeBundleID(app); got != want {
			t.Errorf("looksLikeBundleID(%q) = %v", app, got)
		}
	}
}

// TestComputerToolSchemas: both tools ask for a description of the step, and
// computer_action offers open_app with app.
func TestComputerToolSchemas(t *testing.T) {
	for _, tool := range []acpHostTool{acpComputerObserve, acpComputerAction} {
		required, _ := tool.Parameters["required"].([]string)
		props, _ := tool.Parameters["properties"].(map[string]any)
		if !slices.Contains(required, "description") || props["description"] == nil {
			t.Errorf("%s: description is not a required property: %v", tool.Name, tool.Parameters)
		}
	}
	props := acpComputerAction.Parameters["properties"].(map[string]any)
	if enum := props["action"].(map[string]any)["enum"].([]string); !slices.Contains(enum, ComputerActionOpenApp) || props["app"] == nil {
		t.Errorf("computer_action lacks open_app / app: %v", props)
	}
}

// TestComputerUseDegrades: without a working driver there is no capability, and
// a computer_* tool is refused as capability_unavailable.
func TestComputerUseDegrades(t *testing.T) {
	if _, err := NewCuaDriver(context.Background(), CuaDriverOptions{Path: filepath.Join(t.TempDir(), "no-such-driver")}); !errors.Is(err, ErrComputerUseUnavailable) {
		t.Fatalf("missing driver error = %v", err)
	}
	if _, _, err := fakeCua(t, "BMA_TEST_CUA_FAIL=1"); !errors.Is(err, ErrComputerUseUnavailable) || !strings.Contains(err.Error(), "daemon is not running") {
		t.Fatalf("driver without daemon error = %v", err)
	}
	session, core := computerSession(t, CoreOptions{})
	if core.Capabilities().ComputerUse {
		t.Fatal("computer_use advertised without a backend")
	}
	_, err := session.CallTool(testContext(t), ToolCallParams{Tool: ToolNameComputerObserve, Input: mustJSON(t, map[string]any{})})
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeCapabilityUnavailable {
		t.Fatalf("observe without a backend = %v", err)
	}
}

// TestComputerUseDriverFailureIsAToolError: a driver that fails mid-session is a
// failed tool result, not a broken executor.
func TestComputerUseDriverFailureIsAToolError(t *testing.T) {
	driver, _, err := fakeCua(t)
	if err != nil {
		t.Fatal(err)
	}
	driver.env = append(driver.env, "BMA_TEST_CUA_FAIL=1")
	session, _ := computerSession(t, CoreOptions{ComputerUse: driver})
	result := callComputer(t, session, ToolNameComputerObserve, map[string]any{})
	if !result.IsError || !strings.Contains(textOf(result), "daemon is not running") {
		t.Fatalf("result = %+v", result)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A real macOS 0.34.0 observation failed before reaching the screenshot because
// tree_format is not in that version's input schema. The default response still
// contains tree_markdown; verify both it and the image survive the compatibility fix.
func TestComputerObserveWindowLegacySchema(t *testing.T) {
	driver, _, err := fakeCua(t, "BMA_TEST_CUA_STRICT_WINDOW=1")
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"", "jpeg"} {
		content, err := driver.Observe(context.Background(), ComputerObserveRequest{PID: 1208, WindowID: 983170, ImageFormat: format})
		if err != nil {
			t.Fatal(err)
		}
		if len(content) != 2 || content[0].Data == "" || !strings.Contains(content[1].Text, `[0] Edit "Text Editor"`) {
			t.Fatalf("window result lost image or tree: %+v", content)
		}
	}
}

func TestComputerWindowContextIsStableAndIsolated(t *testing.T) {
	driver, logPath, err := fakeCua(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"first", "second"} {
		_, err = driver.Observe(context.Background(), ComputerObserveRequest{SessionID: session, PID: 1208, WindowID: 983170})
		if err != nil {
			t.Fatal(err)
		}
		_, err = driver.Action(context.Background(), ComputerActionRequest{SessionID: session, PID: 1208, WindowID: 983170, Action: "click", Coordinate: &[2]float64{7, 8}})
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(logPath)
	var labels []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "get_window_state ") && !strings.HasPrefix(line, "click ") {
			continue
		}
		payload := line[strings.Index(line, " ")+1 : strings.LastIndex(line, " telemetry=")]
		var args map[string]any
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			t.Fatal(err)
		}
		label, _ := args["session"].(string)
		if label == "" || label == "default" {
			t.Fatal("disposable driver context")
		}
		labels = append(labels, label)
	}
	if len(labels) != 4 || labels[0] != labels[1] || labels[2] != labels[3] || labels[0] == labels[2] {
		t.Fatalf("window contexts: %v", labels)
	}
}
