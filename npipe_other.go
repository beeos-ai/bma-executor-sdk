//go:build !windows

package bmaexecutor

import (
	"context"
	"net"
)

func dialPipe(context.Context, string) (net.Conn, error) { return nil, ErrNamedPipeUnsupported }

func listenPipe(string) (net.Listener, error) { return nil, ErrNamedPipeUnsupported }
