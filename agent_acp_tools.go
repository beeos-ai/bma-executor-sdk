package bmaexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Tools of an ACP agent run in Core, in a session Core holds for the local
// brain ("acp") whose workspace is the turn's working directory. Inputs come
// in the CMA agent_toolset shape that bma-dsh-acp and brain-host share
// (file_path, timeout in ms, replace_all, output_mode, case_insensitive) and
// are mapped to Core's ToolInput here.

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

func (a *ACPAgent) runTool(ctx context.Context, sessionID, callID, name string, in map[string]any) (string, bool) {
	if !coreTools[name] {
		return fmt.Sprintf("tool %q is not available on this computer", name), true
	}
	str := func(k string) string { v, _ := in[k].(string); return v }
	num := func(k string) int64 { v, _ := in[k].(float64); return int64(v) }
	flag := func(k string) bool { v, _ := in[k].(bool); return v }
	input := ToolInput{Path: str("file_path"), Offset: num("offset"), Limit: num("limit"), Content: str("content"),
		OldString: str("old_string"), NewString: str("new_string"), Pattern: str("pattern"), Glob: str("glob")}
	switch name {
	case ToolNameBash:
		input.Command, input.TimeoutMS = str("command"), num("timeout")
	case ToolNameGlob, ToolNameGrep:
		input.Path = str("path")
		if name == ToolNameGrep && flag("case_insensitive") {
			input.Pattern = "(?i)" + input.Pattern
		}
	case ToolNameEdit:
		if flag("replace_all") {
			return a.replaceAll(sessionID, input)
		}
	}
	raw, _ := json.Marshal(input)
	result := a.opts.Core.toolCall(ctx, acpBrain, ToolCallParams{SessionID: sessionID, ToolCallID: callID, Tool: name, Input: raw})
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

// replaceAll is edit with replace_all (Core's edit replaces exactly one match).
func (a *ACPAgent) replaceAll(sessionID string, input ToolInput) (string, bool) {
	session := a.opts.Core.session(coreKey{acpBrain, sessionID})
	if session == nil {
		return "session_not_active: the session is not open", true
	}
	path, err := session.resolve(input.Path)
	if err != nil {
		return err.Error(), true
	}
	if input.OldString == "" {
		return "edit requires old_string and new_string", true
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err.Error(), true
	}
	n := strings.Count(string(raw), input.OldString)
	if n == 0 {
		return "old_string was not found", true
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), input.OldString, input.NewString)), 0o600); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("edited %s (%d replacements)", filepath.Base(path), n), false
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
