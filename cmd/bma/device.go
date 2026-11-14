package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// The device authorization grant (RFC 8628) of the console: this computer
// asks for a code, the owner approves it in the browser (choosing the
// workspace, the name and labels), and the poll answers the machine token.

const clientID = "bma-machine"

type deviceCodes struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type deviceGrant struct {
	AccessToken string   `json:"access_token"`
	MachineID   string   `json:"bma_machine_id"`
	WorkspaceID string   `json:"bma_workspace_id"`
	DisplayName string   `json:"bma_display_name"`
	Labels      []string `json:"bma_labels"`
	ExecutorURL string   `json:"bma_executor_url"`
}

// apiError is the error envelope of the BMA API.
type apiError struct {
	Status int
	Error  struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *apiError) String() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Error.Code, e.Error.Message)
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// postJSON posts in and decodes a 200 answer into out; any other status is
// an *apiError.
func postJSON(ctx context.Context, url string, in, out any) (*apiError, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bma/"+version)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		e := &apiError{Status: resp.StatusCode}
		if json.Unmarshal(body, e) != nil || e.Error.Code == "" {
			return nil, fmt.Errorf("%s: %d %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		return e, nil
	}
	return nil, json.Unmarshal(body, out)
}

func startDevice(ctx context.Context, api, hostname string) (deviceCodes, error) {
	var codes deviceCodes
	e, err := postJSON(ctx, api+"/v1/auth/device_authorizations", map[string]any{
		"client_id":   clientID,
		"bma_machine": map[string]string{"hostname": hostname, "os": runtime.GOOS, "arch": runtime.GOARCH},
	}, &codes)
	if err == nil && e != nil {
		err = fmt.Errorf("starting the sign-in: %s", e)
	}
	return codes, err
}

// pollDevice waits for the owner's decision (RFC 8628 §3.5).
func pollDevice(ctx context.Context, api string, codes deviceCodes) (deviceGrant, error) {
	interval := time.Duration(codes.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(codes.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return deviceGrant{}, ctx.Err()
		case <-time.After(interval):
		}
		var g deviceGrant
		e, err := postJSON(ctx, api+"/v1/auth/device_authorizations/token", map[string]string{
			"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": codes.DeviceCode,
			"client_id": clientID,
		}, &g)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "  (retrying:", err, ")")
		case e == nil:
			return g, nil
		case e.Error.Code == "authorization_pending":
		case e.Error.Code == "slow_down":
			interval += 5 * time.Second
		case e.Error.Code == "access_denied":
			return deviceGrant{}, fmt.Errorf("the request was denied in the console")
		case e.Error.Code == "expired_token":
			return deviceGrant{}, fmt.Errorf("the code expired: run `bma up` again")
		default:
			return deviceGrant{}, fmt.Errorf("signing in: %s", e)
		}
	}
	return deviceGrant{}, fmt.Errorf("the code expired: run `bma up` again")
}
