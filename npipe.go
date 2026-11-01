package bmaexecutor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Windows named pipes carry the same WebSocket upgrade as a Unix domain
// socket: the endpoint is npipe:////./pipe/<name> (the pipe \\.\pipe\<name>).
// The pipe code needs Windows (go-winio); elsewhere every pipe call returns
// ErrNamedPipeUnsupported, and the URL helpers still work.

// ErrNamedPipeUnsupported is returned by the named pipe transport on
// platforms other than Windows.
var ErrNamedPipeUnsupported = errors.New("bmaexecutor: Windows named pipes are not supported on this platform")

const pipeDir = `\\.\pipe\`

// pipePath returns the \\.\pipe\<name> path of a bare pipe name, a
// \\.\pipe\<name> path or the path of an npipe:////./pipe/<name> URL
// ("//./pipe/<name>").
func pipePath(address string) (string, error) {
	name := address
	for _, prefix := range []string{pipeDir, "//./pipe/", "/./pipe/"} {
		if strings.HasPrefix(address, prefix) {
			name = strings.TrimPrefix(address, prefix)
			break
		}
	}
	if name == "" || strings.ContainsAny(name, `\/`) {
		return "", fmt.Errorf("bmaexecutor: invalid named pipe %q", address)
	}
	return pipeDir + name, nil
}

// localURL is the unix:// or npipe:// URL of a local endpoint given as a
// socket path, a \\.\pipe\<name> path or an npipe:// URL.
func localURL(address string) string {
	switch {
	case strings.HasPrefix(address, "npipe:"):
		return address
	case strings.HasPrefix(address, pipeDir):
		return "npipe:////./pipe/" + strings.TrimPrefix(address, pipeDir)
	}
	return "unix://" + address
}

// pipeHTTPClient sends every request to the named pipe at path.
func pipeHTTPClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dialPipe(ctx, path) },
	}}
}

// NamedPipeDialer dials a harness serving Harness.ServeHTTP on the Windows
// named pipe \\.\pipe\<name> (NamedPipeListener). token.WSURL is ignored;
// StaticToken("npipe:////./pipe/"+name, token) with the default dialer is
// equivalent.
func NamedPipeDialer(name string) Dialer {
	return func(ctx context.Context, token ExecutorTokenResponse, protocol string, target ChannelTarget) (Conn, error) {
		return dialWebSocket(ctx, nil, localURL(`\\.\pipe\`+name), token.Token, protocol, target)
	}
}

// NamedPipeListener listens on the Windows named pipe \\.\pipe\<name> for
// the current user only (remote clients are refused). Serve Harness on it,
// as on a Unix socket listener.
func NamedPipeListener(name string) (net.Listener, error) {
	path, err := pipePath(name)
	if err != nil {
		return nil, err
	}
	return listenPipe(path)
}

// ListenNamedPipe is ListenUnixSocket on a Windows named pipe \\.\pipe\<name>:
// the executor listens and the next local harness that dials it with
// Authorization: Bearer token (Harness.DialExecutor with the pipe name or an
// npipe:// URL) becomes its brain.
func ListenNamedPipe(name, token string) (Dialer, func() error, error) {
	if token == "" {
		return nil, nil, errors.New("bmaexecutor: a local socket needs a token")
	}
	listener, err := NamedPipeListener(name)
	if err != nil {
		return nil, nil, err
	}
	dial, closer := serveLocal(listener, token)
	return dial, closer, nil
}
