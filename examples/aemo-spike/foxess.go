package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is the part of foxess-local's REST API this sidecar uses
// (api/openapi.yaml; GET /api/v1/openapi.yaml on the server).
type Client struct {
	Base   string // http://127.0.0.1:18080
	Token  string // a standard token for this sidecar alone
	Client *http.Client
}

// Override is the API's ScheduleOverride (the fields used here).
type Override struct {
	Active bool      `json:"active"`
	Holder string    `json:"holder"`
	Mode   string    `json:"mode"`
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
}

// State is the part of GET /state used here.
type State struct {
	Online  bool `json:"online"`
	Battery struct {
		SocPct *int `json:"soc_pct"`
	} `json:"battery"`
	Override *Override `json:"schedule_override"`
}

// responseLimit bounds one API response.
const responseLimit = 1 << 20

// APIError is a refused request (RFC 7807).
type APIError struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Detail)
}

// State reads GET /state.
func (c *Client) State(ctx context.Context) (State, error) {
	var st State
	err := c.do(ctx, http.MethodGet, "/api/v1/state", nil, &st)
	return st, err
}

// SetOverride starts or renews a force discharge until until.
func (c *Client) SetOverride(ctx context.Context, until time.Time, cutoffSoc, powerW int) (Override, error) {
	var o Override
	err := c.do(ctx, http.MethodPut, "/api/v1/schedule/override", map[string]any{
		"mode": "force-discharge", "until": until.UTC().Format(time.RFC3339),
		"fd_soc_pct": cutoffSoc, "fd_pwr_w": powerW,
	}, &o)
	return o, err
}

// ClearOverride ends the override now.
func (c *Client) ClearOverride(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/schedule/override", nil, nil)
}

// Notice sends a notice (webhook / ntfy / the web UI's list), from this
// sidecar's token.
func (c *Client) Notice(ctx context.Context, event, severity, title, message string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/notices", map[string]string{
		"event": event, "severity": severity, "title": title, "message": message,
	}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit))
	if err != nil {
		return err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		e := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(b, e) // a non-JSON body keeps the status alone
		return e
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}
