package bmaexecutor

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// SessionContext is what BMA tells this machine about an agent session it
// serves (GET /api/v1/daemon/processes/{process_id}/session and
// GET /api/v1/daemon/sessions/{session_id}): the session's environment and
// the metadata its creator set, never agent configuration or credentials.
// Executors read it to resolve a reference the caller put in metadata (a
// project folder, an owner) before running anything, as Claude Managed
// Agents workers read the session's metadata for a claimed work item.
type SessionContext struct {
	ID            string            `json:"id"`
	EnvironmentID string            `json:"environment_id"`
	Metadata      map[string]string `json:"metadata"`
	CreatedAt     time.Time         `json:"created_at"`
}

// ErrSessionContextNotFound is returned when the process is not on this
// machine (or ended more than 15 minutes ago), or the session is not bound
// to this machine.
var ErrSessionContextNotFound = errors.New("bmaexecutor: session context not found")

const (
	// maxSessionContextBytes bounds one answer (16 metadata pairs of at
	// most 64 + 512 bytes, with room to spare).
	maxSessionContextBytes = 64 * 1024
	// maxCachedSessionContexts bounds each cache; the oldest entry goes.
	maxCachedSessionContexts = 1024
)

// sessionContextCache keeps the first read of each session (metadata is
// read once, as of the session's start) and the session of each process.
type sessionContextCache struct {
	mu        sync.Mutex
	sessions  boundedMap[SessionContext]
	processes boundedMap[string]
}

type boundedMap[V any] struct {
	values map[string]V
	order  []string
}

func (m *boundedMap[V]) get(key string) (V, bool) {
	value, ok := m.values[key]
	return value, ok
}

func (m *boundedMap[V]) put(key string, value V) {
	if m.values == nil {
		m.values = map[string]V{}
	}
	if _, ok := m.values[key]; !ok {
		if len(m.order) >= maxCachedSessionContexts {
			delete(m.values, m.order[0])
			m.order = m.order[1:]
		}
		m.order = append(m.order, key)
	}
	m.values[key] = value
}

// SessionContext returns the context of the agent session processID runs
// for (process.start metadata names it as agent_session_id). It needs a
// machine-scoped executor (ErrMachineTokenRequired). Results are cached per
// session; a failure is not.
func (c *Client) SessionContext(ctx context.Context, processID string) (SessionContext, error) {
	c.sessionContexts.mu.Lock()
	sessionID, known := c.sessionContexts.processes.get(processID)
	c.sessionContexts.mu.Unlock()
	if known {
		return c.SessionContextByID(ctx, sessionID)
	}
	if err := daemonPathSegment("process id", processID); err != nil {
		return SessionContext{}, err
	}
	session, err := c.fetchSessionContext(ctx, "/processes/"+url.PathEscape(processID)+"/session")
	if err != nil {
		return SessionContext{}, err
	}
	c.sessionContexts.mu.Lock()
	defer c.sessionContexts.mu.Unlock()
	c.sessionContexts.processes.put(processID, session.ID)
	if cached, ok := c.sessionContexts.sessions.get(session.ID); ok {
		return cached.clone(), nil
	}
	c.sessionContexts.sessions.put(session.ID, session)
	return session.clone(), nil
}

// SessionContextByID returns the context of an agent session bound to this
// machine, for capabilities without a process (an agent turn.start carries
// only the session id). Cached like SessionContext.
func (c *Client) SessionContextByID(ctx context.Context, sessionID string) (SessionContext, error) {
	c.sessionContexts.mu.Lock()
	cached, ok := c.sessionContexts.sessions.get(sessionID)
	c.sessionContexts.mu.Unlock()
	if ok {
		return cached.clone(), nil
	}
	if err := daemonPathSegment("session id", sessionID); err != nil {
		return SessionContext{}, err
	}
	session, err := c.fetchSessionContext(ctx, "/sessions/"+url.PathEscape(sessionID))
	if err != nil {
		return SessionContext{}, err
	}
	c.sessionContexts.mu.Lock()
	defer c.sessionContexts.mu.Unlock()
	if cached, ok := c.sessionContexts.sessions.get(session.ID); ok {
		return cached.clone(), nil
	}
	c.sessionContexts.sessions.put(session.ID, session)
	return session.clone(), nil
}

func (c *Client) fetchSessionContext(ctx context.Context, path string) (SessionContext, error) {
	var session SessionContext
	err := c.daemonJSON(ctx, path, maxSessionContextBytes, &session)
	var status *DaemonHTTPError
	if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
		return SessionContext{}, ErrSessionContextNotFound
	}
	if err != nil {
		return SessionContext{}, err
	}
	if session.ID == "" {
		return SessionContext{}, errors.New("bmaexecutor: session context response has no id")
	}
	return session, nil
}

func (s SessionContext) clone() SessionContext {
	s.Metadata = maps.Clone(s.Metadata)
	if s.Metadata == nil {
		s.Metadata = map[string]string{}
	}
	return s
}
