package bmaexecutor

import (
	"fmt"
	"os"
)

// fakeClaudeCLI stands in for a claude binary that answers --version only.
func fakeClaudeCLI(version string) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "--version" {
		fmt.Println(version + " (Claude Code)")
		os.Exit(0)
	}
	os.Exit(2)
}

// fakeCodexCLI stands in for a codex binary that answers --version only.
func fakeCodexCLI(version string) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "--version" {
		fmt.Println("codex-cli " + version)
		os.Exit(0)
	}
	os.Exit(2)
}
