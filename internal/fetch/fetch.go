// Package fetch downloads an image on behalf of a caller who is not trusted.
//
// The URL comes from a model, and a model can be talked into asking for
// anything -- including http://169.254.169.254/ or a service on the cluster
// network. So this package is a guard first and a downloader second: https
// only, every resolved address checked against the ranges a public web
// server never has, the checked address (not the name) dialled so DNS cannot
// swap it afterwards, redirects held to the same rules, and size, time and
// content type bounded.
package fetch

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	// MaxBytes is the largest file accepted, from a URL or from base64.
	MaxBytes = 10 << 20

	maxRedirects = 3
	timeout      = 20 * time.Second
)

// Image types accepted for a photo. The sniffed type must be one of these and
// must agree with the declared one.
var imageTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true,
}

// Allowed reports whether content of type ct may be uploaded as attachment
// type kind. Photos are images only; other kinds also take PDFs, because a
// manual or receipt usually is one.
func allowed(kind, ct string) bool {
	if imageTypes[ct] {
		return true
	}

	return kind != "photo" && ct == "application/pdf"
}

// blocked are the ranges netip does not already classify.
var blocked = mustPrefixes(
	"0.0.0.0/8",       // "this network"
	"100.64.0.0/10",   // CGNAT
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // reserved, incl. broadcast
	"64:ff9b::/96",    // NAT64: embeds an IPv4 address we would have to re-check
	"100::/64",        // discard-only
	"2001:db8::/32",   // documentation
)

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}

	return out
}

// Refused reports whether ip is an address the fetcher will not connect to.
func Refused(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:10.0.0.1 is 10.0.0.1

	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}

	for _, p := range blocked {
		if p.Contains(ip) {
			return true
		}
	}

	return false
}

// Fetcher downloads from public https hosts only. The zero value is not
// usable; call New. The fields are injectable so tests need no network.
type Fetcher struct {
	// Resolve returns the addresses of host. Default: the system resolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial connects to an already-checked "ip:port". Default: net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLS overrides the TLS client config; tests use it to trust a test CA.
	TLS *tls.Config
}

// New returns a Fetcher using the system resolver and dialer.
func New() *Fetcher { return &Fetcher{} }

func (f *Fetcher) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if f.Resolve != nil {
		return f.Resolve(ctx, host)
	}

	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// dialContext resolves host, refuses the connection if ANY address is in a
// refused range, and dials one of the checked addresses. Checking here, in
// the dialer, covers the first request and every redirect alike, and means
// the address that was checked is the address that is used.
func (f *Fetcher) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	var ips []netip.Addr

	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = f.resolve(ctx, host); err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}

	for _, ip := range ips {
		if Refused(ip) {
			return nil, fmt.Errorf("refusing %s: resolves to %s, which is not a public address", host, ip.Unmap())
		}
	}

	dial := f.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}

	var last error

	for _, ip := range ips {
		c, err := dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return c, nil
		}

		last = err
	}

	return nil, last
}

func (f *Fetcher) client() *http.Client {
	return &http.Client{
		Timeout: timeout,
		// No Jar: cookies are never stored or replayed across redirects.
		Transport: &http.Transport{
			Proxy:             nil, // an env proxy would resolve and dial for us, unchecked
			DialContext:       f.dialContext,
			TLSClientConfig:   f.TLS,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}

			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to %s: https only", req.URL.Scheme)
			}

			return nil
		},
	}
}

// File is downloaded or decoded content, already type-checked.
type File struct {
	Data        []byte
	ContentType string // sniffed
	Name        string // suggested file name, may be empty
}

// Get downloads rawURL. kind is the HomeBox attachment type, which decides
// which content types are acceptable.
func (f *Fetcher) Get(ctx context.Context, rawURL, kind string) (*File, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}

	if u.Scheme != "https" {
		return nil, errors.New("url: only https is accepted")
	}

	if u.Hostname() == "" {
		return nil, errors.New("url: no host")
	}

	if u.User != nil {
		return nil, errors.New("url: credentials in the URL are not accepted")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}

	req.Header.Set("Accept", "image/*")

	resp, err := f.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: %s", resp.Status)
	}

	declared, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !allowed(kind, declared) {
		return nil, fmt.Errorf("download: content type %q is not accepted for %s", resp.Header.Get("Content-Type"), kind)
	}

	if resp.ContentLength > MaxBytes {
		return nil, fmt.Errorf("download: %d bytes exceeds the %d byte limit", resp.ContentLength, MaxBytes)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}

	if len(data) > MaxBytes {
		return nil, fmt.Errorf("download: larger than the %d byte limit", MaxBytes)
	}

	sniffed, err := check(data, kind)
	if err != nil {
		return nil, err
	}

	if sniffed != declared {
		return nil, fmt.Errorf("download: server says %s but the content is %s", declared, sniffed)
	}

	name := path.Base(u.Path)
	if name == "." || name == "/" {
		name = ""
	}

	return &File{Data: data, ContentType: sniffed, Name: name}, nil
}

// Decode reads base64 content, applying the same size and type rules.
func Decode(s, kind string) (*File, error) {
	s = strings.Join(strings.Fields(s), "")

	if base64.StdEncoding.DecodedLen(len(s)) > MaxBytes+3 {
		return nil, fmt.Errorf("data_base64: larger than the %d byte limit", MaxBytes)
	}

	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("data_base64: %w", err)
	}

	if len(data) > MaxBytes {
		return nil, fmt.Errorf("data_base64: larger than the %d byte limit", MaxBytes)
	}

	sniffed, err := check(data, kind)
	if err != nil {
		return nil, err
	}

	return &File{Data: data, ContentType: sniffed}, nil
}

// check sniffs the bytes -- the declared type is only a claim -- and returns
// the real type if it is acceptable for kind.
func check(data []byte, kind string) (string, error) {
	if len(data) == 0 {
		return "", errors.New("content is empty")
	}

	ct := http.DetectContentType(data)
	if !allowed(kind, ct) {
		return "", fmt.Errorf("content is %s, which is not accepted for %s", ct, kind)
	}

	return ct, nil
}
