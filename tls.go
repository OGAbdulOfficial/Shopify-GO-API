package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// ─── Chrome fingerprint pool ─────────────────────────────────────────────────

var fingerprints = []Fingerprint{
	// Chrome 131 - Windows
	{Impersonate: "chrome131", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		SecCHUA: `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`, SecCHUAPlatform: `"Windows"`, SecCHUAMobile: "?0"},
	// Chrome 131 - macOS
	{Impersonate: "chrome131", UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		SecCHUA: `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`, SecCHUAPlatform: `"macOS"`, SecCHUAMobile: "?0"},
	// Chrome 131 - Linux
	{Impersonate: "chrome131", UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		SecCHUA: `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`, SecCHUAPlatform: `"Linux"`, SecCHUAMobile: "?0"},
	// Chrome 120 - Windows
	{Impersonate: "chrome120", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		SecCHUA: `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`, SecCHUAPlatform: `"Windows"`, SecCHUAMobile: "?0"},
	// Chrome 120 - macOS
	{Impersonate: "chrome120", UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		SecCHUA: `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`, SecCHUAPlatform: `"macOS"`, SecCHUAMobile: "?0"},
	// Chrome 120 - Linux
	{Impersonate: "chrome120", UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		SecCHUA: `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`, SecCHUAPlatform: `"Linux"`, SecCHUAMobile: "?0"},
}

func randomFingerprint() Fingerprint {
	return fingerprints[rand.IntN(len(fingerprints))]
}

// ─── uTLS spec from fingerprint name ─────────────────────────────────────────

func utlsSpec(fp Fingerprint) *utls.ClientHelloID {
	switch fp.Impersonate {
	case "chrome131":
		return &utls.HelloChrome_131
	case "chrome120":
		return &utls.HelloChrome_120
	default:
		return &utls.HelloChrome_131
	}
}

// ─── uTLS round-tripper: real Chrome TLS fingerprint with connection pooling ─

type cachedConn struct {
	h2  *http2.ClientConn // nil if HTTP/1.1
	tls *utls.UConn
	raw net.Conn
}

type utlsTransport struct {
	spec    *utls.ClientHelloID
	proxy   func(*http.Request) (*url.URL, error)
	timeout time.Duration

	mu    sync.Mutex
	conns map[string]*cachedConn // host:port → reusable connection (HTTP/2 only)

	// Fallback standard transport for non-HTTPS
	plainOnce sync.Once
	plainTr   *http.Transport
}

func (t *utlsTransport) getPlainTransport() *http.Transport {
	t.plainOnce.Do(func() {
		t.plainTr = &http.Transport{Proxy: t.proxy}
	})
	return t.plainTr
}

// dial establishes a new TCP+uTLS connection to the target
func (t *utlsTransport) dial(ctx context.Context, req *http.Request, host, addr string) (*utls.UConn, net.Conn, error) {
	var rawConn net.Conn
	var err error

	if t.proxy != nil {
		proxyURL, pErr := t.proxy(req)
		if pErr == nil && proxyURL != nil {
			rawConn, err = dialThroughProxy(ctx, proxyURL, addr)
		} else {
			rawConn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}
	} else {
		rawConn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	tlsConn := utls.UClient(rawConn, &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: true, //nolint:gosec
	}, *t.spec)

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, nil, fmt.Errorf("tls handshake %s: %w", host, err)
	}

	return tlsConn, rawConn, nil
}

func (t *utlsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Non-HTTPS → standard transport
	if req.URL.Scheme != "https" {
		return t.getPlainTransport().RoundTrip(req)
	}

	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(host, port)

	ctx := req.Context()
	if t.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}

	// ── Try cached HTTP/2 connection first ──
	t.mu.Lock()
	if t.conns == nil {
		t.conns = make(map[string]*cachedConn)
	}
	cc := t.conns[addr]
	t.mu.Unlock()

	if cc != nil && cc.h2 != nil {
		if cc.h2.CanTakeNewRequest() {
			resp, err := cc.h2.RoundTrip(req)
			if err == nil {
				return resp, nil
			}
			// Connection is dead — remove from cache and fall through to new connection
			t.mu.Lock()
			if t.conns[addr] == cc { // still the same object
				delete(t.conns, addr)
			}
			t.mu.Unlock()
			cc.tls.Close()
		} else {
			// Can't take new requests — evict
			t.mu.Lock()
			if t.conns[addr] == cc {
				delete(t.conns, addr)
			}
			t.mu.Unlock()
		}
	}

	// ── New connection ──
	tlsConn, rawConn, err := t.dial(ctx, req, host, addr)
	if err != nil {
		return nil, err
	}

	alpn := tlsConn.ConnectionState().NegotiatedProtocol

	if alpn == "h2" {
		// HTTP/2: create a ClientConn we can reuse
		h2Transport := &http2.Transport{
			DisableCompression: false,
			AllowHTTP:          false,
		}
		h2cc, err := h2Transport.NewClientConn(tlsConn)
		if err != nil {
			tlsConn.Close()
			return nil, fmt.Errorf("h2 client conn: %w", err)
		}

		// Cache for reuse
		entry := &cachedConn{h2: h2cc, tls: tlsConn, raw: rawConn}
		t.mu.Lock()
		t.conns[addr] = entry
		t.mu.Unlock()

		resp, err := h2cc.RoundTrip(req)
		if err != nil {
			// Evict on error
			t.mu.Lock()
			if t.conns[addr] == entry {
				delete(t.conns, addr)
			}
			t.mu.Unlock()
			tlsConn.Close()
			return nil, err
		}
		return resp, nil
	}

	// HTTP/1.1 fallback (no reuse — rare for Shopify)
	connTransport := &http.Transport{
		DialTLSContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return tlsConn, nil
		},
		DisableKeepAlives: true,
	}

	resp, err := connTransport.RoundTrip(req)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	return resp, nil
}

// Close cleans up all cached connections
func (t *utlsTransport) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for addr, cc := range t.conns {
		if cc.tls != nil {
			cc.tls.Close()
		}
		delete(t.conns, addr)
	}
}

// dialThroughProxy establishes a TCP connection through an HTTP/SOCKS proxy
func dialThroughProxy(ctx context.Context, proxyURL *url.URL, target string) (net.Conn, error) {
	switch proxyURL.Scheme {
	case "http", "https":
		return dialHTTPProxy(ctx, proxyURL, target)
	case "socks5", "socks5h":
		return dialSOCKS5Proxy(ctx, proxyURL, target)
	default:
		// Try as HTTP proxy
		return dialHTTPProxy(ctx, proxyURL, target)
	}
}

func dialHTTPProxy(ctx context.Context, proxyURL *url.URL, target string) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if !hasPort(proxyAddr) {
		proxyAddr = net.JoinHostPort(proxyAddr, "80")
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("proxy dial: %w", err)
	}

	// CONNECT tunnel
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		auth := base64Encode(user + ":" + pass)
		connectReq += "Proxy-Authorization: Basic " + auth + "\r\n"
	}
	connectReq += "\r\n"

	if _, err := conn.Write([]byte(connectReq)); err != nil {
		conn.Close()
		return nil, err
	}

	// Read response
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp := string(buf[:n])
	if len(resp) < 12 || resp[9] != '2' {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s", resp[:min(len(resp), 80)])
	}

	return conn, nil
}

func dialSOCKS5Proxy(ctx context.Context, proxyURL *url.URL, target string) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if !hasPort(proxyAddr) {
		proxyAddr = net.JoinHostPort(proxyAddr, "1080")
	}
	// For SOCKS5, use Go's built-in SOCKS support via the proxy package
	// But for simplicity and to keep deps minimal, use net.Dialer with proxy
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial: %w", err)
	}

	// SOCKS5 handshake
	user := ""
	pass := ""
	if proxyURL.User != nil {
		user = proxyURL.User.Username()
		pass, _ = proxyURL.User.Password()
	}

	if err := socks5Handshake(conn, target, user, pass); err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}

func socks5Handshake(conn net.Conn, target, user, pass string) error {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	port := 0
	fmt.Sscanf(portStr, "%d", &port)

	// Greeting
	if user != "" {
		conn.Write([]byte{0x05, 0x02, 0x00, 0x02}) // NoAuth + UserPass
	} else {
		conn.Write([]byte{0x05, 0x01, 0x00}) // NoAuth
	}

	buf := make([]byte, 2)
	if _, err := conn.Read(buf); err != nil {
		return err
	}
	if buf[0] != 0x05 {
		return fmt.Errorf("socks5: invalid version %d", buf[0])
	}

	// User/password auth
	if buf[1] == 0x02 {
		authMsg := []byte{0x01, byte(len(user))}
		authMsg = append(authMsg, []byte(user)...)
		authMsg = append(authMsg, byte(len(pass)))
		authMsg = append(authMsg, []byte(pass)...)
		conn.Write(authMsg)

		resp := make([]byte, 2)
		if _, err := conn.Read(resp); err != nil {
			return err
		}
		if resp[1] != 0x00 {
			return fmt.Errorf("socks5: auth failed")
		}
	} else if buf[1] != 0x00 {
		return fmt.Errorf("socks5: unsupported auth method %d", buf[1])
	}

	// Connect request
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port&0xff))
	conn.Write(req)

	resp := make([]byte, 256)
	if _, err := conn.Read(resp); err != nil {
		return err
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("socks5: connect failed with code %d", resp[1])
	}
	return nil
}

// ─── Create an HTTP client with real Chrome TLS fingerprint ──────────────────

func newClient(fp Fingerprint, proxyURL string, timeout time.Duration) *http.Client {
	spec := utlsSpec(fp)

	var proxyFunc func(*http.Request) (*url.URL, error)
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err == nil {
			proxyFunc = http.ProxyURL(parsed)
		}
	}

	jar, _ := cookiejar.New(nil)

	return &http.Client{
		Timeout: timeout,
		Jar:     jar,
		Transport: &utlsTransport{
			spec:    spec,
			proxy:   proxyFunc,
			timeout: timeout,
		},
		// Follow redirects (default behavior), but capture final URL
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// newStandardClient creates a client without uTLS (for non-TLS-sensitive endpoints like card tokenization)
func newStandardClient(proxyURL string, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err == nil {
			tr.Proxy = http.ProxyURL(parsed)
		}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		Jar:       mustCookieJar(),
	}
}

func mustCookieJar() http.CookieJar {
	jar, _ := cookiejar.New(nil)
	return jar
}

func hasPort(host string) bool {
	_, _, err := net.SplitHostPort(host)
	return err == nil
}

func base64Encode(s string) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	result := make([]byte, 0, (len(s)+2)/3*4)
	for i := 0; i < len(s); i += 3 {
		var n uint32
		remaining := len(s) - i
		switch {
		case remaining >= 3:
			n = uint32(s[i])<<16 | uint32(s[i+1])<<8 | uint32(s[i+2])
			result = append(result, charset[n>>18&63], charset[n>>12&63], charset[n>>6&63], charset[n&63])
		case remaining == 2:
			n = uint32(s[i])<<16 | uint32(s[i+1])<<8
			result = append(result, charset[n>>18&63], charset[n>>12&63], charset[n>>6&63], '=')
		case remaining == 1:
			n = uint32(s[i]) << 16
			result = append(result, charset[n>>18&63], charset[n>>12&63], '=', '=')
		}
	}
	return string(result)
}
