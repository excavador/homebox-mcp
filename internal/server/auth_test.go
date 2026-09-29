package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/truvity/access-roster/identity"
)

func TestAuthMetadata(t *testing.T) {
	a, err := NewAuth("https://access.excavador.xyz", "https://mcp.excavador.xyz/homebox", "openid")
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}

	rec := httptest.NewRecorder()
	a.Metadata().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MetadataPath, nil))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if got := body["resource"]; got != "https://mcp.excavador.xyz/homebox" {
		t.Errorf("resource = %v", got)
	}

	servers, _ := body["authorization_servers"].([]any)
	if len(servers) != 1 || servers[0] != "https://access.excavador.xyz" {
		t.Errorf("authorization_servers = %v", body["authorization_servers"])
	}

	// access-roster refuses an authorize request that carries no scope at
	// all; the PRM must offer one a scope-less client can adopt.
	scopes, _ := body["scopes_supported"].([]any)
	if len(scopes) != 1 || scopes[0] != "openid" {
		t.Errorf("scopes_supported = %v, want [openid]", body["scopes_supported"])
	}
}

func TestAuthMetadataOmitsScopesWhenScopeEmpty(t *testing.T) {
	a, err := NewAuth("https://access.excavador.xyz", "https://mcp.excavador.xyz/homebox", "")
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}

	rec := httptest.NewRecorder()
	a.Metadata().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MetadataPath, nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if _, ok := body["scopes_supported"]; ok {
		t.Errorf("scopes_supported = %v, want the field absent for an empty scope", body["scopes_supported"])
	}
}

func TestAuthProtectRefusesUnverified(t *testing.T) {
	a, err := NewAuth("https://access.excavador.xyz", "https://mcp.excavador.xyz/homebox", "openid")
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}

	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

	rec := httptest.NewRecorder()
	a.Protect(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// RFC 9728 §3: the well-known segment is inserted before this
	// resource's own path, not appended after it. The challenge also
	// carries `scope`, next to `resource_metadata` -- access-roster
	// rejects an authorize request with no scope, and the MCP
	// authorization spec has a client prefer this over the PRM's
	// scopes_supported.
	want := `Bearer resource_metadata="https://mcp.excavador.xyz/.well-known/oauth-protected-resource/homebox", scope="openid"`
	if got := rec.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}

	if called {
		t.Error("next was called for an unverified request")
	}
}

func TestAuthProtectPassesVerifiedThrough(t *testing.T) {
	a, err := NewAuth("https://access.excavador.xyz", "https://mcp.excavador.xyz/homebox", "openid")
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req = req.WithContext(identity.WithVerified(req.Context(), identity.Verified{Subject: "oleg@tsarev.id"}))

	rec := httptest.NewRecorder()
	a.Protect(next).ServeHTTP(rec, req)

	if !called {
		t.Fatal("next was not called for a request already carrying a verified caller")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewAuthRejectsRelativeResourceURL(t *testing.T) {
	if _, err := NewAuth("https://access.excavador.xyz", "not-a-url", "openid"); err == nil {
		t.Fatal("expected an error for a resource url with no scheme or host")
	}
}
