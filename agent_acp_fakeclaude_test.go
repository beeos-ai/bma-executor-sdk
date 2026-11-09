package bmaexecutor

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// fakeClaudeAgent speaks enough of claude-agent-acp for the gates. mode:
// ok (set_mode works), stuck (the mode stays bypassPermissions), unpinned
// (an unknown release), catalog (the init message lists a built-in tool).
func fakeClaudeAgent(mode string) {
	out := bufio.NewWriter(os.Stdout)
	var wmu sync.Mutex
	send := func(v any) {
		wmu.Lock()
		defer wmu.Unlock()
		raw, _ := json.Marshal(v)
		_, _ = out.Write(append(raw, '\n'))
		_ = out.Flush()
	}
	update := func(update map[string]any) {
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "claude-session-1", "update": update}})
	}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<24)
	for in.Scan() {
		var m struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		if m.ID == nil {
			if m.Method == "session/cancel" {
				cancelOnce.Do(func() { close(canceled) })
			}
			continue
		}
		id := *m.ID
		reply := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
		switch m.Method {
		case "initialize":
			version := "0.101.0"
			if mode == "unpinned" {
				version = "9.9.9"
			}
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true},
				"agentInfo": map[string]any{"name": claudeAgentACPName, "version": version}})
		case "session/new":
			reply(map[string]any{"sessionId": "claude-session-1", "modes": map[string]any{"currentModeId": "auto"}})
		case "session/set_mode":
			current := "default"
			if mode == "stuck" {
				current = "bypassPermissions"
			}
			update(map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": current})
			reply(map[string]any{})
		case "session/prompt":
			go func() {
				if mode == "catalog" {
					send(map[string]any{"jsonrpc": "2.0", "method": "_claude/sdkMessage", "params": map[string]any{"sessionId": "claude-session-1",
						"message": map[string]any{"type": "system", "subtype": "init", "tools": []string{"mcp__bma__bash", "Bash"}, "permissionMode": "default"}}})
				}
				select {
				case <-canceled:
					reply(map[string]any{"stopReason": "cancelled"})
				case <-time.After(10 * time.Second):
					reply(map[string]any{"stopReason": "end_turn"})
				}
			}()
		default:
			reply(map[string]any{})
		}
	}
}
