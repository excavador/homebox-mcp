// Command homebox-mcp serves a HomeBox inventory over the Model Context
// Protocol: entities, tags, entity types, templates, maintenance records and
// attachments, read and written.
//
// The destructive corners of the API are deliberately not exposed -- see
// "What it will not do" in the README.
//
// Two transports:
//
//	stdio       the default, for running it locally next to a client
//	http        streamable HTTP, for running it in a cluster
//
// The HTTP transport does NO authentication of its own. It is meant to sit
// behind a gateway that validates a token -- on hive that is Envoy checking
// a Zitadel-issued JWT against the homebox project audience. Exposing this
// directly to a network is exposing the whole inventory.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urfave/cli/v3"

	"github.com/excavador/homebox-mcp/internal/homebox"
	"github.com/excavador/homebox-mcp/internal/server"
)

// version is overridden at build time.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := &cli.Command{
		Name:    "homebox-mcp",
		Usage:   "MCP server for a HomeBox inventory",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "homebox-url",
				Usage:    "base URL of the HomeBox instance, without /api",
				Sources:  cli.EnvVars("HOMEBOX_URL"),
				Required: true,
			},
			&cli.StringFlag{
				Name: "homebox-token",
				// A HomeBox API key, not an OIDC token: HomeBox's API
				// accepts only its own bearer tokens.
				Usage:    "HomeBox API key",
				Sources:  cli.EnvVars("HOMEBOX_TOKEN"),
				Required: true,
			},
			&cli.StringFlag{
				Name:    "transport",
				Usage:   "stdio or http",
				Value:   "stdio",
				Sources: cli.EnvVars("TRANSPORT"),
			},
			&cli.StringFlag{
				Name: "addr",
				// 0.0.0.0, not 127.0.0.1: in a pod, loopback means nothing
				// can reach it, including the readiness probe.
				Usage:   "listen address for the http transport",
				Value:   "0.0.0.0:8080",
				Sources: cli.EnvVars("ADDR"),
			},
		},
		Action: run,
	}

	if err := cmd.Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "homebox-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, c *cli.Command) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	client := homebox.New(c.String("homebox-url"), c.String("homebox-token"))

	// Fail at startup rather than on the first tool call. A bad token here
	// otherwise surfaces to a user as an unexplained tool error, one layer
	// away from the cause.
	name, email, group, err := client.Self(ctx)
	if err != nil {
		return fmt.Errorf("homebox unreachable or token rejected: %w", err)
	}

	log.Info("authenticated to homebox",
		"user", name, "email", email, "group", group, "version", version)

	s := server.New(client, version)

	if c.String("transport") == "stdio" {
		return s.Run(ctx, &mcp.StdioTransport{})
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s }, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle("/mcp/", handler)

	// Liveness only. It deliberately does NOT call HomeBox: a probe that
	// fails when a dependency is briefly unavailable restarts a process
	// that would otherwise have recovered on its own.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              c.String("addr"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()

		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()

		_ = srv.Shutdown(shutdown)
	}()

	log.Info("serving mcp over http", "addr", srv.Addr)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}
