package bmaexecutor

// The brain adapter interface (ADR-0066 of BMA).
//
// BrainHost (brain_host.go) is the executor's one implementation of the
// platform semantics of a local brain: turns and their single terminal state,
// deterministic approval ids and platform interactions, tool policy, the
// ToolRoute of each call, host tools and custom tools, CMA events, stop codes,
// user images, the resume ledger, resident sessions. A BrainAdapter only
// drives one kind of brain process: it starts and checks it, opens, resumes,
// suspends and closes its conversations, hands it one input at a time and
// reports what the brain does through a BrainSink. Everything that differs
// between brains is inside an adapter or stated by its BrainCapabilities; the
// host never asks which brain it hosts (a test of this package fails when its
// source names one).

import (
	"context"
	"time"
)

// BrainAdapter drives one kind of local brain.
type BrainAdapter interface {
	// Start starts and checks the brain (its process, pinned version and tool
	// catalog) and reports what it can do. It is called once, before any Open.
	// Options the brain cannot honor (BrainEnv.PlatformModel for a brain on its
	// owner's credentials, BrainEnv.Models for a brain without ModelChoice) are
	// refused before anything starts.
	Start(ctx context.Context, env BrainEnv) (BrainCapabilities, error)
	// Open makes the brain's conversation of a platform session ready for a
	// turn: a new conversation (req.Ref empty), the conversation req.Ref
	// names, or req.Current when that is still ready. A conversation to resume
	// that does not exist is an *AgentError (agent_error); it is never
	// created anew.
	Open(ctx context.Context, req BrainOpen) (BrainSession, error)
	// Ref is the executor_session_ref a conversation of sessionID has before
	// it is opened ("" when the brain assigns it at Open).
	Ref(sessionID string) string
	// Close stops the brain and everything it started.
	Close() error
}

// BrainSession is one open conversation of the brain.
type BrainSession interface {
	// Ref is the executor_session_ref: the id the conversation is resumed by.
	Ref() string
	// Ready reports whether the brain still holds the conversation open.
	Ready() bool
	// Prompt hands the brain one input and returns when the brain is done
	// with it. When ctx ends (turn.cancel, deadline) the adapter makes the
	// brain stop within BrainEnv.CancelGrace, killing it after that, and
	// returns ctx.Err(). Errors carry the contract's stop codes: an
	// *AgentError, or an error whose text starts with "<code>: ".
	Prompt(ctx context.Context, in BrainInput, sink BrainSink) error
	// Suspend releases what the conversation holds in the brain; the next
	// Open with its Ref loads it again.
	Suspend(ctx context.Context) error
	// Close ends the conversation in the brain.
	Close(ctx context.Context) error
}

// BrainSink is how an adapter reports the brain's work of one input and asks
// the host for decisions. The host implements it; an adapter writes no event
// itself. Calls are safe from any goroutine.
type BrainSink interface {
	// Delta streams text or thinking (kind BrainKindMessage or
	// BrainKindThinking) of the brain's message messageID.
	Delta(kind, messageID, text string)
	// Message commits the durable text of the brain's message messageID.
	Message(kind, messageID, text string)
	// ToolUse reports a tool call of the brain, once its input is known.
	// permission is what the brain's own policy evaluated ("allow", "ask"),
	// or "" when the brain applies no policy and the host's decides.
	ToolUse(call BrainCall, permission string)
	// ToolResult reports the end of a tool call the brain reported.
	ToolResult(callID string, content []ContentBlock, isError bool)
	// Permission decides whether the brain may run a call: the turn's tool
	// policy, the host's approver, an earlier decision of the turn, or the
	// platform's user through an interaction.
	Permission(ctx context.Context, call BrainCall) BrainDecision
	// Call runs one tool call through the turn's route (a platform tool, a
	// host tool or a custom tool) and returns its result.
	Call(ctx context.Context, call BrainCall) *RouteResult
}

// Kinds of brain text.
const (
	BrainKindMessage  = "message"
	BrainKindThinking = "thinking"
)

// BrainCall is one tool call of the brain, by the platform's tool name.
type BrainCall struct {
	// ID is the call's id in the brain's own transcript (ACP toolCallId,
	// Claude tool_use_id): the call_id of its approval id.
	ID    string
	Name  string
	Input map[string]any
}

// BrainDecision answers a permission request.
type BrainDecision struct {
	// Outcome is BrainAllow, BrainDeny or BrainCanceled (the turn ended or
	// the interaction expired first).
	Outcome string
	// Message is the refusal the model reads (deny only).
	Message string
	// Session is set when the user allowed the tool for the rest of the
	// session (BrainCapabilities.SessionAllow).
	Session bool
}

// BrainDecision outcomes.
const (
	BrainAllow    = "allow"
	BrainDeny     = "deny"
	BrainCanceled = "canceled"
)

// BrainEnv is what the host gives an adapter at Start.
type BrainEnv struct {
	// Command is the brain's argv; Command[0] is absolute.
	Command  []string
	Env      []string
	StateDir string
	// RunAs is the identity the brain process runs as (CoreOptions.RunAs).
	RunAs *RunAs
	// HostTools are the host tools the host offers the brain besides the six
	// platform tools; the brain reports those it takes in BrainCapabilities.HostTools.
	HostTools []BrainHostTool
	// PlatformModel and Upstream are the model routes the host can hand a
	// turn (BrainInput.Model); an adapter whose brain uses only its owner's
	// credentials refuses PlatformModel.
	PlatformModel bool
	Upstream      ACPModelUpstream
	// Models are the models the host lets a turn choose (ACPAgentOptions.Models).
	Models []AgentModel
	// AllowedTools reports the tools the brain may offer the model in a
	// session: the six platform tools and the host tools it took.
	AllowedTools func() map[string]bool
	CancelGrace  time.Duration
	KillGrace    time.Duration
	// HarnessID is the declared harness id ("" for the adapter's own).
	HarnessID      string
	DisablePrewarm bool
	Log            func(LogLevel, string, error)
	// SystemPrompt is the host's base system prompt for a working directory
	// (for a conversation the adapter opens on its own, such as a prewarm).
	SystemPrompt func(cwd string) string
}

// BrainHostTool is a host tool the host offers: name, model-facing
// description and JSON schema, and its default permission.
type BrainHostTool = acpHostTool

// BrainCapabilities is what a started brain can do. The host reads these
// fields only; it never asks which brain it is.
type BrainCapabilities struct {
	// Harness is the agent capability's harness entry (id, versions,
	// protocol, streaming, permission relay, session load, usage).
	Harness AgentHarnessCapability
	// ModelCredentials is BrainModelPlatform (the brain's model requests go
	// through the host's route, BrainInput.Model) or BrainModelOwner (the
	// brain uses only its node owner's own credentials).
	ModelCredentials string
	// ModelChoice: a turn may choose the model (config.model) and the brain
	// runs it. Without it config.model and thinking_level are ignored.
	ModelChoice bool
	// Models are the models the brain itself reports (nil: the host's
	// ACPAgentOptions.Models).
	Models []AgentModel
	// ThinkingLevels are the reasoning effort values the brain takes.
	ThinkingLevels []string
	// ImageInputs: the brain takes user images.
	ImageInputs bool
	// HostTools are the offered host tools the brain took.
	HostTools []string
	// CustomTools: the brain can call ask_user_question as a custom tool.
	CustomTools bool
	// InputIdempotent: the same input id handed again does not become a
	// second user message (with the host's ledger, turn_resume).
	InputIdempotent bool
	// SessionAllow: the brain keeps a permission the user granted for the
	// rest of the session.
	SessionAllow bool
}

// BrainOpen is what a turn asks the conversation to be.
type BrainOpen struct {
	// SessionID is the platform session id.
	SessionID string
	// Ref is the conversation to resume ("" for a new one).
	Ref string
	// Current is the conversation the host holds from an earlier turn (nil
	// when none); Open returns it when it is still ready.
	Current BrainSession
	// Cwd is the brain's working directory on this machine.
	Cwd string
	// SystemPrompt is the complete system prompt of the conversation.
	SystemPrompt string
	// Mode is the platform permission mode of the turn.
	Mode string
	// Model is the turn's model ("" when the brain picks its own).
	Model string
	// Metadata is the turn.start metadata (model limits).
	Metadata map[string]string
}

// BrainInput is one input of a turn.
type BrainInput struct {
	InputID string
	Text    string
	Images  []BrainImage
	// ToolPolicy is the turn's metadata.tool_policy (tool to allow, ask or deny).
	ToolPolicy map[string]string
	// Effort is the turn's thinking level ("" for none).
	Effort string
	// Model is where the brain's model requests go, for a brain whose model
	// requests the host forwards (nil otherwise).
	Model *BrainModelRoute
}

// BrainImage is one user image, downloaded and checked by the host.
type BrainImage struct {
	MimeType string
	Data     []byte
}

// BrainModelRoute is where one turn's model requests go: the platform's
// per-turn credential, or the node owner's Upstream with the turn's model.
type BrainModelRoute = acpRoute
