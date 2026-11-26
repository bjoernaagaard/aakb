package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bjoernaagaard/aakb/internal/httpapi"
	"github.com/bjoernaagaard/aakb/internal/mcpserver"
	"github.com/bjoernaagaard/aakb/internal/webui"
)

func serveHTTP(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, cfg, err := loadClient(logger)
	if err != nil {
		return err
	}
	server := mcpserver.New(client, logger)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: logger})

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen for HTTP: %w", err)
	}
	boundAddr := listener.Addr().String()
	api := httpapi.New(client, httpapi.Options{
		Token: cfg.HTTPToken, Addr: boundAddr, Version: version(), TrustedProxies: cfg.TrustedProxies,
	}, logger)
	mux := http.NewServeMux()
	mux.Handle("/mcp", api.Guard(handler))
	ui := api.PublicUI(webui.Handler())
	mux.Handle("GET /{$}", ui)
	mux.Handle("GET /ui/{file}", ui)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("upstream") == "1" {
			probeCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			up := client.UpstreamHealth(probeCtx)
			if _, err := fmt.Fprintf(w, `{"ok":true,"upstream":{"status":%q,"http_status":%d}}`, up.Status, up.HTTPStatus); err != nil {
				logger.WarnContext(r.Context(), "health response not written", "error", err)
			}
			return
		}
		if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
			logger.WarnContext(r.Context(), "health response not written", "error", err)
		}
	})
	srv := &http.Server{
		Addr:              boundAddr,
		Handler:           httpapi.Protect(api.Routes(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      40 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("HTTP shutdown did not finish cleanly", "error", err) //nolint:sloglint // the request context is already cancelled
		}
	}()
	logger.InfoContext(ctx, "aakb serving the web UI, REST and MCP over HTTP", "addr", boundAddr, "auth", cfg.HTTPToken != "")
	err = srv.Serve(listener)
	stop()
	<-done
	if !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}
	return nil
}
