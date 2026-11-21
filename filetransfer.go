package bmaexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// MaxFileTransferBytes bounds one file transfer (v1.1 §6.2): 1 GiB.
const MaxFileTransferBytes = 1 << 30

const maxFileTransferErrorBytes = 1024

// fileTransferer moves one file between the machine and the process's BMA file
// endpoint (POST/GET /api/v1/daemon/processes/{process_id}/file), as bmad does,
// with the machine token of the client the process was started through
// (ProcessContext.Client).
type fileTransferer struct{}

// NewFileTransferer returns the FileTransferer for ProcessOptions.FileTransfer
// of a machine-scoped executor (ErrMachineTokenRequired otherwise, on the
// first transfer). An upload streams a regular file (at most
// MaxFileTransferBytes) and is checked against the digest BMA stored. A
// download streams into a temporary file next to the destination and replaces
// it only after the length and sha256 match, keeping the destination's
// permissions (on Windows: its read-only attribute); a leading "~" in the path
// is the user's home directory (USERPROFILE on Windows). The workload's own
// permissions are the executor's: a privileged executor should give a Launch
// hook, not this, the job of confining paths. edit_file downloads are not
// supported (BMA then answers the agent that the executor cannot edit).
func NewFileTransferer() SessionFileTransferer { return fileTransferer{} }

// TransferFile is not used: the process capability calls TransferSessionFile,
// which knows the client.
func (fileTransferer) TransferFile(context.Context, string, ProcessFileTransfer) (FileTransferResult, error) {
	err := errors.New("bmaexecutor: NewFileTransferer needs the process context (TransferSessionFile)")
	return FileTransferResult{Error: &FileTransferError{Code: "file_transfer_failed", Message: err.Error()}}, err
}

// TransferSessionFile implements SessionFileTransferer. A failure is both the
// returned error and FileTransferResult.Error.
func (t fileTransferer) TransferSessionFile(ctx context.Context, process ProcessContext, transfer ProcessFileTransfer) (FileTransferResult, error) {
	result, err := t.transfer(ctx, process, transfer)
	if err == nil {
		return result, nil
	}
	failure := FileTransferError{Code: "file_transfer_failed", Message: err.Error()}
	var status *DaemonHTTPError
	if errors.As(err, &status) && status.Code != "" {
		failure.Code = status.Code
	}
	if len(failure.Message) > maxFileTransferErrorBytes {
		failure.Message = failure.Message[:maxFileTransferErrorBytes]
	}
	return FileTransferResult{Error: &failure}, err
}

func (t fileTransferer) transfer(ctx context.Context, process ProcessContext, transfer ProcessFileTransfer) (FileTransferResult, error) {
	processID, client := process.ProcessID, process.Client
	if client == nil {
		return FileTransferResult{}, errors.New("bmaexecutor: no client for the file transfer")
	}
	if err := daemonPathSegment("process id", processID); err != nil {
		return FileTransferResult{}, err
	}
	path, err := expandHomePath(transfer.LocalPath)
	if err != nil {
		return FileTransferResult{}, err
	}
	if transfer.Direction == FileTransferUpload {
		return t.upload(ctx, client, processID, path)
	}
	return t.download(ctx, client, processID, path)
}

// expandHomePath expands a leading "~" (alone or before a separator).
func expandHomePath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(path[1:], `\`, "/"))), nil
}

func (fileTransferer) upload(ctx context.Context, client *Client, processID, path string) (FileTransferResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return FileTransferResult{}, fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return FileTransferResult{}, fmt.Errorf("inspect file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return FileTransferResult{}, errors.New("source must be a regular file")
	}
	if info.Size() > MaxFileTransferBytes {
		return FileTransferResult{}, errors.New("file exceeds the upload size limit")
	}
	hash := sha256.New()
	var body io.Reader
	if info.Size() > 0 {
		body = io.TeeReader(io.NewSectionReader(file, 0, info.Size()), hash)
	}
	response, err := client.daemonDo(ctx, http.MethodPost,
		"/processes/"+url.PathEscape(processID)+"/file?filename="+url.QueryEscape(filepath.Base(path)),
		body, info.Size(), http.Header{"Accept": {"application/json"}})
	if err != nil {
		return FileTransferResult{}, fmt.Errorf("transfer file: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSessionUploadResponse))
	if err != nil {
		return FileTransferResult{}, fmt.Errorf("read file transfer response: %w", err)
	}
	var result FileTransferResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return FileTransferResult{}, fmt.Errorf("decode file upload response: %w", err)
	}
	if local := fmt.Sprintf("sha256:%x", hash.Sum(nil)); result.Digest != local {
		return FileTransferResult{}, fmt.Errorf("file upload digest mismatch: sent %s, stored %s", local, result.Digest)
	}
	return result, nil
}

func (fileTransferer) download(ctx context.Context, client *Client, processID, path string) (FileTransferResult, error) {
	response, err := client.daemonDo(ctx, http.MethodGet, "/processes/"+url.PathEscape(processID)+"/file", nil, 0,
		http.Header{"Accept": {"application/octet-stream"}})
	if err != nil {
		return FileTransferResult{}, fmt.Errorf("transfer file: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	digest := response.Header.Get(fileDigestHeader)
	if !validDigest(digest) {
		return FileTransferResult{}, errors.New("file transfer response contains an invalid digest")
	}
	if err := replaceWithDownload(path, response.Body, digest); err != nil {
		return FileTransferResult{}, err
	}
	return FileTransferResult{Digest: digest}, nil
}

// replaceWithDownload streams body into a temporary file next to path and
// replaces path only after the length and digest match.
func replaceWithDownload(path string, body io.Reader, digest string) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".bma-file-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporary.Name())
	}()
	info, err := os.Stat(path)
	switch {
	case err == nil && info.Mode().IsRegular():
		if err := temporary.Chmod(info.Mode().Perm()); err != nil {
			return fmt.Errorf("preserve destination permissions: %w", err)
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect destination: %w", err)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(body, MaxFileTransferBytes+1))
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	if written > MaxFileTransferBytes {
		return errors.New("file download exceeds the size limit")
	}
	if fmt.Sprintf("sha256:%x", hash.Sum(nil)) != digest {
		return errors.New("file download digest mismatch")
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close file: %w", err)
	}
	if err := replacePath(temporary.Name(), path); err != nil {
		return fmt.Errorf("replace destination: %w", err)
	}
	return nil
}
