package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/homebox-mcp/internal/fetch"
	"github.com/excavador/homebox-mcp/internal/homebox"
)

// One-time upload links.
//
// The server runs in a cluster; the file is on the caller's machine. Passing
// the bytes through the model (data_base64) costs a token per few characters
// and risks a corrupted copy, and a URL only works for something already
// public. So the tool hands out a link instead, and the caller sends the file
// to it with one `curl -X PUT --data-binary @file`. The bytes never pass
// through the model.
//
// The link IS the credential -- a curl in a shell cannot present the MCP
// bearer token -- so it is built to be worth little if it leaks: signed (the
// server holds no list of links), bound to one entity, one attachment type,
// one name and one size cap, valid for minutes, and usable once.

const (
	defaultTTL = 10 * time.Minute

	// DefaultMaxBytes is HomeBox's own default upload limit. Raising ours
	// above HomeBox's (HBOX_WEB_MAX_UPLOAD_SIZE, in MiB) only moves the
	// refusal to a worse place.
	DefaultMaxBytes = 10 << 20

	claimsVersion = 1
)

// UploadConfig configures an Uploader. Zero values for MaxBytes, TTL and Now
// take their defaults.
type UploadConfig struct {
	// BaseURL is this server's own external URL, as a client reaches it. A
	// link is BaseURL + "/upload/" + token.
	BaseURL string
	// Key signs the links. Derive it from a secret the process already has;
	// changing it invalidates every outstanding link, which is harmless.
	Key      []byte
	MaxBytes int64
	TTL      time.Duration
	Now      func() time.Time
}

// Uploader mints upload links and serves them.
type Uploader struct {
	c    *homebox.Client
	base string
	key  []byte
	max  int64
	ttl  time.Duration
	now  func() time.Time

	mu       sync.Mutex
	inflight map[string]time.Time // nonce -> expiry; present while used or being used
}

// NewUploader validates cfg and returns an Uploader.
func NewUploader(c *homebox.Client, cfg UploadConfig) (*Uploader, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		return nil, errors.New("upload: BaseURL is required")
	}

	if len(cfg.Key) < 16 {
		return nil, errors.New("upload: Key must be at least 16 bytes")
	}

	u := &Uploader{
		c: c, base: base, key: cfg.Key, max: cfg.MaxBytes, ttl: cfg.TTL, now: cfg.Now,
		inflight: map[string]time.Time{},
	}

	if u.max <= 0 {
		u.max = DefaultMaxBytes
	}

	if u.ttl <= 0 {
		u.ttl = defaultTTL
	}

	if u.now == nil {
		u.now = time.Now
	}

	return u, nil
}

// claims is everything a link authorises. Short keys keep the URL short.
type claims struct {
	V       int    `json:"v"`
	Entity  string `json:"e"`
	Kind    string `json:"t"`
	Primary bool   `json:"p,omitempty"`
	Name    string `json:"n,omitempty"`
	Exp     int64  `json:"x"`
	Max     int64  `json:"m"`
	Nonce   string `json:"j"`
}

var b64 = base64.RawURLEncoding

func (u *Uploader) sign(payload []byte) []byte {
	m := hmac.New(sha256.New, u.key)
	m.Write(payload)

	return m.Sum(nil)
}

func (u *Uploader) mint(cl claims) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}

	cl.V = claimsVersion
	cl.Nonce = b64.EncodeToString(nonce[:])

	payload, err := json.Marshal(cl)
	if err != nil {
		return "", err
	}

	return b64.EncodeToString(payload) + "." + b64.EncodeToString(u.sign(payload)), nil
}

// errLink is deliberately one message: a caller learns nothing about WHY a
// forged or stale link failed.
var errLink = errors.New("invalid or expired upload link")

func (u *Uploader) verify(token string) (claims, error) {
	var cl claims

	p, s, ok := strings.Cut(token, ".")
	if !ok {
		return cl, errLink
	}

	payload, err := b64.DecodeString(p)
	if err != nil {
		return cl, errLink
	}

	sig, err := b64.DecodeString(s)
	if err != nil || !hmac.Equal(sig, u.sign(payload)) {
		return cl, errLink
	}

	if err := json.Unmarshal(payload, &cl); err != nil || cl.V != claimsVersion ||
		!attachmentTypes[cl.Kind] || cl.Entity == "" || cl.Max <= 0 || cl.Nonce == "" {
		return cl, errLink
	}

	if u.now().Unix() >= cl.Exp {
		return cl, errLink
	}

	return cl, nil
}

// reserve claims the link's nonce so two parallel PUTs cannot both succeed.
// It is released again if the upload fails, so a refused file (wrong type,
// too big) does not burn the link; only a stored file does.
func (u *Uploader) reserve(cl claims) bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	now := u.now()
	for n, exp := range u.inflight {
		if !now.Before(exp) {
			delete(u.inflight, n)
		}
	}

	if _, used := u.inflight[cl.Nonce]; used {
		return false
	}

	u.inflight[cl.Nonce] = time.Unix(cl.Exp, 0)

	return true
}

func (u *Uploader) release(cl claims) {
	u.mu.Lock()
	delete(u.inflight, cl.Nonce)
	u.mu.Unlock()
}

type prepareArgs struct {
	ID      string `json:"id" jsonschema:"the entity UUID"`
	Type    string `json:"type,omitempty" jsonschema:"photo (default), attachment, manual, receipt or warranty"`
	Primary bool   `json:"primary,omitempty" jsonschema:"make the uploaded photo the entity's primary photo"`
	Name    string `json:"name,omitempty" jsonschema:"file name to store; or add ?name= to the upload URL"`
}

func (u *Uploader) addTool(s *mcp.Server) {
	mib := u.max >> 20

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_prepare_attachment_upload",
		Description: "Get a one-time URL for uploading a file that is on THIS machine -- a photo, or a PDF manual, " +
			"receipt or warranty -- without passing its bytes through the model. Then run the returned curl with the " +
			"file's real path: the body is the raw file, the answer is the new attachment. " +
			fmt.Sprintf("The link is valid %d minutes, works once for this entity and type, takes up to %d MiB, ", int(u.ttl.Minutes()), mib) +
			"and the content must be a jpeg, png, webp or gif (a PDF for anything but a photo). " +
			"Use homebox_create_attachment instead for a file that is already at a public https URL.",
		Annotations: creates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a prepareArgs) (*mcp.CallToolResult, any, error) {
		if a.ID == "" {
			return nil, nil, errors.New("id is required")
		}

		kind := a.Type
		if kind == "" {
			kind = "photo"
		}

		if !attachmentTypes[kind] {
			return nil, nil, fmt.Errorf("type %q is not one of photo, attachment, manual, receipt, warranty", kind)
		}

		// Fail now, with HomeBox's own message, rather than after the file
		// has been sent to a link for an entity that does not exist.
		if _, err := u.c.Call(ctx, http.MethodGet, "/entities/"+url.PathEscape(a.ID), nil, nil); err != nil {
			return nil, nil, err
		}

		exp := u.now().Add(u.ttl)

		token, err := u.mint(claims{Entity: a.ID, Kind: kind, Primary: a.Primary, Name: a.Name, Exp: exp.Unix(), Max: u.max})
		if err != nil {
			return nil, nil, err
		}

		link := u.base + "/upload/" + token

		return nil, map[string]any{
			"upload_url": link,
			"method":     "PUT",
			"curl":       "curl -sS -X PUT --data-binary @/path/to/file '" + link + "'",
			"type":       kind,
			"max_bytes":  u.max,
			"expires_at": exp.UTC().Format(time.RFC3339),
		}, nil
	})
}

// Mount registers the upload endpoint on mux under prefix, which is the path
// the server itself is served at ("/mcp"), not the external one: a gateway
// that rewrites /homebox to /mcp sends the link here unchanged.
//
// The pattern is more specific than the "/mcp/" route that sits behind the
// bearer-token check, so Go's mux sends a PUT here and everything else there.
func (u *Uploader) Mount(mux *http.ServeMux, prefix string) {
	mux.Handle("PUT "+strings.TrimRight(prefix, "/")+"/upload/{token}", http.HandlerFunc(u.handle))
}

func (u *Uploader) handle(w http.ResponseWriter, r *http.Request) {
	cl, err := u.verify(r.PathValue("token"))
	if err != nil {
		reply(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}

	if !u.reserve(cl) {
		reply(w, http.StatusConflict, map[string]any{"error": "this upload link was already used"})
		return
	}

	stored := false

	defer func() {
		if !stored {
			u.release(cl)
		}
	}()

	if r.ContentLength > cl.Max {
		reply(w, http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("larger than the %d byte limit", cl.Max)})
		return
	}

	// Bound how many uploads run at once; each holds the file twice.
	select {
	case uploadSlots <- struct{}{}:
		defer func() { <-uploadSlots }()
	case <-r.Context().Done():
		reply(w, http.StatusServiceUnavailable, map[string]any{"error": "cancelled while waiting for a free upload slot"})
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cl.Max))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			reply(w, http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("larger than the %d byte limit", cl.Max)})
		} else {
			reply(w, http.StatusBadRequest, map[string]any{"error": "could not read the body"})
		}

		return
	}

	file, err := fetch.Inspect(data, cl.Kind)
	if err != nil {
		reply(w, http.StatusUnsupportedMediaType, map[string]any{"error": err.Error()})
		return
	}

	name := cl.Name
	if name == "" {
		name = r.URL.Query().Get("name")
	}

	out, err := storeAttachment(context.WithoutCancel(r.Context()), u.c, cl.Entity, cl.Kind, cl.Primary, fileName(name, file), file)
	if err != nil {
		reply(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	stored = true

	reply(w, http.StatusCreated, out)
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
