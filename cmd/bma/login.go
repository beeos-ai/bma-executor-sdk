//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func loginCmd() *cobra.Command {
	var withToken, noBrowser bool
	var apiURL, authURL string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in: approve this computer in the dashboard, or --with-token from stdin",
		Long: `Sign in the selected profile.

Without --with-token, bma asks the dashboard for a code (device
authorization), you check the computer and choose the organization and
workspace in the browser, and bma receives a personal access token named
"bma CLI on <computer>" (revoke it in the dashboard under your profile).

With --with-token, bma reads an organization API key or a personal access
token from stdin, e.g. "bma login --with-token < key.txt".`,
		Args: args(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := loadCLIConfig()
			if err != nil {
				return err
			}
			name := profileName(c)
			p := c.Profiles[name]
			if apiURL != "" {
				p.APIURL = strings.TrimRight(apiURL, "/")
			}
			if authURL != "" {
				p.AuthURL = strings.TrimRight(authURL, "/")
			}
			if opts.Workspace != "" {
				p.Workspace = opts.Workspace
			}
			var secret string
			if withToken {
				b, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
				if err != nil {
					return err
				}
				if secret = strings.TrimSpace(string(b)); secret == "" {
					return usageError("--with-token reads the key from stdin; stdin was empty")
				}
			} else {
				auth := firstNonEmpty(p.AuthURL, defaultAuthURL)
				host := computerName()
				codes, err := startDevice(cmd.Context(), auth, clientCLI, host)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "→ Approve the sign-in in your browser:\n    %s\n  Code: %s (check it matches the page)\n",
					codes.VerificationURIComplete, codes.UserCode)
				if !noBrowser && os.Getenv("SSH_CONNECTION") == "" {
					openBrowser(codes.VerificationURIComplete)
				}
				g, err := pollDevice(cmd.Context(), auth, clientCLI, codes)
				if err != nil {
					return err
				}
				secret, p.Organization, p.Workspace = g.AccessToken, g.OrganizationID, g.WorkspaceID
			}
			where, err := saveCredential(name, secret)
			if err != nil {
				return err
			}
			c.Profiles[name] = p
			if c.Current == "" {
				c.Current = name
			}
			if err := c.save(); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "✓ Signed in (profile %s, organization %s, workspace %s); credential kept in %s\n",
				name, orDash(p.Organization), orDash(p.Workspace), where)
			return nil
		},
	}
	cmd.Flags().BoolVar(&withToken, "with-token", false, "read an API key or personal access token from stdin")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the sign-in link instead of opening a browser")
	cmd.Flags().StringVar(&apiURL, "api", "", "API origin to store in the profile (default "+defaultAPIURL+")")
	cmd.Flags().StringVar(&authURL, "auth", "", "dashboard API origin to sign in through (default "+defaultAuthURL+")")
	return cmd
}

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the selected profile's credential",
		Long: "Forget the selected profile's credential on this computer. A personal access token from " +
			"`bma login` stays valid until you revoke it in the dashboard.",
		Args: args(0),
		RunE: func(*cobra.Command, []string) error {
			name, _, err := currentProfile()
			if err != nil {
				return err
			}
			if err := deleteCredential(name); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "✓ Signed out of profile %s (revoke the token in the dashboard to end it)\n", name)
			return nil
		},
	}
}

func authCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Show the sign-in or print its credential", Args: args(0),
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the profile, the credential's source and whether the API accepts it",
		Args:  args(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, _, err := currentProfile()
			if err != nil {
				return err
			}
			out := map[string]any{"profile": name, "credential": credentialSource(name)}
			s, err := apiSession()
			if errors.Is(err, errNotSignedIn) {
				out["signed_in"] = false
				if perr := printValue(out, false); perr != nil {
					return perr
				}
				return exitCode(3)
			}
			if err != nil {
				return err
			}
			out["api_url"], out["organization"], out["workspace"] = s.profile.APIURL, s.profile.Organization,
				s.profile.Workspace
			org, err := s.json(cmd.Context(), "GET", "v1/organizations/me", nil, nil)
			if err != nil {
				out["signed_in"] = false
				out["error"] = err.Error()
				if perr := printValue(out, false); perr != nil {
					return perr
				}
				return exitCode(exitCodeOf(err))
			}
			out["signed_in"] = true
			out["organization_info"] = org
			return printValue(out, false)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "token",
		Short: "Print the selected profile's credential (for scripts: curl -H \"x-api-key: $(bma auth token)\")",
		Args:  args(0),
		RunE: func(*cobra.Command, []string) error {
			name, _, err := currentProfile()
			if err != nil {
				return err
			}
			key := firstNonEmpty(opts.APIKey, os.Getenv("BMA_API_KEY"))
			if key == "" {
				if key, err = loadCredential(name); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintln(stdout, key)
			return err
		},
	})
	return cmd
}
