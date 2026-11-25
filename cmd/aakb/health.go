package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"time"
)

// cliHealth asks a running aakb serve for /healthz. It backs the Docker
// HEALTHCHECK, because the image holds no shell and no curl.
func cliHealth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address of the running server")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if host, port, err := net.SplitHostPort(*addr); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		*addr = net.JoinHostPort("127.0.0.1", port)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+*addr+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("health request for %s: %w", *addr, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // only the status code is used
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: server answered HTTP %d", resp.StatusCode)
	}
	fmt.Println("ok")
	return nil
}
