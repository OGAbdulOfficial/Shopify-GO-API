package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

var quietLogs atomic.Bool

// SetQuietLogs suppresses PRODUCT/CHECKOUT/debug lines (used by GET /shopify).
func SetQuietLogs(v bool) {
	quietLogs.Store(v)
}

// logDebug writes structured debug lines to stderr (visible in API/server logs).
func logDebug(component, format string, args ...any) {
	if quietLogs.Load() {
		return
	}
	fmt.Fprintf(os.Stderr, "[%s] %s\n", component, fmt.Sprintf(format, args...))
}

func logProductAPI(shopURL, strategy, path string, status int, count int, note string) {
	host := strings.TrimPrefix(strings.TrimPrefix(shopURL, "https://"), "http://")
	if note != "" {
		logDebug("PRODUCT", "%s | %s | %s → HTTP %d | %d products | %s", host, strategy, path, status, count, note)
		return
	}
	logDebug("PRODUCT", "%s | %s | %s → HTTP %d | %d products", host, strategy, path, status, count)
}

// logProductAPIAudit — full per-request audit (status, bytes, body snippet, Shopify headers).
func logProductAPIAudit(shopURL, api, path string, resp *http.Response, body []byte, extra string) {
	host := strings.TrimPrefix(strings.TrimPrefix(shopURL, "https://"), "http://")
	status := 0
	shopHdr := ""
	if resp != nil {
		status = resp.StatusCode
		for _, k := range []string{"X-Shopid", "X-Shopify-Shop-Api-Call-Limit", "Retry-After", "Cf-Cache-Status", "Shopify-Complexity-Score"} {
			if v := resp.Header.Get(k); v != "" {
				shopHdr += fmt.Sprintf(" %s=%s", k, v)
			}
		}
	}
	preview := auditBodyPreview(body)
	logDebug("PRODUCT-AUDIT", "%s | API=%s | path=%s | HTTP %d | bytes=%d |%s | body=%s | %s",
		host, api, path, status, len(body), shopHdr, preview, extra)
}

func auditBodyPreview(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\n", " ")
	if s == "" {
		return "(empty)"
	}
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

func logCheckout(siteLabel, step, format string, args ...any) {
	logDebug("CHECKOUT", "[%s] %s: %s", siteLabel, step, fmt.Sprintf(format, args...))
}

func extractLastStepCode(failDetails []string) (step, code string) {
	for i := len(failDetails) - 1; i >= 0; i-- {
		parts := strings.SplitN(failDetails[i], ":", 3)
		if len(parts) >= 3 {
			return parts[1], parts[2]
		}
	}
	return "", ""
}

func lastCodeOr(fallback string, failDetails []string) string {
	_, code := extractLastStepCode(failDetails)
	if code != "" {
		return code
	}
	return fallback
}
