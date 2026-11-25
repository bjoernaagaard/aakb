// Package adapter translates command and MCP request values into library inputs.
package adapter

import (
	"context"
	"strings"
	"time"

	"github.com/bjoernaagaard/aakb"
)

const (
	maxTimeout         = 10 * time.Minute
	maxDownloadTimeout = 2 * time.Hour
)

// WithTimeout applies an explicit timeout in seconds. Zero leaves the
// deadline unchanged so the library can supply its default. A negative
// value is rejected. A value above the cap for op, which is two hours for a
// download and ten minutes for anything else, is lowered to the cap.
func WithTimeout(ctx context.Context, op string, seconds int) (context.Context, context.CancelFunc, error) {
	if seconds < 0 {
		return nil, nil, &aakb.Error{Code: aakb.CodeInvalidArgument, Op: op, Message: "timeout_seconds must not be negative"}
	}
	if seconds == 0 {
		return ctx, func() {}, nil
	}
	limit := maxTimeout
	if op == "download" {
		limit = maxDownloadTimeout
	}
	timeout := limit
	if int64(seconds) < int64(limit/time.Second) {
		timeout = time.Duration(seconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, cancel, nil
}

// SearchKind applies the OpenAlex provider option used by older callers.
func SearchKind(kind, provider string) aakb.Kind {
	if strings.EqualFold(provider, "openalex") {
		return aakb.KindOpenAlex
	}
	return aakb.Kind(kind)
}

// SplitList parses one comma-separated filter argument.
func SplitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
