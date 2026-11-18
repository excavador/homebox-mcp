package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// requireLoopback refuses a listen address that is not loopback.
//
// The MCP endpoint has no authentication: the sidecar in front of it is the
// only thing that checks a caller. A wrong values file that binds it to
// 0.0.0.0 would publish the inventory to everything that can reach the pod, so
// the check is on the address, at startup, and the default is to refuse.
//
// An empty host (":8081") and an unspecified one ("0.0.0.0", "[::]") listen
// on every interface and are refused. A name is accepted only when it is
// "localhost"; any other name could resolve anywhere.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--addr %q: %w", addr, err)
	}

	if host == "localhost" {
		return nil
	}

	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}

	return fmt.Errorf("--addr %q is not a loopback address: the MCP endpoint has no authentication of its own "+
		"(the resource-proxy sidecar in front of it checks every caller). Bind 127.0.0.1, or set "+
		"--allow-non-loopback (ALLOW_NON_LOOPBACK=true) if the network around it does the isolation", addr)
}

// mcpMux is the MCP listener's whole handler: /mcp and below, nothing else.
// There is no /healthz here (that is the ops listener's) and no metadata (the
// sidecar serves it).
func mcpMux(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	mux.Handle("/mcp/", h)

	return mux
}

// opsMux is the ops listener's whole handler: /healthz, for the kubelet.
//
// Liveness only. It deliberately does NOT call HomeBox: a probe that fails
// when a dependency is briefly unavailable restarts a process that would
// otherwise have recovered on its own. It is unauthenticated because a probe
// carries no bearer token, and it is on its own port so the Service never
// exposes it.
func opsMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	return mux
}

// listener is one HTTP listener with one job.
type listener struct {
	name    string
	addr    string
	handler http.Handler
}

// serve runs every listener until ctx is cancelled or one of them fails, then
// shuts the rest down. A listener that cannot bind is an error for the whole
// process: a pod with no upload listener, or no ops port, must not look healthy.
func serve(ctx context.Context, log *slog.Logger, listeners []listener) error {
	// Bind everything first, so a bad address fails before any traffic is
	// accepted anywhere.
	servers := make([]*http.Server, 0, len(listeners))
	bound := make([]net.Listener, 0, len(listeners))

	closeAll := func() {
		for _, l := range bound {
			_ = l.Close()
		}
	}

	for _, l := range listeners {
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			closeAll()

			return fmt.Errorf("%s listener: %w", l.name, err)
		}

		bound = append(bound, ln)
		servers = append(servers, &http.Server{
			Handler:           l.handler,
			ReadHeaderTimeout: 10 * time.Second,
		})

		log.Info("listening", "listener", l.name, "addr", ln.Addr().String())
	}

	errc := make(chan error, len(servers))

	for i, srv := range servers {
		go func() {
			if err := srv.Serve(bound[i]); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s listener: %w", listeners[i].name, err)

				return
			}

			errc <- nil
		}()
	}

	var first error

	select {
	case <-ctx.Done():
	case first = <-errc:
	}

	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	for _, srv := range servers {
		_ = srv.Shutdown(shutdown)
	}

	return first
}
