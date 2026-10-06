package fetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

var (
	png  = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	jpeg = append([]byte("\xff\xd8\xff\xe0"), make([]byte, 64)...)
)

// public is an address no guard refuses; the test dialer ignores it and
// connects to the httptest server instead.
const public = "93.184.216.34"

// newFetcher returns a Fetcher that resolves every name to ips and dials srv,
// recording the address it was asked to dial.
func newFetcher(t *testing.T, srv *httptest.Server, ips ...string) (*Fetcher, *[]string) {
	t.Helper()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	var dialed []string

	return &Fetcher{
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, s := range ips {
				out = append(out, netip.MustParseAddr(s))
			}

			return out, nil
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLS: &tls.Config{RootCAs: pool, ServerName: "example.com"},
	}, &dialed
}

func serve(h http.HandlerFunc) *httptest.Server {
	return httptest.NewTLSServer(h)
}

func body(ct string, b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ct)
		_, _ = w.Write(b)
	}
}

func TestRefusedAddresses(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "100.127.255.255", "0.0.0.0", "224.0.0.1", "255.255.255.255",
		"::1", "::", "fc00::1", "fd12:3456::1", "fe80::1", "ff02::1",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "64:ff9b::a00:1",
	} {
		if !Refused(netip.MustParseAddr(s)) {
			t.Errorf("%s must be refused", s)
		}
	}

	for _, s := range []string{"93.184.216.34", "8.8.8.8", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if Refused(netip.MustParseAddr(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
}

func TestGetRefusesBeforeConnecting(t *testing.T) {
	srv := serve(body("image/png", png))
	defer srv.Close()

	cases := map[string]struct {
		url  string
		ips  []string
		want string
	}{
		"http scheme":     {"http://example.com/a.png", []string{public}, "only https"},
		"credentials":     {"https://u:p@example.com/a.png", []string{public}, "credentials"},
		"loopback v4":     {"https://example.com/a.png", []string{"127.0.0.1"}, "not a public address"},
		"private v4":      {"https://example.com/a.png", []string{"10.0.0.5"}, "not a public address"},
		"link-local meta": {"https://example.com/a.png", []string{"169.254.169.254"}, "not a public address"},
		"cgnat":           {"https://example.com/a.png", []string{"100.64.1.1"}, "not a public address"},
		"loopback v6":     {"https://example.com/a.png", []string{"::1"}, "not a public address"},
		"ula v6":          {"https://example.com/a.png", []string{"fd00::1"}, "not a public address"},
		"mapped private":  {"https://example.com/a.png", []string{"::ffff:192.168.0.1"}, "not a public address"},
		"unspecified":     {"https://example.com/a.png", []string{"0.0.0.0"}, "not a public address"},
		"one bad of many": {"https://example.com/a.png", []string{public, "10.0.0.1"}, "not a public address"},
		"ip literal":      {"https://127.0.0.1/a.png", nil, "not a public address"},
		"ip literal v6":   {"https://[::1]/a.png", nil, "not a public address"},
		"no addresses":    {"https://example.com/a.png", []string{}, "no addresses"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, dialed := newFetcher(t, srv, tc.ips...)

			_, err := f.Get(context.Background(), tc.url, "photo")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}

			if len(*dialed) != 0 {
				t.Errorf("dialled %v, want no connection at all", *dialed)
			}
		})
	}
}

func TestGetDialsTheCheckedAddress(t *testing.T) {
	srv := serve(body("image/png", png))
	defer srv.Close()

	f, dialed := newFetcher(t, srv, public)

	got, err := f.Get(context.Background(), "https://example.com/dir/pic.png", "photo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if !bytes.Equal(got.Data, png) || got.ContentType != "image/png" || got.Name != "pic.png" {
		t.Errorf("got %q %q %d bytes", got.ContentType, got.Name, len(got.Data))
	}

	if len(*dialed) != 1 || (*dialed)[0] != public+":443" {
		t.Errorf("dialled %v, want the resolved IP %s:443, not the name", *dialed, public)
	}
}

func TestGetRedirects(t *testing.T) {
	final := serve(body("image/png", png))
	defer final.Close()

	t.Run("followed within the limit", func(t *testing.T) {
		n := 0
		srv := serve(func(w http.ResponseWriter, r *http.Request) {
			if n++; n <= 3 {
				http.Redirect(w, r, "/next", http.StatusFound)
				return
			}
			body("image/png", png)(w, r)
		})
		defer srv.Close()

		f, _ := newFetcher(t, srv, public)
		if _, err := f.Get(context.Background(), "https://example.com/", "photo"); err != nil {
			t.Fatalf("3 redirects must be followed: %v", err)
		}
	})

	t.Run("too many", func(t *testing.T) {
		srv := serve(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/again", http.StatusFound)
		})
		defer srv.Close()

		f, _ := newFetcher(t, srv, public)
		if _, err := f.Get(context.Background(), "https://example.com/", "photo"); err == nil ||
			!strings.Contains(err.Error(), "redirects") {
			t.Fatalf("err = %v, want the redirect limit", err)
		}
	})

	t.Run("to http", func(t *testing.T) {
		srv := serve(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.com/a.png", http.StatusFound)
		})
		defer srv.Close()

		f, _ := newFetcher(t, srv, public)
		if _, err := f.Get(context.Background(), "https://example.com/", "photo"); err == nil ||
			!strings.Contains(err.Error(), "https only") {
			t.Fatalf("err = %v, want https-only refusal", err)
		}
	})

	t.Run("to a private address", func(t *testing.T) {
		srv := serve(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://internal.example/a.png", http.StatusFound)
		})
		defer srv.Close()

		f, _ := newFetcher(t, srv, public)
		// the first host is public, the redirect target resolves privately
		f.Resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "internal.example" {
				return []netip.Addr{netip.MustParseAddr("10.0.0.9")}, nil
			}

			return []netip.Addr{netip.MustParseAddr(public)}, nil
		}

		if _, err := f.Get(context.Background(), "https://example.com/", "photo"); err == nil ||
			!strings.Contains(err.Error(), "not a public address") {
			t.Fatalf("err = %v, want the redirect target refused", err)
		}
	})

	t.Run("to a literal private IP", func(t *testing.T) {
		srv := serve(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://169.254.169.254/latest", http.StatusFound)
		})
		defer srv.Close()

		f, _ := newFetcher(t, srv, public)
		if _, err := f.Get(context.Background(), "https://example.com/", "photo"); err == nil ||
			!strings.Contains(err.Error(), "not a public address") {
			t.Fatalf("err = %v, want refusal", err)
		}
	})
}

func TestGetContent(t *testing.T) {
	big := append(append([]byte{}, png...), make([]byte, MaxBytes)...)
	html := []byte("<html><body>nope</body></html>")

	cases := map[string]struct {
		kind    string
		handler http.HandlerFunc
		want    string // empty means success
	}{
		"ok jpeg":             {"photo", body("image/jpeg", jpeg), ""},
		"charset param":       {"photo", body("image/png; charset=binary", png), ""},
		"oversize":            {"photo", body("image/png", big), "larger than"},
		"wrong content type":  {"photo", body("text/html", png), "content type"},
		"svg is not accepted": {"photo", body("image/svg+xml", []byte("<svg/>")), "content type"},
		"no content type":     {"photo", func(w http.ResponseWriter, _ *http.Request) { w.Header()["Content-Type"] = nil; _, _ = w.Write(png) }, "content type"},
		"sniff mismatch html": {"photo", body("image/png", html), "not accepted for photo"},
		"sniff vs declared":   {"photo", body("image/png", jpeg), "server says image/png but the content is image/jpeg"},
		"not found":           {"photo", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }, "404"},
		"pdf is not a photo":  {"photo", body("application/pdf", []byte("%PDF-1.7 ....")), "content type"},
		"pdf as manual":       {"manual", body("application/pdf", []byte("%PDF-1.7 ....")), ""},
		"empty":               {"photo", body("image/png", nil), "empty"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := serve(tc.handler)
			defer srv.Close()

			f, _ := newFetcher(t, srv, public)

			_, err := f.Get(context.Background(), "https://example.com/x", tc.kind)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestDecode(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString

	if f, err := Decode(enc(png), "photo"); err != nil || f.ContentType != "image/png" {
		t.Fatalf("png: %v %v", f, err)
	}

	if _, err := Decode("  "+enc(jpeg)[:8]+"\n"+enc(jpeg)[8:], "photo"); err != nil {
		t.Errorf("whitespace in base64 must be tolerated: %v", err)
	}

	for name, in := range map[string]string{
		"not base64":  "!!!not base64!!!",
		"html":        enc([]byte("<html>")),
		"oversize":    enc(append(append([]byte{}, png...), make([]byte, MaxBytes)...)),
		"huge string": strings.Repeat("A", 4*MaxBytes),
		"empty":       "",
	} {
		if _, err := Decode(in, "photo"); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
