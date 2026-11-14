// Command bma connects this computer to BMA: one command (`bma up`) signs
// the computer in through the console (device authorization, no token is
// copied), prepares the coding harnesses the owner enables (pinned releases,
// verified), checks the owner's own harness login, and installs a user
// service that runs the executor (`bma run`).
//
//	bma up [--api URL] [--labels a,b] [--harness id,...] [--remote-brain] [--reauth] [--no-browser]
//	bma status [--json]
//	bma harness list | add <id> | login <id>
//	bma disconnect
//	bma run            (the service's entry; also runs in the foreground)
//	bma version
//
// Files: ~/.bma/config.json (no secrets), ~/.bma/state.json (written by the
// service), ~/.bma/harnesses/<id>/<version>/ (pinned harness releases). The
// machine token is kept in the login keychain (macOS) or in
// ~/.config/bma/machine-token (0600).
package main

import (
	"errors"
	"fmt"
	"os"
)

// version is set at release (-ldflags "-X main.version=X.Y.Z").
var version = "dev"

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		var exit exitCode
		if errors.As(err, &exit) {
			os.Exit(int(exit))
		}
		fmt.Fprintln(os.Stderr, "bma:", err)
		os.Exit(1)
	}
}

// exitCode ends the process with a code and no message.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

const usage = `usage: bma <command>

  up          connect this computer (sign in through the console, prepare harnesses, start the service)
  status      show the connection, the service and the harnesses
  harness     list | add <id> | login <id>
  disconnect  stop the service and remove this computer from BMA
  run         run the executor in the foreground (the service's entry)
  version     print the version`

func dispatch(args []string) error {
	if len(args) == 0 {
		fmt.Println(usage)
		return exitCode(2)
	}
	switch args[0] {
	case "up":
		return cmdUp(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "harness":
		return cmdHarness(args[1:])
	case "disconnect":
		return cmdDisconnect(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "version", "--version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], usage)
}
