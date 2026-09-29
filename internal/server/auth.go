package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/truvity/access-roster/identity"
)

// MetadataPath is where this server answers its own OAuth 2.0 Protected
// Resource Metadata (RFC 9728), INTERNALLY. A gateway fronting this server
// on a shared hostname is expected to rewrite whichever external
// well-known form a client used to this fixed path -- the same way it
// already rewrites the MCP endpoint itself.
const MetadataPath = "/.well-known/oauth-protected-resource"

// Auth is this server's resource-server half of the Model Context
// Protocol's authorization spec: it verifies a bearer token against
// access-roster (issuer + audience) and publishes the RFC 9728 document a
// client needs to discover that issuer.
//
// access-roster is the authorization server named inside that document,
// not the party that serves it -- publishing it is this server's own
// responsibility (access-roster docs/connect/mcp.md, "Publish RFC 9728").
type Auth struct {
	issuer          *identity.Issuer
	resource        string
	scope           string
	metadataURL     string
	metadataDoc     []byte
	challengeHeader string
}

// NewAuth builds the verifier for one resource.
//
// issuerURL is access-roster's own URL, exactly as it appears in a
// token's `iss`. resourceURL is this server's own externally-reachable
// URL: the RFC 8707 resource indicator a client names, and the `aud`
// access-roster mints for it -- it MUST match, byte for byte, whatever
// this installation's access-roster policy declares under `resources`.
//
// scope is advertised in both the PRM's `scopes_supported` and the 401
// challenge's `scope`, but never checked here -- access-roster's own
// `resources.<uri>.requires` is what actually decides who may reach this
// resource (see Protect). It exists only because access-roster refuses
// an authorize request that carries no scope at all ("The scope of your
// request is missing"): per the MCP authorization spec's scope-selection
// order, a client uses `WWW-Authenticate`'s `scope` first, then the
// PRM's `scopes_supported`, and otherwise sends none -- so a client with
// no scope of its own would otherwise be unable to reach an issuer that
// requires one. An empty scope omits both fields rather than advertising
// an empty list, which is not a valid alternative to omitting the field.
func NewAuth(issuerURL, resourceURL, scope string) (*Auth, error) {
	u, err := url.Parse(resourceURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("resource url %q is not an absolute URL", resourceURL)
	}

	// RFC 9728 §3: a resource with a path serves its metadata with that
	// path appended after the well-known segment, not replacing it --
	// this is what lets several resources share one hostname, which is
	// exactly what mcp.excavador.xyz does for /homebox and /netbox.
	metadataURL := u.Scheme + "://" + u.Host + MetadataPath + u.Path

	fields := map[string]any{
		"resource":                 resourceURL,
		"authorization_servers":    []string{issuerURL},
		"bearer_methods_supported": []string{"header"},
	}

	challenge := `Bearer resource_metadata="` + metadataURL + `"`
	if scope != "" {
		fields["scopes_supported"] = []string{scope}
		challenge += `, scope="` + scope + `"`
	}

	doc, err := json.Marshal(fields)
	if err != nil {
		return nil, err // unreachable: the map above always marshals
	}

	return &Auth{
		issuer:          &identity.Issuer{URL: issuerURL, Audience: resourceURL},
		resource:        resourceURL,
		scope:           scope,
		metadataURL:     metadataURL,
		metadataDoc:     doc,
		challengeHeader: challenge,
	}, nil
}

// Protect refuses a request identity.Middleware did not establish a
// caller for, naming this server's own protected-resource metadata in
// WWW-Authenticate (RFC 9728 §5.1) so a compliant client can discover
// access-roster without being told out of band. Middleware and the
// refusal are combined here -- unlike a console, an MCP endpoint has no
// unauthenticated route that would need excluding from it.
//
// No group is required beyond "access-roster vouches for this caller at
// all": the resource's own `requires` in access-roster's policy already
// decided who may reach this audience, and reading that decision a
// second time here would be a second vocabulary for the same thing.
func (a *Auth) Protect(next http.Handler) http.Handler {
	refuse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := identity.FromContext(r.Context()); !ok {
			w.Header().Set("WWW-Authenticate", a.challengeHeader)
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
	return identity.Middleware(a.issuer)(refuse)
}

// Metadata serves this server's RFC 9728 Protected Resource Metadata: its
// own resource URI and the issuer that mints tokens for it. Deliberately
// unauthenticated -- a discovery document that needs a token to read is
// useless to a client that does not have one yet.
func (a *Auth) Metadata() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(a.metadataDoc)
	})
}
