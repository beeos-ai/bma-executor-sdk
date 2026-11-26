//go:build !windows

package bmaexecutor

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// endRead reads processID until it ended and returns that read: the one that
// uploads the session's outputs and reports them.
func endRead(t *testing.T, h processHarness, processID string) ProcessReadResult {
	t.Helper()
	cursor := int64(0)
	for {
		read, err := h.read(t, ProcessReadParams{ProcessID: processID, Stream: StreamStdout, Cursor: cursor, WaitMS: 2000})
		if err != nil {
			t.Fatalf("process.read: %v", err)
		}
		cursor = read.NextCursor
		if read.EOF && endedProcessState(read.State) {
			return read
		}
	}
}

// A first upload that fails is retried before the read that reports the
// process's end answers, and the read lists the output with its digest and
// uploaded=true: the tool result never claims success for an upload that
// silently failed, and the platform learns which version the turn left.
func TestProcessEndRetriesAFailedOutputUploadAndReportsIt(t *testing.T) {
	daemon := &fakeDaemonFiles{processes: map[string]string{"proc_w": "sesn_w"}, failUploads: 1}
	h, files, _ := newSessionFilesHarness(t, daemon)
	_, outputs := files.Dirs("sesn_w")
	h.mustStart(t, ProcessStartParams{ProcessID: "proc_w",
		Argv:     []string{"sh", "-c", fmt.Sprintf(`printf report > %q`, filepath.Join(outputs, "报告.pdf"))},
		Metadata: map[string]string{MetadataAgentSessionID: "sesn_w"}})
	read := endRead(t, h, "proc_w")
	want := []SessionOutputReport{{Path: "报告.pdf", Digest: digestOf([]byte("report")), Uploaded: true}}
	if fmt.Sprint(read.SessionOutputs) != fmt.Sprint(want) {
		t.Fatalf("session_outputs = %+v, want %+v", read.SessionOutputs, want)
	}
	if got := daemon.uploaded(); len(got) != 1 || got[0].path != "报告.pdf" {
		t.Fatalf("uploaded before the read answered: %+v", got)
	}
	// Reading the ended process again finds nothing pending.
	again, err := h.read(t, ProcessReadParams{ProcessID: "proc_w", Stream: StreamStdout, Cursor: read.NextCursor})
	if err != nil || len(again.SessionOutputs) != 0 {
		t.Fatalf("second read: %+v %v", again.SessionOutputs, err)
	}
}

// An upload still failing when the bound passes is reported uploaded=false
// (with the digest of the content on the machine) instead of a success; the
// read answers within the bound.
func TestProcessEndReportsAnOutputUploadStillFailing(t *testing.T) {
	bound := sessionFilesFlushTimeout
	sessionFilesFlushTimeout = time.Second
	t.Cleanup(func() { sessionFilesFlushTimeout = bound })
	daemon := &fakeDaemonFiles{processes: map[string]string{"proc_f": "sesn_f"}, failUploads: -1}
	h, files, _ := newSessionFilesHarness(t, daemon)
	_, outputs := files.Dirs("sesn_f")
	h.mustStart(t, ProcessStartParams{ProcessID: "proc_f",
		Argv:     []string{"sh", "-c", fmt.Sprintf(`printf final > %q && printf draft > %q`, filepath.Join(outputs, "报告.pdf"), filepath.Join(outputs, "draft.md"))},
		Metadata: map[string]string{MetadataAgentSessionID: "sesn_f"}})
	started := time.Now()
	read := endRead(t, h, "proc_f")
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the read took %v, past its bound", took)
	}
	got := map[string]SessionOutputReport{}
	for _, o := range read.SessionOutputs {
		got[o.Path] = o
	}
	if len(got) != 2 || got["报告.pdf"] != (SessionOutputReport{Path: "报告.pdf", Digest: digestOf([]byte("final"))}) ||
		got["draft.md"].Uploaded {
		t.Fatalf("session_outputs = %+v", read.SessionOutputs)
	}
	// Once the platform accepts uploads again, the next read of the ended
	// process uploads them and says so.
	daemon.mu.Lock()
	daemon.failUploads = 0
	daemon.mu.Unlock()
	again, err := h.read(t, ProcessReadParams{ProcessID: "proc_f", Stream: StreamStdout, Cursor: read.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range again.SessionOutputs {
		if !o.Uploaded {
			t.Fatalf("still pending after the platform recovered: %+v", again.SessionOutputs)
		}
	}
	if len(again.SessionOutputs) != 2 || len(daemon.uploaded()) != 2 {
		t.Fatalf("second read %+v, uploaded %+v", again.SessionOutputs, daemon.uploaded())
	}
}
