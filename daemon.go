package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrMachineTokenRequired is returned by the machine-token HTTP features
// (SessionContext, SessionContextByID, SessionFiles) of a client that is not
// a machine-scoped executor: Options.EnvironmentKey must hold the machine
// daemon token and Options.EnvironmentID must be empty.
var ErrMachineTokenRequired = errors.New("bmaexecutor: a machine-scoped executor (machine daemon token in EnvironmentKey, empty EnvironmentID) is required")

// DaemonHTTPError is a non-2xx answer of a BMA daemon route.
type DaemonHTTPError struct {
	StatusCode int
	// Code and Message are BMA's error body when it sent one.
	Code    string
	Message string
}

func (e *DaemonHTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("bmaexecutor: daemon API returned %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("bmaexecutor: daemon API returned %d", e.StatusCode)
}

// permanent reports a refusal that repeating the same request cannot change:
// a client error other than not found (a renewed anchor may succeed),
// timeout, conflict and rate limiting.
func (e *DaemonHTTPError) permanent() bool {
	switch e.StatusCode {
	case http.StatusNotFound, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return false
	}
	return e.StatusCode >= 400 && e.StatusCode < 500
}

// maxDaemonErrorBytes bounds an error body read from the daemon API.
const maxDaemonErrorBytes = 64 * 1024

// daemonPathSegment rejects an id that is not one path segment; the caller
// still escapes it.
func daemonPathSegment(kind, id string) error {
	if id == "" || len(id) > 256 || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00") {
		return fmt.Errorf("bmaexecutor: invalid %s %q", kind, id)
	}
	return nil
}

// daemonDo sends one request to <BaseURL>/api/v1/daemon<path> with the
// machine token, as bmad does. A non-2xx answer is a *DaemonHTTPError; the
// caller closes the body of a 2xx answer.
func (c *Client) daemonDo(ctx context.Context, method, path string, body io.Reader, size int64, header http.Header) (*http.Response, error) {
	if c.options.EnvironmentID != "" || strings.TrimSpace(c.options.EnvironmentKey) == "" {
		return nil, ErrMachineTokenRequired
	}
	request, err := http.NewRequestWithContext(ctx, method, c.options.BaseURL+"/api/v1/daemon"+path, body)
	if err != nil {
		return nil, err
	}
	for key, values := range header {
		request.Header[key] = values
	}
	request.ContentLength = size
	request.Header.Set("Authorization", "Bearer "+c.options.EnvironmentKey)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	// Redirects would forward the machine token elsewhere.
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, maxDaemonErrorBytes))
	status := &DaemonHTTPError{StatusCode: response.StatusCode}
	var apiErr struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &apiErr) == nil && apiErr.Message != "" {
		status.Code, status.Message = apiErr.Code, apiErr.Message
	} else {
		status.Message = strings.TrimSpace(string(raw))
	}
	return nil, status
}

// daemonJSON GETs a daemon route and decodes at most limit bytes of JSON.
func (c *Client) daemonJSON(ctx context.Context, path string, limit int64, out any) error {
	response, err := c.daemonDo(ctx, http.MethodGet, path, nil, 0, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return fmt.Errorf("bmaexecutor: read %s: %w", path, err)
	}
	if int64(len(raw)) > limit {
		return fmt.Errorf("bmaexecutor: %s response is too large", path)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("bmaexecutor: decode %s: %w", path, err)
	}
	return nil
}
