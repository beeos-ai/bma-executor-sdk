//go:build darwin || linux

// Command bma is the BMA command line (ADR-0102): one static binary that is
// both the API client and the BYO executor.
//
// API: `bma login`, then a command per operation of the public API, generated
// from the OpenAPI document (`bma sessions list`, `bma machines get <id>`,
// ...), plus `bma sessions events stream`, `bma sessions send`,
// `bma files upload|download`, `bma skills push`. Every command takes
// --json / --format json|jsonl|yaml|table and --query <jq>.
//
// BYO executor: `bma up` signs this computer in through the dashboard
// (device authorization, no token is copied), prepares the coding harnesses
// the owner enables (pinned releases, verified), checks the owner's own
// harness login, and installs a user service that runs the executor
// (`bma run`); `bma status`, `bma harness`, `bma disconnect`, `bma upgrade`,
// `bma uninstall`.
//
// Files: ~/.config/bma/config.yaml (CLI profiles, no secrets), the CLI
// credentials in the login keychain (macOS) or ~/.config/bma/credentials.json
// (0600); ~/.bma/config.json (the executor, no secrets), ~/.bma/state.json
// (written by the service), ~/.bma/harnesses/<id>/<version>/; the machine
// token in the login keychain (macOS) or ~/.config/bma/machine-token (0600).
//
// Exit codes: 0 success, 1 other failure, 2 usage, 3 not authenticated or
// not permitted (401/403), 4 not found (404), 5 conflict or precondition
// (409/412), 6 rate limited, server error or network after retries, 130
// interrupted.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// version is set at release (-ldflags "-X main.version=X.Y.Z").
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRoot().ExecuteContext(ctx)
	interrupted := ctx.Err() != nil
	stop()
	os.Exit(report(err, interrupted))
}

// report prints err (once) and answers the exit code.
func report(err error, interrupted bool) int {
	if err == nil {
		return 0
	}
	if interrupted {
		return 130
	}
	code := exitCodeOf(err)
	var silent exitCode
	if !errors.As(err, &silent) {
		printError(err)
	}
	return code
}

// exitCode ends the process with a code and no message.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }
