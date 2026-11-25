//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	bmasdk "github.com/beeos-ai/bma-sdk/v3"
	"github.com/spf13/cobra"
)

// globals are the flags every command takes.
type globals struct {
	Profile   string
	APIURL    string
	APIKey    string
	Workspace string
	Format    string
	JSON      bool
	Query     string
}

// opts are this process's global flags (one command per process).
var opts globals

var stdout io.Writer = os.Stdout

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "bma",
		Short: "The BMA command line: the public API and this computer as a BYO executor",
		Long: `bma is the BMA command line (BeeOS Managed Agents).

Sign in with "bma login" (or set BMA_API_KEY), then use a command per API
operation, e.g. "bma sessions list", "bma machines get <id>". Connect this
computer as a BYO executor with "bma up".

Output: an API command prints the API's JSON answer, indented. --format yaml
prints YAML; --format jsonl prints it on one line. A list prints one page
(the page object with data and next_page); on a terminal it prints the
page's items as a table, and --format table does so everywhere. --all fetches
every page and prints the items (with --format jsonl, one item per line).
--query applies a jq expression first and prints each result, a string
result as raw text. A command that downloads content writes it to stdout or
to --output.

Exit codes: 0 success; 1 other failure; 2 usage error; 3 not signed in, or
the API answered 401 or 403; 4 not found (404); 5 conflict or failed
precondition (409, 412); 6 rate limited (429), server error (5xx) or network
failure after the retries; 130 interrupted. An error is written to stderr:
one line, or with --json the API's error envelope as it answered.`,
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if opts.JSON {
				if cmd.Flags().Changed("format") && opts.Format != "json" {
					return usageError("--json and --format " + opts.Format + " disagree")
				}
				opts.Format = "json"
			}
			switch opts.Format {
			case "", "json", "jsonl", "yaml", "table":
				return nil
			}
			return usageError("--format must be json, jsonl, yaml or table")
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	f := root.PersistentFlags()
	f.StringVar(&opts.Profile, "profile", "", "profile of ~/.config/bma/config.yaml (default: BMA_PROFILE, else the current one)")
	f.StringVar(&opts.APIURL, "api-url", "", "API origin (default: BMA_BASE_URL, else the profile's)")
	f.StringVar(&opts.APIKey, "api-key", "", "API key or personal access token (default: BMA_API_KEY, else the profile's credential)")
	f.StringVar(&opts.Workspace, "workspace", "", "workspace id (default: BMA_WORKSPACE_ID, else the profile's)")
	f.StringVar(&opts.Format, "format", "", "output format: json, jsonl, yaml or table (default: table on a terminal for lists, else json)")
	f.BoolVar(&opts.JSON, "json", false, "same as --format json")
	f.StringVar(&opts.Query, "query", "", "jq expression applied to the output (gojq)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err.Error()) })

	root.AddGroup(&cobra.Group{ID: "api", Title: "API resources:"},
		&cobra.Group{ID: "auth", Title: "Sign-in and profiles:"},
		&cobra.Group{ID: "byo", Title: "This computer as a BYO executor:"})
	addAPICommands(root)
	addHandWritten(root)
	for _, c := range []*cobra.Command{loginCmd(), logoutCmd(), authCmd(), profilesCmd()} {
		c.GroupID = "auth"
		root.AddCommand(c)
	}
	for _, c := range byoCmds() {
		c.GroupID = "byo"
		root.AddCommand(c)
	}
	root.AddCommand(versionCmd(), docsCmd())
	addExamples(root)
	return root
}

// handExamples are the examples of the hand-written commands, by command
// path (the generated API commands carry their own, from the contract).
var handExamples = map[string]string{
	"bma login": `bma login
bma login --no-browser
printf '%s' "$BMA_API_KEY" | bma login --with-token`,
	"bma logout": `bma logout
bma logout --profile work`,
	"bma auth status":     "bma auth status",
	"bma auth token":      `curl -sS "$BMA_BASE_URL/v1/agents" -H "x-api-key: $(bma auth token)"`,
	"bma profiles list":   "bma profiles list",
	"bma profiles use":    "bma profiles use work",
	"bma profiles set":    "bma profiles set work --api https://api.beeos.ai --workspace-id <workspace_id>",
	"bma profiles delete": "bma profiles delete work",
	"bma up": `bma up
bma up --harness claude-code --labels office,gpu
bma up --reauth --no-browser`,
	"bma status": `bma status
bma status --json`,
	"bma harness": `bma harness list
bma harness add claude-code
bma harness login claude-code`,
	"bma disconnect": "bma disconnect",
	"bma upgrade": `bma upgrade --check
bma upgrade`,
	"bma uninstall": "bma uninstall --yes",
	"bma version": `bma version
bma version --json`,
	"bma sessions send": `bma sessions send <session_id> "Summarize the open issues" --wait
echo "Next step" | bma sessions send <session_id> - --stream`,
	"bma sessions events stream": "bma sessions events stream <session_id> --until-idle",
	"bma files upload": `bma files upload ./report.pdf
bma files upload ./report.pdf --expires-in-seconds 86400`,
	"bma skills push": `bma skills push ./my-skill
bma skills push ./my-skill --skill <skill_id>`,
}

// addExamples sets the hand-written commands' examples.
func addExamples(root *cobra.Command) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if ex, ok := handExamples[c.CommandPath()]; ok {
			c.Example = indent(ex, "  ")
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// usageErr is a usage error (exit 2).
type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

func usageError(msg string) error { return usageErr{msg} }

// args checks the number of positional arguments as a usage error.
func args(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if len(a) != n {
			return usageError(fmt.Sprintf("%s takes %d argument(s), got %d\nusage: %s", cmd.CommandPath(), n,
				len(a), cmd.UseLine()))
		}
		return nil
	}
}

func exitCodeOf(err error) int {
	var silent exitCode
	if errors.As(err, &silent) {
		return int(silent)
	}
	var u usageErr
	if errors.As(err, &u) || strings.HasPrefix(err.Error(), "unknown command") ||
		strings.HasPrefix(err.Error(), "unknown flag") {
		return 2
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	if errors.Is(err, errNotSignedIn) {
		return 3
	}
	var api *anthropic.Error
	if errors.As(err, &api) {
		switch {
		case api.StatusCode == http.StatusUnauthorized, api.StatusCode == http.StatusForbidden:
			return 3
		case api.StatusCode == http.StatusNotFound:
			return 4
		case api.StatusCode == http.StatusConflict, api.StatusCode == http.StatusPreconditionFailed:
			return 5
		case api.StatusCode == http.StatusTooManyRequests, api.StatusCode >= 500:
			return 6
		}
		return 1
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) {
		return 6
	}
	return 1
}

// printError writes err to stderr: one line, or with --json the API error
// envelope as the API answered it.
func printError(err error) {
	var api *anthropic.Error
	if errors.As(err, &api) {
		if opts.Format == "json" && api.RawJSON() != "" {
			fmt.Fprintln(os.Stderr, api.RawJSON())
			return
		}
		detail := []string{}
		if t := string(api.Type()); t != "" {
			if c := bmasdk.ErrorCode(err); c != "" {
				t += "/" + c
			}
			detail = append(detail, t)
		}
		if api.RequestID != "" {
			detail = append(detail, "request_id "+api.RequestID)
		}
		msg := api.Message
		if msg == "" {
			msg = http.StatusText(api.StatusCode)
		}
		if len(detail) > 0 {
			msg += " (" + strings.Join(detail, ", ") + ")"
		}
		fmt.Fprintln(os.Stderr, "error:", msg)
		return
	}
	fmt.Fprintln(os.Stderr, "error:", err)
}

// session is what an API command needs: the resolved profile and a client.
type session struct {
	profile profile
	client  *bmasdk.Client
}

// apiSession resolves the credential and the profile into a client.
func apiSession() (*session, error) {
	name, p, err := currentProfile()
	if err != nil {
		return nil, err
	}
	key := firstNonEmpty(opts.APIKey, os.Getenv("BMA_API_KEY"))
	if key == "" {
		if key, err = loadCredential(name); err != nil {
			return nil, err
		}
	}
	p.APIURL = strings.TrimRight(firstNonEmpty(opts.APIURL, os.Getenv("BMA_BASE_URL"), p.APIURL, defaultAPIURL), "/")
	p.Workspace = firstNonEmpty(opts.Workspace, os.Getenv("BMA_WORKSPACE_ID"), p.Workspace)
	o := []option.RequestOption{option.WithAPIKey(key), option.WithBaseURL(p.APIURL + "/"),
		option.WithHeader("User-Agent", "bma/"+version)}
	if p.Workspace != "" {
		o = append(o, bmasdk.WithProject(p.Workspace))
	}
	if p.Organization != "" {
		o = append(o, option.WithHeader("bma-organization-id", p.Organization))
	}
	return &session{profile: p, client: bmasdk.NewClient(o...)}, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  args(0),
		RunE: func(*cobra.Command, []string) error {
			if opts.Format == "" {
				_, err := fmt.Fprintln(stdout, version)
				return err
			}
			return printValue(map[string]any{"version": version, "contract": contractSource}, false)
		},
	}
}
