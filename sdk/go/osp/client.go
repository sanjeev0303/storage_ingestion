// Package osp is the object storage platform's Go SDK (docs/sdks.md):
// resumable, hash-verified uploads, with a bandwidth limit and progress that
// can be kept across restarts. It uses only the standard library, so it fits
// on small devices.
package osp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIError is the API's refusal.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

// IsStatus reports whether err is the API answering this status.
func IsStatus(err error, status int) bool {
	var a *APIError
	return errors.As(err, &a) && a.Status == status
}

// Client calls the API with a token: an API token or a device's credential.
type Client struct {
	BaseURL string
	Token   string
	// HTTP makes the requests; the default never times out a whole request,
	// because a slow link is normal, and gives up on a silent connection.
	HTTP *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 2 * time.Minute, IdleConnTimeout: time.Minute, MaxIdleConnsPerHost: 8},
	}}
}

// Do sends a JSON request and decodes the JSON answer into out, if any.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var problem struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(raw, &problem)
		if problem.Error == "" {
			problem.Error = strings.TrimSpace(string(raw))
		}
		return &APIError{Status: resp.StatusCode, Code: problem.Code, Message: problem.Error}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Query builds a path with its parameters escaped.
func Query(path string, params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	return path + "?" + q.Encode()
}
