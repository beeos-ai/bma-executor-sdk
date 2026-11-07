// Command sandboxed-folder runs this machine as a BMA executor whose
// processes work in a local folder chosen per session, inside a macOS
// sandbox-exec profile that only allows writes to that folder and the
// session's outputs.
//
// The caller creates the session on this machine's environment and puts two
// opaque references in the session metadata, for example
// {"workspace_ref": "ws_7", "owner": "u_42"}. Before each process starts, the
// Launch hook reads the session context, refuses a session of another owner
// (session_rejected), resolves workspace_ref with this machine's own folder
// table (BMA never stores the path; an unknown reference is
// workspace_unavailable), refuses a cwd outside the folder
// (workspace_boundary) and wraps the command in sandbox-exec. A process
// without agent_session_id is refused with session_rejected, unless the
// platform started it on its own (metadata initiator "platform", for example
// the machine desktop agent) and -platform-dir is set: that runs in its own
// directory by this machine's policy, never in a session folder. A package
// install is refused (local_confirmation_denied), which writes outside the folder.
//
//	BMA_BASE_URL=https://api.<env>.bma.beeos.ai BMA_MACHINE_TOKEN=... \
//	go run ./examples/sandboxed-folder -owner u_42 -folder ws_7=$HOME/projects/site \
//	  -session-files "$HOME/Library/Application Support/example/sessions" \
//	  -platform-dir "$HOME/Library/Application Support/example/platform"
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// Metadata keys this executor reads; the caller that creates sessions sets them.
const (
	metadataWorkspaceRef = "workspace_ref"
	metadataOwner        = "owner"
	// metadataInitiator marks a process the platform starts on its own; it is
	// an open set and "platform" is the only value defined. Such a process
	// carries no agent_session_id.
	metadataInitiator = "initiator"
	initiatorPlatform = "platform"
)

// sandboxProfile allows everything except writes outside the session's
// folder, its outputs and the temporary directories.
const sandboxProfile = `(version 1)
(allow default)
(deny file-write*)
(allow file-write*
  (subpath (param "FOLDER"))
  (subpath (param "OUTPUTS"))
  (subpath "/private/tmp")
  (subpath "/private/var/folders")
  (literal "/dev/null"))`

// policy is this machine's local configuration.
type policy struct {
	owner   string
	folders map[string]string // workspace_ref -> absolute folder
	// outputs returns the session's outputs directory ("" without session files).
	outputs func(agentSessionID string) string
	// wrap is true where sandbox-exec exists (macOS).
	wrap bool
	// platformDir is where platform-initiated processes run; empty means this
	// machine hosts none and refuses them.
	platformDir string
}

// admit refuses what no session or local policy can allow, before the session
// context is read. It reports platform == true for a process the platform
// started on its own, which has no session and is handled by launchPlatform.
// A process with neither agent_session_id nor a hosted platform initiator has
// no session to check the owner or workspace against: it is refused with
// session_rejected instead of falling back to a default folder (the harness
// does not send one; a person's console terminal is terminal.attach, not
// process.start). A package install runs a package manager that writes outside
// the folder; this example has no local confirmation, so it refuses.
func (p policy) admit(req bmaexecutor.LaunchRequest) (platform bool, err error) {
	if req.AgentSessionID == "" {
		if p.platformDir == "" || req.Metadata[metadataInitiator] != initiatorPlatform {
			return false, bmaexecutor.NewReasonError(bmaexecutor.CodeForbidden, bmaexecutor.ReasonSessionRejected,
				"the process names no agent session; this machine serves sessions only")
		}
		platform = true
	}
	if req.PackageInstall != nil {
		return false, bmaexecutor.NewReasonError(bmaexecutor.CodeForbidden, bmaexecutor.ReasonLocalConfirmationDenied,
			"package installs need a local confirmation this executor does not offer")
	}
	return platform, nil
}

// launchPlatform starts a platform-initiated process in platformDir under the
// same write-confining sandbox, apart from every session folder.
func (p policy) launchPlatform(req bmaexecutor.LaunchRequest) (bmaexecutor.LaunchSpec, error) {
	dir, err := confine(p.platformDir, req.Cwd)
	if err != nil {
		return bmaexecutor.LaunchSpec{}, err
	}
	spec := bmaexecutor.LaunchSpec{Dir: dir}
	if p.wrap {
		spec.Argv = p.sandboxed(p.platformDir, p.platformDir, req.Argv)
	}
	return spec, nil
}

// sandboxed wraps argv in sandbox-exec, allowing writes only to folder and outputs.
func (p policy) sandboxed(folder, outputs string, argv []string) []string {
	return append([]string{"/usr/bin/sandbox-exec", "-D", "FOLDER=" + folder, "-D", "OUTPUTS=" + outputs,
		"-p", sandboxProfile}, argv...)
}

// confine resolves cwd (relative to folder) and refuses one outside it.
func confine(folder, cwd string) (string, error) {
	dir := folder
	if cwd != "" {
		dir = cwd
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(folder, dir)
		}
		if !within(folder, dir) {
			return "", bmaexecutor.NewReasonError(bmaexecutor.CodeForbidden,
				bmaexecutor.ReasonWorkspaceBoundary, "cwd is outside the allowed folder")
		}
	}
	return dir, nil
}

// launch decides how one process of a session starts.
func (p policy) launch(session bmaexecutor.SessionContext, req bmaexecutor.LaunchRequest) (bmaexecutor.LaunchSpec, error) {
	if session.Metadata[metadataOwner] != p.owner {
		return bmaexecutor.LaunchSpec{}, bmaexecutor.NewReasonError(bmaexecutor.CodeForbidden,
			bmaexecutor.ReasonSessionRejected, "this machine does not serve the session's owner")
	}
	folder, ok := p.folders[session.Metadata[metadataWorkspaceRef]]
	if !ok {
		return bmaexecutor.LaunchSpec{}, bmaexecutor.NewReasonError(bmaexecutor.CodeForbidden,
			bmaexecutor.ReasonWorkspaceUnavailable, "the session's workspace is not known on this machine")
	}
	if info, err := os.Stat(folder); err != nil || !info.IsDir() {
		return bmaexecutor.LaunchSpec{}, bmaexecutor.NewReasonError(bmaexecutor.CodeForbidden,
			bmaexecutor.ReasonWorkspaceUnavailable, "the session's workspace folder does not exist")
	}
	dir, err := confine(folder, req.Cwd)
	if err != nil {
		return bmaexecutor.LaunchSpec{}, err
	}
	spec := bmaexecutor.LaunchSpec{Dir: dir}
	if p.wrap {
		outputs := folder
		if p.outputs != nil && req.AgentSessionID != "" {
			outputs = p.outputs(req.AgentSessionID)
		}
		spec.Argv = p.sandboxed(folder, outputs, req.Argv)
	}
	return spec, nil
}

// within reports whether target is folder or below it, after resolving
// symlinks of the existing part (a link inside the folder may point out).
func within(folder, target string) bool {
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type folderFlag map[string]string

func (f folderFlag) String() string { return fmt.Sprint(map[string]string(f)) }
func (f folderFlag) Set(value string) error {
	ref, folder, ok := strings.Cut(value, "=")
	if !ok || ref == "" || !filepath.IsAbs(folder) {
		return errors.New("want <workspace_ref>=<absolute folder>")
	}
	f[ref] = filepath.Clean(folder)
	return nil
}

func main() {
	folders := folderFlag{}
	owner := flag.String("owner", "", "the owner reference this machine serves")
	workRoot := flag.String("work-root", "bma-work", "directory holding session workspaces")
	sessionFilesRoot := flag.String("session-files", "", "absolute session files root (optional)")
	platformDir := flag.String("platform-dir", "", "absolute directory for platform-initiated processes, e.g. the desktop agent (optional; without it they are refused)")
	flag.Var(folders, "folder", "<workspace_ref>=<absolute folder>, repeatable")
	flag.Parse()
	if *owner == "" || len(folders) == 0 {
		log.Fatal("-owner and at least one -folder are required")
	}

	core, err := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: *workRoot})
	if err != nil {
		log.Fatal(err)
	}
	defer core.Close()
	local := policy{owner: *owner, folders: folders, wrap: runtime.GOOS == "darwin"}
	if *platformDir != "" {
		if !filepath.IsAbs(*platformDir) {
			log.Fatal("-platform-dir must be an absolute path")
		}
		if err := os.MkdirAll(*platformDir, 0o700); err != nil {
			log.Fatal(err)
		}
		local.platformDir = filepath.Clean(*platformDir)
	}
	var files *bmaexecutor.SessionFiles
	if *sessionFilesRoot != "" {
		if files, err = bmaexecutor.NewSessionFiles(bmaexecutor.SessionFilesOptions{Root: *sessionFilesRoot}); err != nil {
			log.Fatal(err)
		}
		local.outputs = func(id string) string { _, outputs := files.Dirs(id); return outputs }
	}
	var client *bmaexecutor.Client
	launch := func(ctx context.Context, req bmaexecutor.LaunchRequest) (bmaexecutor.LaunchSpec, error) {
		platform, err := local.admit(req)
		if err != nil {
			return bmaexecutor.LaunchSpec{}, err
		}
		if platform {
			return local.launchPlatform(req)
		}
		session, err := client.SessionContext(ctx, req.ProcessID)
		if errors.Is(err, bmaexecutor.ErrSessionContextNotFound) {
			return bmaexecutor.LaunchSpec{}, bmaexecutor.NewReasonError(bmaexecutor.CodeForbidden,
				bmaexecutor.ReasonSessionRejected, "the process has no session on this machine")
		}
		if err != nil {
			return bmaexecutor.LaunchSpec{}, err // answered as spawn_failed
		}
		spec, err := local.launch(session, req)
		if err == nil {
			log.Printf("process %s of %s in %s (argv %q)", req.ProcessID, session.ID, spec.Dir, spec.Argv)
		}
		return spec, err
	}
	if err := core.Register(bmaexecutor.NewProcessCapability(core, bmaexecutor.ProcessOptions{Launch: launch, SessionFiles: files})); err != nil {
		log.Fatal(err)
	}
	client, err = bmaexecutor.NewClient(bmaexecutor.Options{
		BaseURL: os.Getenv("BMA_BASE_URL"),
		// A machine daemon token and no environment id: a machine-scoped
		// executor, which may read session contexts and session files.
		EnvironmentKey: os.Getenv("BMA_MACHINE_TOKEN"),
		Executor:       bmaexecutor.ExecutorInfo{Name: "sandboxed-folder-example", OS: runtime.GOOS, Arch: runtime.GOARCH, Version: "0.1.0"},
		Capabilities:   core.Capabilities(),
		OnLog:          func(e bmaexecutor.LogEvent) { log.Printf("%s: %s %v", e.Level, e.Message, e.Err) },
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := core.Attach("cloud", client); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if files != nil {
		go files.Run(ctx)
	}
	registered, err := client.Start(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("registered as %s (Ctrl-C to stop)", registered.ExecutorID)
	<-ctx.Done()
	_ = client.Stop(context.Background(), "shutdown")
}
