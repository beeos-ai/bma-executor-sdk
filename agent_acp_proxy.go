package bmaexecutor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// acpModelProxy is the machine-local model endpoint of an ACPAgent. The
// agent only knows http://127.0.0.1:<port>/v1 and a per-session placeholder
// key; the proxy forwards POST /v1/chat/completions while that session has a
// running turn, after checking that the request offers only the six platform
// tools, and adds the real credential. The forwarded request is built anew:
// of the agent's headers only Accept is kept, so the agent cannot set
// Upstream.SessionHeader itself. Bodies and credentials are never logged.
type acpModelProxy struct {
	agent  *ACPAgent
	url    string
	server *http.Server
	client *http.Client
}

const acpMaxModelRequestBytes = 32 << 20

func startACPModelProxy(a *ACPAgent) (*acpModelProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &acpModelProxy{agent: a, url: "http://" + listener.Addr().String(), client: &http.Client{}}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.server.Serve(listener) }()
	return p, nil
}

func (p *acpModelProxy) close() { _ = p.server.Close() }

// routeFor is the route of the running turn of the session holding placeholder.
func (a *ACPAgent) routeFor(placeholder string) (acpRoute, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		if s.placeholder == placeholder {
			if s.turn == nil {
				return acpRoute{}, s.id, false
			}
			return s.turn.route, s.id, true
		}
	}
	return acpRoute{}, "", false
}

func (p *acpModelProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	route, sessionID, running := p.agent.routeFor(key)
	if !running {
		reason := "unknown key"
		if sessionID != "" {
			reason = "no running turn in session " + sessionID
		}
		p.agent.log(LogWarn, "acp model proxy refused a request: "+reason, nil)
		http.Error(w, "forbidden: "+reason, http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, acpMaxModelRequestBytes+1))
	if err != nil || len(body) > acpMaxModelRequestBytes {
		http.Error(w, "request too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if names := acpExtraTools(request["tools"], p.agent.toolNames()); len(names) > 0 {
		p.agent.log(LogWarn, fmt.Sprintf("acp model proxy refused tools %v in session %s", names, sessionID), nil)
		http.Error(w, "forbidden: tool catalog not allowed: "+strings.Join(names, ","), http.StatusForbidden)
		return
	}
	if route.model != "" {
		request["model"], _ = json.Marshal(route.model)
		body, _ = json.Marshal(request)
	}
	base, header := route.baseURL, route.header
	if route.platform != nil {
		var token string
		base, token = route.platform.credential(r.Context())
		header = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad upstream", http.StatusBadGateway)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	if accept := r.Header.Get("Accept"); accept != "" {
		upstream.Header.Set("Accept", accept)
	}
	if route.sessionHeader != "" {
		upstream.Header.Set(route.sessionHeader, sessionID)
	}
	if header != nil {
		header(upstream)
	}
	resp, err := p.client.Do(upstream)
	if err != nil {
		p.agent.log(LogWarn, "acp model proxy: upstream unreachable", err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Retry-After"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// acpHeaderName reports whether name is an HTTP header field name (an RFC
// 9110 token); the transport refuses to send any other.
func acpHeaderName(name string) bool {
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			return false
		}
	}
	return name != ""
}

// acpExtraTools lists the offered tools that are not in allowed (the platform tools and the host tools).
func acpExtraTools(raw json.RawMessage, allowed map[string]bool) []string {
	if len(raw) == 0 {
		return nil
	}
	var tools []struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &tools) != nil {
		return []string{"<unreadable tools>"}
	}
	var extra []string
	for _, t := range tools {
		name := t.Function.Name
		if name == "" {
			name = t.Name
		}
		if !allowed[name] {
			extra = append(extra, name)
		}
	}
	return extra
}
