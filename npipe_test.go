package bmaexecutor

import (
	"errors"
	"runtime"
	"testing"
)

func TestNamedPipeAddressForms(t *testing.T) {
	for _, address := range []string{`bma-x`, `\\.\pipe\bma-x`, `//./pipe/bma-x`} {
		if got, err := pipePath(address); err != nil || got != `\\.\pipe\bma-x` {
			t.Fatalf("pipePath(%q) = %q, %v", address, got, err)
		}
	}
	for _, address := range []string{``, `\\.\pipe\`, `a\b`, `//./pipe/a/b`} {
		if got, err := pipePath(address); err == nil {
			t.Fatalf("pipePath(%q) = %q, want an error", address, got)
		}
	}
	for address, want := range map[string]string{
		`\\.\pipe\bma-x`:         `npipe:////./pipe/bma-x`,
		`npipe:////./pipe/bma-x`: `npipe:////./pipe/bma-x`,
		`/run/bma.sock`:          `unix:///run/bma.sock`,
	} {
		if got := localURL(address); got != want {
			t.Fatalf("localURL(%q) = %q, want %q", address, got, want)
		}
	}
}

func TestChannelURLAcceptsNamedPipes(t *testing.T) {
	got, client, err := channelURL(nil, "npipe:////./pipe/bma-x", ChannelTarget{Channel: ChannelMCP, SessionID: "ses_1", ServerKey: "echo"})
	if err != nil || client == nil {
		t.Fatalf("channelURL = %q, %v, %v", got, client, err)
	}
	if want := "ws://local/?channel=mcp&server_key=echo&session_id=ses_1"; got != want {
		t.Fatalf("channelURL = %q, want %q", got, want)
	}
	if _, _, err := channelURL(nil, "npipe:////./pipe/a/b", ChannelTarget{Channel: ChannelControl}); err == nil {
		t.Fatal("a pipe name with a separator was accepted")
	}
}

func TestNamedPipesNeedWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by npipe_windows_test.go")
	}
	if _, err := NamedPipeListener("bma-x"); !errors.Is(err, ErrNamedPipeUnsupported) {
		t.Fatalf("NamedPipeListener err = %v", err)
	}
	if _, _, err := ListenNamedPipe("bma-x", "token"); !errors.Is(err, ErrNamedPipeUnsupported) {
		t.Fatalf("ListenNamedPipe err = %v", err)
	}
	_, err := NamedPipeDialer("bma-x")(testContext(t), ExecutorTokenResponse{}, ProtocolV11, ChannelTarget{Channel: ChannelControl})
	if !errors.Is(err, ErrNamedPipeUnsupported) {
		t.Fatalf("NamedPipeDialer err = %v", err)
	}
}
