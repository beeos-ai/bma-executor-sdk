package bmaexecutor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeFileEndpoint is BMA's /api/v1/daemon/processes/{id}/file.
type fakeFileEndpoint struct {
	stored   []byte
	filename string
	download []byte
}

func (f *fakeFileEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer mtok" ||
		!strings.HasPrefix(r.URL.Path, "/api/v1/daemon/processes/prc_") || !strings.HasSuffix(r.URL.Path, "/file") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodPost:
		f.stored, _ = io.ReadAll(r.Body)
		f.filename = r.URL.Query().Get("filename")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"path":"/mnt/session/outputs/` + f.filename + `","digest":"` + digestOf(f.stored) + `"}`))
	case http.MethodGet:
		if f.download == nil {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"conflict","message":"nothing to download"}`))
			return
		}
		w.Header().Set(fileDigestHeader, digestOf(f.download))
		_, _ = w.Write(f.download)
	}
}

// fileTransfer binds NewFileTransferer to a client of the fake endpoint.
type fileTransfer struct {
	transferer SessionFileTransferer
	client     *Client
}

func (f fileTransfer) TransferFile(ctx context.Context, processID string, transfer ProcessFileTransfer) (FileTransferResult, error) {
	return f.transferer.TransferSessionFile(ctx, ProcessContext{ProcessID: processID, Client: f.client}, transfer)
}

func newFileTransferClient(t *testing.T, endpoint *fakeFileEndpoint) fileTransfer {
	t.Helper()
	server := httptest.NewServer(endpoint)
	t.Cleanup(server.Close)
	client, err := NewClient(Options{
		BaseURL: server.URL, EnvironmentKey: "mtok", Executor: ExecutorInfo{Name: "ft-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fileTransfer{transferer: NewFileTransferer(), client: client}
}

func TestFileTransfererUploadAndDownload(t *testing.T) {
	endpoint := &fakeFileEndpoint{download: []byte("from bma")}
	transferer := newFileTransferClient(t, endpoint)
	dir := t.TempDir()
	source := filepath.Join(dir, "report one.txt")
	if err := os.WriteFile(source, []byte("deliverable"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := transferer.TransferFile(testContext(t), "prc_1", ProcessFileTransfer{Direction: FileTransferUpload, LocalPath: source})
	if err != nil || result.Digest != digestOf([]byte("deliverable")) || endpoint.filename != "report one.txt" || string(endpoint.stored) != "deliverable" {
		t.Fatalf("upload = %+v, %v (filename %q)", result, err, endpoint.filename)
	}

	// Download replaces an existing read-only file and keeps its read-only mode.
	target := filepath.Join(dir, "download.txt")
	if err := os.WriteFile(target, []byte("old"), 0o444); err != nil {
		t.Fatal(err)
	}
	result, err = transferer.TransferFile(testContext(t), "prc_1", ProcessFileTransfer{Direction: FileTransferDownload, LocalPath: target})
	if err != nil || result.Digest != digestOf([]byte("from bma")) {
		t.Fatalf("download = %+v, %v", result, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "from bma" {
		t.Fatalf("downloaded content = %q", got)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("mode after download = %v (%v)", info, err)
	}
	// A new destination is created; no temporary file is left behind.
	fresh := filepath.Join(dir, "fresh.txt")
	if _, err := transferer.TransferFile(testContext(t), "prc_1", ProcessFileTransfer{Direction: FileTransferDownload, LocalPath: fresh}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".bma-file-") {
			t.Fatalf("temporary file %s left behind", entry.Name())
		}
	}
}

func TestFileTransfererReportsFailures(t *testing.T) {
	transferer := newFileTransferClient(t, &fakeFileEndpoint{})
	result, err := transferer.TransferFile(testContext(t), "prc_1",
		ProcessFileTransfer{Direction: FileTransferDownload, LocalPath: filepath.Join(t.TempDir(), "x")})
	if err == nil || result.Error == nil || result.Error.Code != "conflict" || !strings.Contains(result.Error.Message, "nothing to download") {
		t.Fatalf("download failure = %+v, %v", result, err)
	}
	result, err = transferer.TransferFile(testContext(t), "prc_1",
		ProcessFileTransfer{Direction: FileTransferUpload, LocalPath: t.TempDir()})
	if err == nil || result.Error == nil || !strings.Contains(result.Error.Message, "regular file") {
		// Opening a directory fails on Windows before the type check.
		if runtime.GOOS != "windows" || err == nil {
			t.Fatalf("upload of a directory = %+v, %v", result, err)
		}
	}
	if _, err := transferer.TransferFile(testContext(t), "../x", ProcessFileTransfer{Direction: FileTransferUpload, LocalPath: "x"}); err == nil {
		t.Fatal("a process id that is not a path segment was accepted")
	}
}

func TestExpandHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for in, want := range map[string]string{
		"~":                 home,
		"~/a/b.txt":         filepath.Join(home, "a", "b.txt"),
		`~\a\b.txt`:         filepath.Join(home, "a", "b.txt"),
		"~user/x":           "~user/x",
		"relative/file.txt": "relative/file.txt",
	} {
		if got, err := expandHomePath(in); err != nil || got != want {
			t.Errorf("expandHomePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
