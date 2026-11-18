package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequireLoopback(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8081", "127.0.0.2:8081", "[::1]:8081", "localhost:8081"} {
		if err := requireLoopback(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}

	// Every one of these would publish the endpoint beyond the pod's own
	// loopback, or is not an address at all.
	for _, bad := range []string{
		"0.0.0.0:8081", ":8081", "[::]:8081", "10.1.2.3:8081", "192.168.1.5:8081", "[2001:db8::1]:8081",
		"example.com:8081", "homebox-mcp:8081", "127.0.0.1", "", "8081",
	} {
		if err := requireLoopback(bad); err == nil {
			t.Errorf("%q accepted: a non-loopback MCP address must be refused", bad)
		}
	}
}

func TestLoopbackRefusalSaysHowToOverride(t *testing.T) {
	err := requireLoopback("0.0.0.0:8080")
	if err == nil || !strings.Contains(err.Error(), "--allow-non-loopback") {
		t.Errorf("error = %v; it must name the explicit override", err)
	}
}

func status(h http.Handler, method, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))

	return rec.Code
}

// The MCP listener carries /mcp and below. /healthz is the ops listener's, and
// the protected-resource metadata is the sidecar's: neither may answer here.
func TestMCPListenerServesOnlyMCP(t *testing.T) {
	hit := 0
	h := mcpMux(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit++ }))

	for _, p := range []string{"/mcp", "/mcp/", "/mcp/x"} {
		if status(h, http.MethodPost, p) != http.StatusOK {
			t.Errorf("%s did not reach the MCP handler", p)
		}
	}

	if hit != 3 {
		t.Errorf("MCP handler hit %d times, want 3", hit)
	}

	for _, p := range []string{"/", "/healthz", "/.well-known/oauth-protected-resource", "/mcpx", "/upload/x", "/other"} {
		if got := status(h, http.MethodGet, p); got != http.StatusNotFound {
			t.Errorf("%s = %d on the MCP listener, want 404", p, got)
		}
	}

	if hit != 3 {
		t.Errorf("MCP handler hit by a non-MCP path (%d hits)", hit)
	}
}

func TestOpsListenerServesOnlyHealthz(t *testing.T) {
	h := opsMux()

	if got := status(h, http.MethodGet, "/healthz"); got != http.StatusOK {
		t.Errorf("/healthz = %d", got)
	}

	for _, p := range []string{"/", "/mcp", "/mcp/", "/readyz", "/metrics", "/upload/x", "/.well-known/oauth-protected-resource"} {
		if got := status(h, http.MethodGet, p); got != http.StatusNotFound {
			t.Errorf("%s = %d on the ops listener, want 404", p, got)
		}
	}
}

func TestCheckPublicURL(t *testing.T) {
	for _, ok := range []string{"https://mcp.example/homebox", "https://mcp.example", "http://localhost:8082/x"} {
		if _, err := checkPublicURL(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}

	for _, bad := range []string{"", "mcp.example/homebox", "ftp://mcp.example/x", "https://", "https://mcp.example/x?y=1", "https://mcp.example/x#f"} {
		if _, err := checkPublicURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A values file left over from before the sidecar must not leave anyone
// believing this server checks tokens.
func TestLeftoverAuthSettingsAreRefused(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}

	if err := refuseLeftoverAuthSettings(env(nil)); err != nil {
		t.Errorf("clean environment refused: %v", err)
	}

	if err := refuseLeftoverAuthSettings(env(map[string]string{"ISSUER_URL": ""})); err != nil {
		t.Errorf("empty value refused: %v", err)
	}

	for _, name := range []string{"ISSUER_URL", "RESOURCE_URL", "SCOPE"} {
		err := refuseLeftoverAuthSettings(env(map[string]string{name: "x"}))
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "PUBLIC_URL") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

// serve binds every listener before it accepts anything, and a listener that
// cannot bind fails the whole process.
func TestServeFailsWhenAListenerCannotBind(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	err = serve(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), []listener{
		{name: "mcp", addr: "127.0.0.1:0", handler: http.NotFoundHandler()},
		{name: "ops", addr: taken.Addr().String(), handler: http.NotFoundHandler()},
	})
	if err == nil || !strings.Contains(err.Error(), "ops listener") {
		t.Errorf("err = %v, want an ops listener bind failure", err)
	}
}

func TestServeStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- serve(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), []listener{
			{name: "mcp", addr: "127.0.0.1:0", handler: http.NotFoundHandler()},
			{name: "upload", addr: "127.0.0.1:0", handler: http.NotFoundHandler()},
			{name: "ops", addr: "127.0.0.1:0", handler: opsMux()},
		})
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop on context cancel")
	}
}
