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
// (workspace_boundary) and wraps the command in sandbox-exec.
//
//	BMA_BASE_URL=https://api.<env>.bma.beeos.ai BMA_MACHINE_TOKEN=... \
//	go run ./examples/sandboxed-folder -owner u_42 -folder ws_7=$HOME/projects/site \
//	  -session-files "$HOME/Library/Application Support/example/sessions"
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
	dir := folder
	if req.Cwd != "" {
		dir = req.Cwd
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(folder, dir)
		}
		if !within(folder, dir) {
			return bmaexecutor.LaunchSpec{}, bmaexecutor.NewReasonError(bmaexecutor.CodeForbidden,
				bmaexecutor.ReasonWorkspaceBoundary, "cwd is outside the session's workspace folder")
		}
	}
	spec := bmaexecutor.LaunchSpec{Dir: dir}
	if p.wrap {
		outputs := folder
		if p.outputs != nil && req.AgentSessionID != "" {
			outputs = p.outputs(req.AgentSessionID)
		}
		spec.Argv = append([]string{"/usr/bin/sandbox-exec", "-D", "FOLDER=" + folder, "-D", "OUTPUTS=" + outputs,
			"-p", sandboxProfile}, req.Argv...)
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
	var files *bmaexecutor.SessionFiles
	if *sessionFilesRoot != "" {
		if files, err = bmaexecutor.NewSessionFiles(bmaexecutor.SessionFilesOptions{Root: *sessionFilesRoot}); err != nil {
			log.Fatal(err)
		}
		local.outputs = func(id string) string { _, outputs := files.Dirs(id); return outputs }
	}
	var client *bmaexecutor.Client
	launch := func(ctx context.Context, req bmaexecutor.LaunchRequest) (bmaexecutor.LaunchSpec, error) {
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
