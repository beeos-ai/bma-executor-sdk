package bmaexecutor

// Typed tool handlers of the executor end (ADR 0012 §4.1): one slot per
// built-in tool with that tool's input type from the contract.
//
//	client.Tools.Bash.Handle(func(ctx context.Context, in bmaexecutor.BashInput, tc bmaexecutor.ToolContext) (bmaexecutor.ToolOutput, error) {
//		out, err := run(ctx, in.Command)
//		return bmaexecutor.BashResult{Stdout: out, ExitCode: code}.Output(), err
//	})
//
// A handler returns a ToolOutput (TextOutput, BashResult.Output, or content
// blocks). Return *ToolError (NewToolError) for a tool-level error reported
// to the model (outcome succeeded, is_error true); any other error is
// outcome failed. tool.cancel, the deadline and session.stop cancel ctx.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ToolContext describes the call a handler serves.
type ToolContext struct {
	SessionID  string
	ToolCallID string
	Permission *ToolPermission
	// Progress sends tool.progress for this call (best-effort).
	Progress func(message string)

	params ToolCallParams
}

// ToolOutput is a handler's result; the client fills in the ids and duration.
type ToolOutput struct {
	Content   []ContentBlock
	IsError   bool
	Artifacts []ArtifactRef
	// Outcome is empty for succeeded; ToolOutcomeCanceled reports a call the
	// handler abandoned because ctx was canceled.
	Outcome string
}

// TextOutput is a ToolOutput of one text block.
func TextOutput(text string) ToolOutput {
	return ToolOutput{Content: []ContentBlock{{Type: ContentBlockTypeText, Text: text}}}
}

// BashResult is a finished command, rendered as Core renders it.
type BashResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Output renders stdout and stderr (when not empty) and `exit_code: N`.
func (r BashResult) Output() ToolOutput {
	var out ToolOutput
	if r.Stdout != "" {
		out.Content = append(out.Content, ContentBlock{Type: ContentBlockTypeText, Text: r.Stdout})
	}
	if r.Stderr != "" {
		out.Content = append(out.Content, ContentBlock{Type: ContentBlockTypeText, Text: r.Stderr})
	}
	out.Content = append(out.Content, ContentBlock{Type: ContentBlockTypeText, Text: fmt.Sprintf("exit_code: %d", r.ExitCode)})
	return out
}

// ToolError is a tool-level error reported to the model (outcome
// succeeded, is_error true), as Core reports a missing file. Same role as
// the Anthropic SDK ToolError.
type ToolError struct {
	Content []ContentBlock
}

// NewToolError returns a ToolError with one text block.
func NewToolError(text string) *ToolError {
	return &ToolError{Content: []ContentBlock{{Type: ContentBlockTypeText, Text: text}}}
}

func (e *ToolError) Error() string {
	for _, block := range e.Content {
		if block.Text != "" {
			return block.Text
		}
	}
	return "tool error"
}

type toolHandler func(ctx context.Context, params ToolCallParams, progress func(string)) (ToolResultParams, error)

// ToolSlot serves tool.call for one built-in tool.
type ToolSlot[In any] struct {
	name   string
	client *Client
}

// Handle registers the handler of this tool, replacing an earlier one.
// Register before Start: capabilities are derived from the handlers.
func (s ToolSlot[In]) Handle(handler func(ctx context.Context, input In, tool ToolContext) (ToolOutput, error)) {
	s.client.setToolHandler(s.name, func(ctx context.Context, params ToolCallParams, progress func(string)) (ToolResultParams, error) {
		var input In
		if len(params.Input) > 0 {
			if err := json.Unmarshal(params.Input, &input); err != nil {
				return ToolResultParams{}, fmt.Errorf("invalid %s input: %w", s.name, err)
			}
		}
		started := time.Now()
		out, err := handler(ctx, input, ToolContext{
			SessionID: params.SessionID, ToolCallID: params.ToolCallID, Permission: params.Permission,
			Progress: progress, params: params,
		})
		var toolErr *ToolError
		switch {
		case errors.As(err, &toolErr):
			out = ToolOutput{Content: toolErr.Content, IsError: true}
		case err != nil:
			return ToolResultParams{}, err
		}
		outcome := out.Outcome
		if outcome == "" {
			outcome = ToolOutcomeSucceeded
		}
		return ToolResultParams{
			Outcome: outcome, IsError: out.IsError, Content: out.Content, Artifacts: out.Artifacts,
			DurationMS: time.Since(started).Milliseconds(),
		}, nil
	})
}

// Tools has one typed slot per built-in tool: client.Tools.Bash.Handle(...).
type Tools struct {
	Bash            ToolSlot[BashInput]
	Read            ToolSlot[ReadInput]
	Write           ToolSlot[WriteInput]
	Edit            ToolSlot[EditInput]
	Glob            ToolSlot[GlobInput]
	Grep            ToolSlot[GrepInput]
	ListFiles       ToolSlot[ListFilesInput]
	ComputerObserve ToolSlot[ComputerObserveInput]
	ComputerAction  ToolSlot[ComputerActionInput]
}

func newTools(c *Client) Tools {
	return Tools{
		Bash: ToolSlot[BashInput]{ToolNameBash, c}, Read: ToolSlot[ReadInput]{ToolNameRead, c},
		Write: ToolSlot[WriteInput]{ToolNameWrite, c}, Edit: ToolSlot[EditInput]{ToolNameEdit, c},
		Glob: ToolSlot[GlobInput]{ToolNameGlob, c}, Grep: ToolSlot[GrepInput]{ToolNameGrep, c},
		ListFiles:       ToolSlot[ListFilesInput]{ToolNameListFiles, c},
		ComputerObserve: ToolSlot[ComputerObserveInput]{ToolNameComputerObserve, c},
		ComputerAction:  ToolSlot[ComputerActionInput]{ToolNameComputerAction, c},
	}
}

// CallToolOptions are the optional fields of a typed harness tool call.
type CallToolOptions struct {
	// ToolCallID is the idempotency id of the call; generated when empty.
	ToolCallID string
	DeadlineMS int64
	Permission *ToolPermission
}

// HarnessToolSlot calls one built-in tool from the harness end.
type HarnessToolSlot[In any] struct {
	name    string
	session *Session
}

// Call sends tool.call with a typed input and waits for tool.result, as
// Session.CallTool.
func (s HarnessToolSlot[In]) Call(ctx context.Context, input In, options CallToolOptions) (ToolResultParams, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return ToolResultParams{}, err
	}
	return s.session.CallTool(ctx, ToolCallParams{
		ToolCallID: options.ToolCallID, Tool: s.name, Input: raw, DeadlineMS: options.DeadlineMS, Permission: options.Permission,
	})
}

// HarnessTools has one typed entry per built-in tool: session.Tools.Bash.Call(...).
type HarnessTools struct {
	Bash            HarnessToolSlot[BashInput]
	Read            HarnessToolSlot[ReadInput]
	Write           HarnessToolSlot[WriteInput]
	Edit            HarnessToolSlot[EditInput]
	Glob            HarnessToolSlot[GlobInput]
	Grep            HarnessToolSlot[GrepInput]
	ListFiles       HarnessToolSlot[ListFilesInput]
	ComputerObserve HarnessToolSlot[ComputerObserveInput]
	ComputerAction  HarnessToolSlot[ComputerActionInput]
}

func newHarnessTools(s *Session) HarnessTools {
	return HarnessTools{
		Bash: HarnessToolSlot[BashInput]{ToolNameBash, s}, Read: HarnessToolSlot[ReadInput]{ToolNameRead, s},
		Write: HarnessToolSlot[WriteInput]{ToolNameWrite, s}, Edit: HarnessToolSlot[EditInput]{ToolNameEdit, s},
		Glob: HarnessToolSlot[GlobInput]{ToolNameGlob, s}, Grep: HarnessToolSlot[GrepInput]{ToolNameGrep, s},
		ListFiles:       HarnessToolSlot[ListFilesInput]{ToolNameListFiles, s},
		ComputerObserve: HarnessToolSlot[ComputerObserveInput]{ToolNameComputerObserve, s},
		ComputerAction:  HarnessToolSlot[ComputerActionInput]{ToolNameComputerAction, s},
	}
}
