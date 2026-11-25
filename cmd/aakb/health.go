package main

import (
	"context"
	"net/http"
	"strings"
)

func upstreamReachable(ctx context.Context, base string) (string, int) {
	if base == "" {
		base = "https://annas-archive.gl"
	}
	if !strings.HasPrefix(base, "http") {
		base = "https://" + base
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base, nil)
	if err != nil {
		return "unreachable", 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "unreachable", 0
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // HEAD probe body is empty
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return "ok", resp.StatusCode
	}
	return "challenged", resp.StatusCode
}
