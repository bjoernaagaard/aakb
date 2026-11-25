// Package adapter translates command and MCP request values into library inputs.
package adapter

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/bjoernaagaard/aakb"
)

// WithTimeout applies an explicit timeout in seconds. Zero leaves the
// deadline unchanged so the library can supply its default.
func WithTimeout(ctx context.Context, op string, seconds int) (context.Context, context.CancelFunc, error) {
	if seconds < 0 || int64(seconds) > int64(math.MaxInt64/time.Second) {
		return nil, nil, &aakb.Error{Code: aakb.CodeInvalidArgument, Op: op, Message: "timeout_seconds is outside the supported range"}
	}
	if seconds == 0 {
		return ctx, func() {}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
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
