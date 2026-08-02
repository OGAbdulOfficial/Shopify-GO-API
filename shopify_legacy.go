package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ShopifyLegacyResponse matches the desired GET /shopify JSON format.
type ShopifyLegacyResponse struct {
	CC       string `json:"cc"`
	Gateway  string `json:"Gateway"`
	Response string `json:"Response"`
	Price    string `json:"Price"`
	Currency string `json:"Currency"`
	Status   string `json:"Status"`
	Time     string `json:"Time"`
	Proxy    string `json:"Proxy"`
}

// realStdout keeps the process stdout handle for logging API results.
var realStdout = os.Stdout

func handleShopifyLegacy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed, use GET"})
		return
	}

	start := time.Now()
	site := strings.TrimSpace(r.URL.Query().Get("site"))
	cc := strings.TrimSpace(r.URL.Query().Get("cc"))
	proxyRaw := strings.TrimSpace(r.URL.Query().Get("proxy"))

	if site == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "site query parameter is required"})
		return
	}
	if cc == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cc query parameter is required (format: num|mm|yyyy|cvv)"})
		return
	}

	proxyStatus := "Not Used"
	var proxyURL string
	var proxies []string

	if proxyRaw != "" {
		proxyURL = normalizeProxy(proxyRaw)
		if proxyURL == "" {
			legacy := ShopifyLegacyResponse{
				CC:       cc,
				Gateway:  "UNKNOWN",
				Response: "<b>Invalid proxy format</b>",
				Price:    "0",
				Currency: "USD",
				Status:   "false",
				Time:     formatLegacyTime(elapsed(start)),
				Proxy:    "Dead",
			}
			logShopifyResult(legacy)
			writeJSON(w, http.StatusOK, legacy)
			return
		}
		if testProxyConnectivity(proxyURL) {
			proxyStatus = "Live"
			proxies = []string{proxyURL}
		} else {
			proxyStatus = "Dead"
			legacy := ShopifyLegacyResponse{
				CC:       cc,
				Gateway:  "UNKNOWN",
				Response: "<b>Proxy Dead!</b>",
				Price:    "0",
				Currency: "USD",
				Status:   "false",
				Time:     formatLegacyTime(elapsed(start)),
				Proxy:    proxyStatus,
			}
			logShopifyResult(legacy)
			writeJSON(w, http.StatusOK, legacy)
			return
		}
	}



	legacy := runShopifyQuiet(cc, site, proxyURL, proxies, proxyStatus)
	logShopifyResult(legacy)
	writeJSON(w, http.StatusOK, legacy)
}

func runShopifyQuiet(cc, site, proxyURL string, proxies []string, proxyStatus string) ShopifyLegacyResponse {

	addrFile := ""
	if _, statErr := os.Stat("addresses.txt"); statErr == nil {
		addrFile = "addresses.txt"
	}

	result := processSingleMultiSite(
		cc,
		[]string{site},
		proxyURL,
		proxies,
		addrFile,
		make(map[string]CachedProduct),
		nil,
	)
	return mapSingleToLegacy(result, cc, proxyStatus)
}

// logShopifyResult writes the compact legacy JSON to the real process stdout so it
// always appears in API/server logs (same line as the HTTP response body).
func logShopifyResult(legacy ShopifyLegacyResponse) {
	b, err := json.Marshal(legacy)
	if err != nil {
		return
	}
	fmt.Fprintln(realStdout, string(b))
}

func mapSingleToLegacy(r SingleResponse, cc, proxyStatus string) ShopifyLegacyResponse {
	price := formatLegacyPrice(parseLegacyPrice(r.Amount, r.ProductPrice))
	status := "false"
	if legacyStatus(r) {
		status = "true"
	}

	return ShopifyLegacyResponse{
		CC:       cc,
		Gateway:  legacyGateway(r),
		Response: legacyResponse(r),
		Price:    price,
		Currency: "USD",
		Status:   status,
		Time:     formatLegacyTime(r.Elapsed),
		Proxy:    proxyStatus,
	}
}

func legacyGateway(r SingleResponse) string {
	if strings.TrimSpace(r.Gateway) != "" {
		return r.Gateway
	}
	return "UNKNOWN"
}

func legacyStatus(r SingleResponse) bool {
	return r.Status == "charged"
}

func legacyResponse(r SingleResponse) string {
	if isCaptchaRequired(r) {
		return "CARD_DECLINED"
	}
	if isActionRequired(r) {
		return "3DS_REQUIRED"
	}
	if msg := legacySiteErrorMessage(r); msg != "" {
		return msg
	}
	if msg := legacyProductErrorMessage(r); msg != "" {
		return msg
	}

	switch r.Status {
	case "charged":
		return "ORDER_PAID"
	case "declined", "approved", "cvv":
		if strings.EqualFold(r.Code, "ACTION_REQUIRED") {
			return "3DS_REQUIRED"
		}
		if r.Code != "" {
			return r.Code
		}
		return strings.ToUpper(r.Status)
	case "captcha":
		return "CARD_DECLINED"
	case "site_error", "error":
		if r.Code != "" && r.Code != "ALL_SITES_EXHAUSTED" {
			return r.Code
		}
		if r.Error != "" {
			return r.Error
		}
		if r.Code != "" {
			return r.Code
		}
		return "Site Error"
	default:
		if strings.EqualFold(r.Code, "ACTION_REQUIRED") {
			return "3DS_REQUIRED"
		}
		if r.Code != "" && r.Code != "ALL_SITES_EXHAUSTED" {
			return r.Code
		}
		if _, code := extractLastStepCode(r.FailureDetails); code != "" {
			if strings.EqualFold(code, "CAPTCHA_REQUIRED") {
				return "CARD_DECLINED"
			}
			if strings.EqualFold(code, "ACTION_REQUIRED") {
				return "3DS_REQUIRED"
			}
			return code
		}
		if r.Code != "" {
			return r.Code
		}
		if r.Error != "" {
			return r.Error
		}
		return "Unknown Error"
	}
}

func isActionRequired(r SingleResponse) bool {
	if strings.EqualFold(strings.TrimSpace(r.Code), "ACTION_REQUIRED") {
		return true
	}
	if _, code := extractLastStepCode(r.FailureDetails); strings.EqualFold(code, "ACTION_REQUIRED") {
		return true
	}
	return false
}

func legacyProductErrorMessage(r SingleResponse) string {
	code := strings.ToUpper(strings.TrimSpace(r.Code))
	switch code {
	case "NO_PRODUCT_FOUND":
		return "NO_PRODUCT_FOUND"
	case "HTTP_429":
		return "<b>Site Error! Status: 429</b>"
	}
	if strings.Contains(strings.ToLower(r.Error), "none had products") {
		return "NO_PRODUCT_FOUND"
	}
	return ""
}

func isCaptchaRequired(r SingleResponse) bool {
	if strings.EqualFold(strings.TrimSpace(r.Code), "CAPTCHA_REQUIRED") {
		return true
	}
	if _, code := extractLastStepCode(r.FailureDetails); strings.EqualFold(code, "CAPTCHA_REQUIRED") {
		return true
	}
	return false
}

func legacySiteErrorMessage(r SingleResponse) string {
	err := strings.ToLower(r.Error)
	code := strings.ToUpper(r.Code)

	for _, status := range []string{"429", "403", "404", "402", "500", "502", "503"} {
		if strings.Contains(err, "status: "+status) ||
			strings.Contains(err, "status "+status) ||
			strings.Contains(code, "HTTP_"+status) ||
			code == status {
			return fmt.Sprintf("<b>Site Error! Status: %s</b>", status)
		}
	}

	if strings.Contains(code, "HTTP_429") || strings.Contains(err, "429") {
		return "<b>Site Error! Status: 429</b>"
	}
	if strings.Contains(code, "THROTTLED") {
		return "<b>Site Error! Status: 429</b>"
	}
	if r.ErrorType == "site_ratelimit" || r.ErrorType == "rate_limit" {
		return "<b>Site Error! Status: 429</b>"
	}
	if strings.Contains(err, "session token not found") {
		return "<b>Site Error! Status: 403</b>"
	}
	if strings.Contains(err, "cartCreate failed") || strings.Contains(err, "cart add failed") {
		return "NO_PRODUCT_FOUND"
	}
	if strings.Contains(code, "ALL_SITES_EXHAUSTED") && strings.Contains(err, "429") {
		return "<b>Site Error! Status: 429</b>"
	}

	return ""
}

func parseLegacyPrice(amount, productPrice string) float64 {
	for _, s := range []string{amount, productPrice} {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		s = strings.TrimPrefix(s, "$")
		s = strings.TrimPrefix(s, "€")
		s = strings.TrimPrefix(s, "£")
		s = strings.ReplaceAll(s, ",", "")
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v
		}
	}
	return 0
}

// formatLegacyPrice renders price as a compact string ("8.9", "5", "17.22").
func formatLegacyPrice(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if s == "" {
		return "0"
	}
	return s
}

func formatLegacyTime(seconds float64) string {
	return fmt.Sprintf("%.2fs", seconds)
}

func htmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(s)
}
