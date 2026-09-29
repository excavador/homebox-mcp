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
// The HTTP transport validates every request itself: a bearer token minted
// by access-roster (github.com/truvity/access-roster), checked against its
// JWKS with this server's own resource URL as the required audience (RFC
// 8707), and its own RFC 9728 Protected Resource Metadata document so a
// client can discover that issuer without being told out of band. A client
// identifies itself the way the Model Context Protocol's authorization
// spec recommends now that dynamic registration is deprecated there: a
// Client ID Metadata Document, an HTTPS URL access-roster's policy allows.
// See docs/design/cimd-auth.md. Exposing this with no --issuer-url and
// --resource-url set is exposing the whole inventory.
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
			&cli.StringFlag{
				Name:    "issuer-url",
				Usage:   "access-roster issuer that mints tokens for this resource (http transport only)",
				Sources: cli.EnvVars("ISSUER_URL"),
			},
			&cli.StringFlag{
				Name: "resource-url",
				// The RFC 8707 resource indicator a client names and the
				// audience access-roster mints for it -- this server's own
				// externally-reachable URL, not its in-cluster address.
				Usage:   "this server's own external URL (http transport only)",
				Sources: cli.EnvVars("RESOURCE_URL"),
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

	issuerURL := c.String("issuer-url")
	resourceURL := c.String("resource-url")
	if issuerURL == "" || resourceURL == "" {
		return fmt.Errorf("--issuer-url and --resource-url (or ISSUER_URL / RESOURCE_URL) are required for the http transport")
	}

	auth, err := server.NewAuth(issuerURL, resourceURL)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s }, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", auth.Protect(handler))
	mux.Handle("/mcp/", auth.Protect(handler))
	mux.Handle(server.MetadataPath, auth.Metadata())

	// Liveness only. It deliberately does NOT call HomeBox: a probe that
	// fails when a dependency is briefly unavailable restarts a process
	// that would otherwise have recovered on its own. It is also, on
	// purpose, unauthenticated -- a probe carries no bearer token.
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
