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
// The HTTP transport holds NO authentication code. In a cluster it runs behind
// the sluis resource-proxy sidecar (github.com/truvity/sluis, cmd/resource-proxy),
// which checks every caller's access token, serves the RFC 9728 Protected
// Resource Metadata, writes the audit line and forwards to this server over
// loopback. So there are three listeners, each with one job:
//
//	--addr         the MCP endpoint, /mcp, loopback only (refused otherwise)
//	--upload-addr  the signed upload link, PUT <path of --public-url>/upload/<token>,
//	               and nothing else; reachable from the gateway, no bearer token
//	--ops-addr     /healthz for the kubelet, and nothing else
//
// Exposing --addr beyond loopback is exposing the whole inventory with no
// authentication, which is why a non-loopback address is refused unless
// --allow-non-loopback says so. See docs/design/cimd-auth.md.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
			&cli.DurationFlag{
				Name: "homebox-timeout",
				// Counts the whole request, body included. A streamed upload is
				// bound by --upload-timeout instead.
				Usage:   "deadline for an ordinary request to HomeBox",
				Value:   homebox.DefaultTimeout,
				Sources: cli.EnvVars("HOMEBOX_TIMEOUT"),
			},
			&cli.StringFlag{
				Name:    "transport",
				Usage:   "stdio or http",
				Value:   "stdio",
				Sources: cli.EnvVars("TRANSPORT"),
			},
			&cli.StringFlag{
				Name: "addr",
				// Loopback, not 0.0.0.0: this listener has no authentication of its
				// own. The resource-proxy sidecar in the same pod is what faces
				// the network and forwards to it here.
				Usage:   "listen address of the MCP endpoint, loopback only (http transport only)",
				Value:   defaultMCPAddr,
				Sources: cli.EnvVars("ADDR"),
			},
			&cli.BoolFlag{
				Name: "allow-non-loopback",
				// An escape hatch for a deployment whose network does the
				// isolation (a pod IP that nothing but a trusted proxy reaches).
				Usage:   "let --addr bind a non-loopback address: the MCP endpoint then has NO authentication (http transport only)",
				Sources: cli.EnvVars("ALLOW_NON_LOOPBACK"),
			},
			&cli.StringFlag{
				Name: "upload-addr",
				// Reachable by the gateway, so not loopback in a pod: the chart
				// sets the pod's own IP. It serves the upload link and nothing
				// else.
				Usage:   "listen address of the signed upload link, which is NOT behind the proxy (http transport only)",
				Value:   defaultUploadAddr,
				Sources: cli.EnvVars("UPLOAD_ADDR"),
			},
			&cli.StringFlag{
				Name:    "ops-addr",
				Usage:   "listen address of /healthz, for the kubelet; serves nothing else (http transport only)",
				Value:   defaultOpsAddr,
				Sources: cli.EnvVars("OPS_ADDR"),
			},
			&cli.StringFlag{
				Name: "public-url",
				// What a client uses to reach this server from outside -- the
				// resource URL the sidecar checks tokens for. Here it only builds
				// the upload links, and its path is where the upload listener
				// serves them.
				Usage:   "this server's own external URL, the base of upload links (http transport only)",
				Sources: cli.EnvVars("PUBLIC_URL"),
			},
			&cli.Int64Flag{
				Name: "upload-max-bytes",
				// HomeBox's own limit is HBOX_WEB_MAX_UPLOAD_SIZE (MiB, default
				// 10). Raise both together, or the refusal moves to HomeBox.
				Usage:   "largest file an upload link accepts (http transport only)",
				Value:   server.DefaultMaxBytes,
				Sources: cli.EnvVars("UPLOAD_MAX_BYTES"),
			},
			&cli.DurationFlag{
				Name:    "upload-ttl",
				Usage:   "how long an upload link stays valid (http transport only)",
				Value:   10 * time.Minute,
				Sources: cli.EnvVars("UPLOAD_TTL"),
			},
			&cli.DurationFlag{
				Name: "upload-timeout",
				// A hundred megabytes over a slow uplink takes minutes.
				Usage:   "how long one whole upload may take, receiving and storing it (http transport only)",
				Value:   server.DefaultUploadTimeout,
				Sources: cli.EnvVars("UPLOAD_TIMEOUT"),
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

	// Refuse an unsafe or incomplete http configuration before anything else:
	// before HomeBox is contacted, and before any listener is bound.
	var publicURL string

	if c.String("transport") != "stdio" {
		var err error

		if publicURL, err = checkHTTPConfig(c, log); err != nil {
			return err
		}
	}

	client := homebox.New(c.String("homebox-url"), c.String("homebox-token"),
		homebox.WithTimeout(c.Duration("homebox-timeout")))

	// Fail at startup rather than on the first tool call. A bad token here
	// otherwise surfaces to a user as an unexplained tool error, one layer
	// away from the cause.
	name, email, group, err := client.Self(ctx)
	if err != nil {
		return fmt.Errorf("homebox unreachable or token rejected: %w", err)
	}

	log.Info("authenticated to homebox",
		"user", name, "email", email, "group", group, "version", version)

	if c.String("transport") == "stdio" {
		return server.New(client, version).Run(ctx, &mcp.StdioTransport{})
	}

	// Upload links are signed with a key derived from the HomeBox API key, so
	// there is no second secret to provision or rotate. The key is derived,
	// not the token itself: the signature must not be an oracle for it.
	keyMAC := hmac.New(sha256.New, []byte(c.String("homebox-token")))
	keyMAC.Write([]byte("homebox-mcp/upload-link/v1"))

	uploads, err := server.NewUploader(client, server.UploadConfig{
		BaseURL:  publicURL,
		Key:      keyMAC.Sum(nil),
		MaxBytes: c.Int64("upload-max-bytes"),
		TTL:      c.Duration("upload-ttl"),
		Timeout:  c.Duration("upload-timeout"),
	})
	if err != nil {
		return err
	}

	s := server.New(client, version, server.WithUploads(uploads))

	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s }, nil)

	listeners := []listener{
		{name: "mcp", addr: c.String("addr"), handler: mcpMux(mcpHandler)},
		// The signed upload link is reachable WITHOUT a bearer token on
		// purpose: the caller is a curl in a shell, and the link is the
		// credential. It has a listener of its own so the gateway can send it
		// past the proxy, and so that this listener carries nothing else.
		{name: "upload", addr: c.String("upload-addr"), handler: uploads.Handler()},
		{name: "ops", addr: c.String("ops-addr"), handler: opsMux()},
	}

	return serve(ctx, log, listeners)
}

// checkHTTPConfig validates what the http transport must have before it starts
// and returns the public URL.
func checkHTTPConfig(c *cli.Command, log *slog.Logger) (string, error) {
	publicURL, err := checkPublicURL(c.String("public-url"))
	if err != nil {
		return "", err
	}

	if err := refuseLeftoverAuthSettings(os.LookupEnv); err != nil {
		return "", err
	}

	// Fail closed: the MCP endpoint has no authentication, so it may only be
	// bound where the sidecar alone can reach it.
	mcpAddr := c.String("addr")
	if c.Bool("allow-non-loopback") {
		log.Warn("MCP endpoint may bind a non-loopback address: it has NO authentication", "addr", mcpAddr)
	} else if err := requireLoopback(mcpAddr); err != nil {
		return "", err
	}

	return publicURL, nil
}

// Default listen addresses. The chart overrides them only where it must.
const (
	defaultMCPAddr    = "127.0.0.1:8081"
	defaultUploadAddr = "127.0.0.1:8082"
	defaultOpsAddr    = ":9090"
)

// checkPublicURL requires an absolute http(s) URL, and returns it as given.
func checkPublicURL(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("--public-url (or PUBLIC_URL) is required for the http transport: it is the base of upload links")
	}

	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", fmt.Errorf("--public-url %q must be an absolute http(s) URL", raw)
	}

	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("--public-url %q must have no query or fragment", raw)
	}

	return raw, nil
}

// refuseLeftoverAuthSettings stops a stale values file from leaving a false
// impression. Authentication moved to the sidecar; an ISSUER_URL or
// RESOURCE_URL still set on THIS container means someone believes this server
// checks tokens. It does not, and it will not pretend to.
func refuseLeftoverAuthSettings(lookup func(string) (string, bool)) error {
	for _, name := range []string{"ISSUER_URL", "RESOURCE_URL", "SCOPE"} {
		if v, ok := lookup(name); ok && v != "" {
			return fmt.Errorf("%s is set, but this server no longer authenticates anything: the resource-proxy sidecar does. "+
				"Move it to the sidecar's environment and, for the server's own external URL, set PUBLIC_URL", name)
		}
	}

	return nil
}
