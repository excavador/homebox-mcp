// Package homebox is a minimal client for the HomeBox API.
//
// Deliberately not generated from the OpenAPI spec: this server exposes a
// handful of GETs, and a generated client would be several thousand lines
// of surface we do not use and would have to keep in step with upstream.
package homebox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// maxTextBytes caps a non-JSON response. The reports have no pagination,
// so an inventory of any size would otherwise arrive whole.
const maxTextBytes = 256 << 10

// Client talks to one HomeBox instance as one API key.
//
// HomeBox scopes data by GROUP, not by user: every item belongs to the group
// the key's owner belongs to. Two people in the same group see the same
// inventory, which for a household is the point rather than a limitation.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client for baseURL (e.g. "http://homebox").
//
// The API lives under /api — swagger reports basePath "/api" and the paths
// themselves carry HomeBox's own /v1 version segment, so a full URL looks
// like http://homebox/api/v1/entities.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// do performs one request against /api/v1 and decodes the JSON body, if any.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	u := c.baseURL + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var rdr io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode body: %w", err)
		}

		rdr = bytes.NewReader(b)
	}

	ct := ""
	if body != nil {
		ct = "application/json"
	}

	return c.send(ctx, method, path, u, rdr, ct, out)
}

// send performs a prepared request and decodes the JSON answer, if any.
func (c *Client) send(ctx context.Context, method, path, u string, rdr io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	// HomeBox's only auth scheme: its own bearer token. It does NOT accept
	// an OIDC token -- the OIDC endpoints are a browser redirect flow that
	// ends by issuing one of these.
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Cap the body: an HTML error page from a misconfigured proxy is
		// otherwise pasted whole into an LLM's context.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}

	// 204, and some deletes, answer with no body at all.
	if resp.StatusCode == http.StatusNoContent || out == nil {
		return nil
	}

	dec := json.NewDecoder(resp.Body)

	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // success with an empty body
		}

		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}

	return nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

// Text fetches an endpoint that answers with something other than JSON.
// HomeBox serves its reports as text/csv, which the JSON decoder rejects with
// a parse error that reads as a broken server rather than a wrong expectation.
func (c *Client) Text(ctx context.Context, path string, query url.Values) (string, error) {
	u := c.baseURL + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "*/*")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	// Reports grow with the inventory, so cap what can reach an LLM's context.
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxTextBytes))
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("GET %s: %s: %s", path, resp.Status,
			strings.TrimSpace(string(b[:min(len(b), 512)])))
	}

	return string(b), nil
}

// Call performs any method and returns the decoded JSON as-is.
//
// Bodies are passed through unmodelled, for the same reason responses are:
// HomeBox's own shapes are what its docs describe, and every struct defined
// here would be one more thing to drift from upstream.
func (c *Client) Call(ctx context.Context, method, path string, query url.Values, body any) (any, error) {
	var out any
	if err := c.do(ctx, method, path, query, body, &out); err != nil {
		return nil, err
	}

	if out == nil {
		// A successful write with no body still needs to say so.
		return map[string]any{"ok": true}, nil
	}

	return out, nil
}

// Raw fetches a path and returns the decoded JSON as-is.
//
// The tools hand HomeBox's own shapes straight through rather than
// remodelling them. A model reads the field names fine, and every struct we
// defined here would be one more thing to drift from upstream.
func (c *Client) Raw(ctx context.Context, path string, query url.Values) (any, error) {
	var out any
	if err := c.get(ctx, path, query, &out); err != nil {
		return nil, err
	}

	return out, nil
}

// Self returns the authenticated user, used as the startup health check:
// it proves the token works and reports which group it can see.
func (c *Client) Self(ctx context.Context) (name, email, group string, err error) {
	var out struct {
		Item struct {
			Name           string `json:"name"`
			Email          string `json:"email"`
			DefaultGroupID string `json:"defaultGroupId"`
		} `json:"item"`
	}
	if err := c.get(ctx, "/users/self", nil, &out); err != nil {
		return "", "", "", err
	}

	return out.Item.Name, out.Item.Email, out.Item.DefaultGroupID, nil
}

// Upload POSTs a multipart form to path: the given text fields, then the file
// as the "file" part with its own content type. HomeBox's attachment endpoint is multipart, so Call's
// JSON body cannot reach it.
func (c *Client) Upload(ctx context.Context, path string, fields map[string]string, filename, contentType string, data []byte) (any, error) {
	var buf bytes.Buffer

	mw := multipart.NewWriter(&buf)

	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return nil, fmt.Errorf("encode form: %w", err)
		}
	}

	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	h.Set("Content-Type", contentType)

	fw, err := mw.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("encode form: %w", err)
	}

	if _, err := fw.Write(data); err != nil {
		return nil, fmt.Errorf("encode form: %w", err)
	}

	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("encode form: %w", err)
	}

	var out any
	if err := c.send(ctx, http.MethodPost, path, c.baseURL+"/api/v1"+path, &buf, mw.FormDataContentType(), &out); err != nil {
		return nil, err
	}

	return out, nil
}
