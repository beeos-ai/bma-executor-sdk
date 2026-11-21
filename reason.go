package bmaexecutor

// Recommended error.data.reason values (contracts/bma README, open set):
// an executor that refuses a process.start or a tool.call answers with a
// JSON-RPC error whose data.reason is one of these, and BMA passes it to the
// tool result's error_code and the turn's session.error.code unchanged. Any
// other value is passed through as well. TypeScript ErrorReason and Python
// ErrorReason carry the same names and values.
const (
	// The referenced workspace is gone or its directory does not exist.
	ReasonWorkspaceUnavailable = "workspace_unavailable"
	// The request leaves what the executor's local policy allows.
	ReasonWorkspaceBoundary = "workspace_boundary"
	// The request writes where the local policy only allows reads.
	ReasonWorkspaceReadOnly = "workspace_read_only"
	// The session is not one this machine serves (an owner check failed).
	ReasonSessionRejected = "session_rejected"
	// The local user denied the access.
	ReasonLocalConfirmationDenied = "local_confirmation_denied"
	// Nobody answered the local confirmation in time.
	ReasonLocalConfirmationExpired = "local_confirmation_expired"
	// The process could not be started.
	ReasonSpawnFailed = "spawn_failed"
	// A process.start replay changed the spec of an existing process id.
	ReasonProcessIDSpecMismatch = "process_id_spec_mismatch"
)

// MetadataAgentSessionID is the process.start metadata key naming the agent
// session a process runs for (the session SessionContext and SessionFiles
// look up).
const MetadataAgentSessionID = "agent_session_id"

// NewReasonError returns the JSON-RPC error a handler or a Launch hook
// returns to refuse a request with a machine-readable reason, for example
// NewReasonError(CodeForbidden, ReasonSessionRejected, "not this machine's user").
func NewReasonError(code int, reason, message string) *RPCError {
	return &RPCError{Code: code, Message: message, Data: &JSONRPCErrorData{Reason: reason}}
}
