package processing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// HookChecker sends each upload to an engine the operator runs (a PII or
// de-identification service, YARA, a second antivirus) and records what
// it answers. The request is a POST of the object's bytes:
//
//	POST <URL>
//	Authorization: Bearer <Token>
//	Content-Type: application/octet-stream
//	X-OSP-Asset-Id, X-OSP-File-Name, X-OSP-Content-Type, X-OSP-Size
//
// and the answer, with status 200:
//
//	{"findings": [{"code": "pii.ssn", "action": "reject", "detail": "3 US SSNs"}]}
//
// Codes are recorded as "external.<code>". An empty list means nothing was
// found. A failed or malformed answer is an error, so the job is retried
// and in the end held: the hook never passes a file it did not judge.
type HookChecker struct {
	URL    string
	Token  string
	Client *http.Client
	// MaxBytes: larger objects are not sent, and a note says so.
	MaxBytes int64
}

func (HookChecker) Name() string { return "external" }

var hookCode = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,60}$`)

func (h HookChecker) Validate(ctx context.Context, sample interfaces.ContentSample, _ model.ContentPolicy) (interfaces.ValidationResult, error) {
	if h.MaxBytes > 0 && sample.SizeBytes > h.MaxBytes {
		return interfaces.ValidationResult{Notes: []string{fmt.Sprintf("not sent to the external check: larger than %d bytes", h.MaxBytes)}}, nil
	}
	body, err := sample.Open(ctx)
	if err != nil {
		return interfaces.ValidationResult{}, fmt.Errorf("open object for the external check: %w", err)
	}
	defer body.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, body)
	if err != nil {
		return interfaces.ValidationResult{}, err
	}
	req.ContentLength = sample.SizeBytes
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-OSP-Asset-Id", sample.AssetID)
	req.Header.Set("X-OSP-File-Name", sample.FileName)
	req.Header.Set("X-OSP-Content-Type", sample.DetectedType)
	req.Header.Set("X-OSP-Size", strconv.FormatInt(sample.SizeBytes, 10))
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return interfaces.ValidationResult{}, fmt.Errorf("external check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return interfaces.ValidationResult{}, fmt.Errorf("external check answered %d", resp.StatusCode)
	}
	var answer struct {
		Findings []struct {
			Code   string `json:"code"`
			Action string `json:"action"`
			Detail string `json:"detail"`
		} `json:"findings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer); err != nil {
		return interfaces.ValidationResult{}, fmt.Errorf("external check answered with invalid JSON: %w", err)
	}
	var result interfaces.ValidationResult
	for _, f := range answer.Findings {
		if !hookCode.MatchString(f.Code) {
			return interfaces.ValidationResult{}, errors.New("external check answered with an invalid finding code")
		}
		result.Findings = append(result.Findings, model.ContentFinding{
			Code: "external." + f.Code, Action: f.Action, Detail: truncate(f.Detail, 300),
		})
	}
	return result, nil
}
