package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/excavador/homebox-mcp/internal/homebox"
)

var testPDF = []byte("%PDF-1.7\n%test manual\n")

// stubHomebox answers the entity read and the multipart upload the way
// HomeBox does, and records what reached it.
type stubHomebox struct {
	mu    sync.Mutex
	posts int
	form  map[string]string
	file  []byte
	ct    string
	name  string
}

func (s *stubHomebox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodGet {
		if !strings.HasSuffix(r.URL.Path, "/entities/e1") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))

			return
		}

		_, _ = w.Write([]byte(`{"id":"e1","attachments":[{"id":"old","type":"photo","title":"old.png"}]}`))

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.posts++

	_ = r.ParseMultipartForm(1 << 20)

	s.form = map[string]string{}
	for k, v := range r.MultipartForm.Value {
		s.form[k] = v[0]
	}

	fh := r.MultipartForm.File["file"][0]
	s.name, s.ct = fh.Filename, fh.Header.Get("Content-Type")
	f, _ := fh.Open()
	s.file, _ = io.ReadAll(f)

	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"id":"e1","attachments":[{"id":"old","type":"photo","title":"old.png"},` +
		`{"id":"a1","type":"` + s.form["type"] + `","primary":` + s.form["primary"] + `,"title":"` + s.name + `"}]}`))
}

type fixture struct {
	hb    *stubHomebox
	up    *Uploader
	mux   *http.ServeMux
	clock *time.Time
}

func newFixture(t *testing.T, max int64) *fixture {
	t.Helper()

	hb := &stubHomebox{}
	srv := httptest.NewServer(hb)
	t.Cleanup(srv.Close)

	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	f := &fixture{hb: hb, clock: &now}

	up, err := NewUploader(homebox.New(srv.URL, "t"), UploadConfig{
		BaseURL: "https://mcp.example/homebox/", Key: []byte("0123456789abcdef0123456789abcdef"),
		MaxBytes: max, Now: func() time.Time { return *f.clock },
	})
	if err != nil {
		t.Fatalf("uploader: %v", err)
	}

	f.up = up
	f.mux = http.NewServeMux()
	up.Mount(f.mux, "")

	return f
}

func (f *fixture) link(t *testing.T, kind string, primary bool) string {
	t.Helper()

	tok, err := f.up.mint(claims{Entity: "e1", Kind: kind, Primary: primary, Exp: f.clock.Add(10 * time.Minute).Unix(), Max: f.up.max})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	return tok
}

func (f *fixture) put(path string, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body)))

	return rec
}

func TestUploadLinkStoresTheFile(t *testing.T) {
	f := newFixture(t, 0)

	rec := f.put("/upload/"+f.link(t, "photo", true)+"?name=../front.png", testPNG)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}

	if f.hb.form["type"] != "photo" || f.hb.form["primary"] != "true" || f.hb.form["name"] != "front.png" {
		t.Errorf("form = %v, want the link's type and primary, and a sanitised name", f.hb.form)
	}

	if !bytes.Equal(f.hb.file, testPNG) || f.hb.name != "front.png" || f.hb.ct != "image/png" {
		t.Errorf("file part = %q %q, %d bytes", f.hb.name, f.hb.ct, len(f.hb.file))
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["id"] != "a1" {
		t.Errorf("answer = %s, want the new attachment, not the entity", rec.Body)
	}
}

// A manual is a PDF; the same bytes must not pass as a photo.
func TestUploadLinkIsBoundToItsType(t *testing.T) {
	f := newFixture(t, 0)

	if rec := f.put("/upload/"+f.link(t, "photo", false), testPDF); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("pdf to a photo link: status = %d, want 415", rec.Code)
	}

	if rec := f.put("/upload/"+f.link(t, "manual", false), testPDF); rec.Code != http.StatusCreated || f.hb.form["type"] != "manual" {
		t.Errorf("pdf to a manual link: status = %d, form = %v", rec.Code, f.hb.form)
	}
}

func TestUploadLinkWorksOnce(t *testing.T) {
	f := newFixture(t, 0)
	tok := f.link(t, "photo", false)

	if rec := f.put("/upload/"+tok, testPNG); rec.Code != http.StatusCreated {
		t.Fatalf("first use: %d", rec.Code)
	}

	if rec := f.put("/upload/"+tok, testPNG); rec.Code != http.StatusConflict {
		t.Errorf("second use: status = %d, want 409", rec.Code)
	}

	if f.hb.posts != 1 {
		t.Errorf("HomeBox saw %d uploads, want 1", f.hb.posts)
	}
}

// A refused file must not burn the link: the caller sent the wrong file, and
// should be able to send the right one to the same link.
func TestRefusedFileDoesNotBurnTheLink(t *testing.T) {
	f := newFixture(t, 0)
	tok := f.link(t, "photo", false)

	if rec := f.put("/upload/"+tok, []byte("<html>not an image</html>")); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("html: status = %d, want 415", rec.Code)
	}

	if f.hb.posts != 0 {
		t.Fatalf("a refused file reached HomeBox")
	}

	if rec := f.put("/upload/"+tok, testPNG); rec.Code != http.StatusCreated {
		t.Errorf("retry with a real image: status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestUploadLinkRefusals(t *testing.T) {
	f := newFixture(t, 16) // smaller than testPNG
	good := f.link(t, "photo", false)

	other, _ := NewUploader(f.up.c, UploadConfig{BaseURL: "https://x", Key: []byte("fedcba9876543210fedcba9876543210")})
	forged, _ := other.mint(claims{Entity: "e1", Kind: "photo", Exp: f.clock.Add(time.Minute).Unix(), Max: 1 << 20})

	expired, _ := f.up.mint(claims{Entity: "e1", Kind: "photo", Exp: f.clock.Add(-time.Second).Unix(), Max: 1 << 20})

	// Same payload, signature of another link: edited claims must not verify.
	payload, sig, _ := strings.Cut(good, ".")
	edited := strings.Replace(payload, payload[:6], "AAAAAA", 1) + "." + sig

	for name, tc := range map[string]struct {
		path string
		want int
	}{
		"signed with another key": {"/upload/" + forged, http.StatusUnauthorized},
		"expired":                 {"/upload/" + expired, http.StatusUnauthorized},
		"edited payload":          {"/upload/" + edited, http.StatusUnauthorized},
		"no signature":            {"/upload/" + payload, http.StatusUnauthorized},
		"garbage":                 {"/upload/not-a-link", http.StatusUnauthorized},
		"over the size limit":     {"/upload/" + good, http.StatusRequestEntityTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := f.put(tc.path, testPNG); rec.Code != tc.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}

	if f.hb.posts != 0 {
		t.Errorf("HomeBox saw %d uploads; a refused link must never reach it", f.hb.posts)
	}
}

// Only PUT is served here; anything else falls through to the route behind
// the bearer-token check in the real server.
func TestUploadEndpointIsPutOnly(t *testing.T) {
	f := newFixture(t, 0)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/upload/"+f.link(t, "photo", false), nil))

	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		t.Errorf("GET answered %d", rec.Code)
	}
}

// main.go registers the upload route next to the catch-all "/mcp/" that sits
// behind the bearer-token check. A pattern conflict panics at startup, not at
// build time, and a route that loses to the catch-all would demand a token a
// curl cannot present -- so build the same mux and send requests through it.
func TestUploadRouteBeatsTheProtectedCatchAll(t *testing.T) {
	f := newFixture(t, 0)

	protected := 0
	mux := http.NewServeMux()
	f.up.Mount(mux, "/mcp")
	mux.HandleFunc("/mcp", func(http.ResponseWriter, *http.Request) { protected++ })
	mux.HandleFunc("/mcp/", func(http.ResponseWriter, *http.Request) { protected++ })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/mcp/upload/"+f.link(t, "photo", false), bytes.NewReader(testPNG)))

	if rec.Code != http.StatusCreated || protected != 0 {
		t.Errorf("PUT: status = %d, protected route hit %d times; the link must not need a bearer token", rec.Code, protected)
	}

	for _, p := range []struct{ method, path string }{
		{http.MethodPost, "/mcp"}, {http.MethodPost, "/mcp/"}, {http.MethodGet, "/mcp/upload/x"}, {http.MethodPost, "/mcp/upload/x"},
	} {
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(p.method, p.path, nil))
	}

	if protected != 4 {
		t.Errorf("protected route hit %d times, want 4: everything but a PUT to an upload link goes behind the token check", protected)
	}
}

func TestPrepareAttachmentUploadMintsALink(t *testing.T) {
	hb := &stubHomebox{}
	srv := httptest.NewServer(hb)
	t.Cleanup(srv.Close)

	c := homebox.New(srv.URL, "t")

	up, err := NewUploader(c, UploadConfig{BaseURL: "https://mcp.example/homebox", Key: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}

	ct, st := mcp.NewInMemoryTransports()

	ss, err := New(c, "test", WithUploads(up)).Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "homebox_prepare_attachment_upload", Arguments: map[string]any{"id": "e1", "type": "manual", "name": "rd6006.pdf"},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}

	out, _ := res.StructuredContent.(map[string]any)

	link, _ := out["upload_url"].(string)
	if !strings.HasPrefix(link, "https://mcp.example/homebox/upload/") || out["method"] != "PUT" ||
		!strings.Contains(out["curl"].(string), link) {
		t.Fatalf("result = %v", out)
	}

	// The minted link must be accepted by the handler, carrying what was asked.
	mux := http.NewServeMux()
	up.Mount(mux, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, strings.TrimPrefix(link, "https://mcp.example/homebox"), bytes.NewReader(testPDF)))

	if rec.Code != http.StatusCreated || hb.form["type"] != "manual" || hb.form["name"] != "rd6006.pdf" {
		t.Errorf("status = %d, form = %v", rec.Code, hb.form)
	}

	// An id HomeBox does not know must fail now, not after the file is sent.
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "homebox_prepare_attachment_upload", Arguments: map[string]any{"id": "nope"},
	}); err == nil && !res.IsError {
		t.Error("an unknown entity must be refused when the link is requested")
	}

	// And it must carry an annotation, like every other tool.
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, tl := range tools.Tools {
		if tl.Name == "homebox_prepare_attachment_upload" && (tl.Annotations == nil || tl.Annotations.ReadOnlyHint) {
			t.Errorf("annotations = %+v, want a non-destructive create", tl.Annotations)
		}
	}
}

func TestNewUploaderRejectsWeakConfig(t *testing.T) {
	c := homebox.New("http://x", "t")

	if _, err := NewUploader(c, UploadConfig{Key: []byte("0123456789abcdef")}); err == nil {
		t.Error("no BaseURL must be refused")
	}

	if _, err := NewUploader(c, UploadConfig{BaseURL: "https://x", Key: []byte("short")}); err == nil {
		t.Error("a short key must be refused")
	}
}
