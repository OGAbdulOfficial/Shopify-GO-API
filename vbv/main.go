package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type BatchVBVRequest struct {
	Cards      []string `json:"cards"`
	Site       string   `json:"site,omitempty"`
	Proxies    []string `json:"proxies,omitempty"`
	MaxWorkers int      `json:"max_workers,omitempty"` // Default 4
}

type BatchVBVResponse struct {
	Results    []VBVResponse `json:"results"`
	TotalCards int           `json:"total_cards"`
	Elapsed    float64       `json:"elapsed_seconds"`
}

var (
	defaultSites = []string{
		"https://checkout.stripe.com",
		"https://pages.razorpay.com/epdonation",
	}
	sitesMu sync.RWMutex
)

func main() {
	port := os.Getenv("VBV_PORT")
	if port == "" {
		port = "8087"
	}

	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/vbv", vbvCheckHandler)
	http.HandleFunc("/vbv/batch", vbvBatchCheckHandler)
	http.HandleFunc("/bin", binLookupHandler)
	http.HandleFunc("/sites", sitesHandler)
	http.HandleFunc("/sites/add", sitesAddHandler)
	http.HandleFunc("/sites/remove", sitesRemoveHandler)
	http.HandleFunc("/sites/test", sitesTestHandler)
	http.HandleFunc("/", rootHandler)

	fmt.Printf("======================================================================\n")
	fmt.Printf("  VBV / 3D-Secure Checker API Server\n")
	fmt.Printf("======================================================================\n")
	fmt.Printf("  Listening on http://0.0.0.0:%s\n\n", port)
	fmt.Printf("  Endpoints:\n")
	fmt.Printf("    GET /vbv?cc=CC|MM|YYYY|CVV&proxy=...&site=...\n")
	fmt.Printf("    POST /vbv (JSON Body: {\"cc\": \"...\", \"proxy\": \"...\", \"site\": \"...\"})\n")
	fmt.Printf("    POST /vbv/batch (JSON Body: {\"cards\": [...], \"proxies\": [...]})\n")
	fmt.Printf("    GET /bin?bin=411111\n")
	fmt.Printf("    GET /sites          - List active VBV sites pool\n")
	fmt.Printf("    POST /sites/add     - Add site to pool (JSON: {\"site\": \"https://...\"})\n")
	fmt.Printf("    POST /sites/remove  - Remove site from pool (JSON: {\"site\": \"https://...\"})\n")
	fmt.Printf("    GET /sites/test     - Live-test all pool sites\n")
	fmt.Printf("    GET /health\n")
	fmt.Printf("======================================================================\n")

	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, nil))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "vbv-checker-api",
		"version": "1.1.0",
	})
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	sitesMu.RLock()
	activePoolCount := len(defaultSites)
	sitesMu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"service":           "VBV / 3D-Secure Checker API",
		"version":           "1.1.0",
		"status":            "online",
		"active_sites_count": activePoolCount,
		"endpoints": map[string]string{
			"single_check": "GET/POST /vbv?cc=4111111111111111|12|2026|123&site=...&proxy=...",
			"batch_check":  "POST /vbv/batch",
			"bin_lookup":   "GET /bin?bin=411111",
			"list_sites":   "GET /sites",
			"add_site":     "POST /sites/add",
			"remove_site":  "POST /sites/remove",
			"test_sites":   "GET /sites/test",
			"health":       "GET /health",
		},
	})
}

func vbvCheckHandler(w http.ResponseWriter, r *http.Request) {
	var req VBVRequest

	switch r.Method {
	case http.MethodGet:
		req.CC = r.URL.Query().Get("cc")
		req.Site = r.URL.Query().Get("site")
		req.Proxy = r.URL.Query().Get("proxy")
		req.Amount = r.URL.Query().Get("amount")
		req.Currency = r.URL.Query().Get("currency")
	case http.MethodPost:
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "Invalid JSON request body: " + err.Error(),
			})
			return
		}
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed. Use GET or POST.",
		})
		return
	}

	if req.CC == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Missing 'cc' parameter. Format: CC|MM|YYYY|CVV",
		})
		return
	}

	// Use site pool if no custom site is passed
	if req.Site == "" {
		sitesMu.RLock()
		if len(defaultSites) > 0 {
			req.Site = defaultSites[0]
		}
		sitesMu.RUnlock()
	}

	resp := PerformVBVCheck(req)
	writeJSON(w, http.StatusOK, resp)
}

func vbvBatchCheckHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed. Use POST for batch check.",
		})
		return
	}

	var batchReq BatchVBVRequest
	if err := json.NewDecoder(r.Body).Decode(&batchReq); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Invalid JSON body: " + err.Error(),
		})
		return
	}

	if len(batchReq.Cards) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "'cards' array is required and must not be empty.",
		})
		return
	}

	maxWorkers := batchReq.MaxWorkers
	if maxWorkers <= 0 || maxWorkers > 50 {
		maxWorkers = 4
	}

	startTime := time.Now()
	totalCards := len(batchReq.Cards)
	results := make([]VBVResponse, totalCards)

	proxyCount := len(batchReq.Proxies)

	sitesMu.RLock()
	poolSites := append([]string{}, defaultSites...)
	sitesMu.RUnlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxWorkers)

	for i, cc := range batchReq.Cards {
		wg.Add(1)
		sem <- struct{}{}

		proxyURL := ""
		if proxyCount > 0 {
			proxyURL = batchReq.Proxies[i%proxyCount]
		}

		targetSite := batchReq.Site
		if targetSite == "" && len(poolSites) > 0 {
			targetSite = poolSites[i%len(poolSites)]
		}

		go func(index int, cardStr string, prx string, site string) {
			defer wg.Done()
			defer func() { <-sem }()

			singleReq := VBVRequest{
				CC:    cardStr,
				Site:  site,
				Proxy: prx,
			}
			results[index] = PerformVBVCheck(singleReq)
		}(i, cc, proxyURL, targetSite)
	}

	wg.Wait()

	elapsed := time.Since(startTime).Seconds()
	writeJSON(w, http.StatusOK, BatchVBVResponse{
		Results:    results,
		TotalCards: totalCards,
		Elapsed:    elapsed,
	})
}

func binLookupHandler(w http.ResponseWriter, r *http.Request) {
	bin := r.URL.Query().Get("bin")
	if bin == "" {
		pathParts := strings.Split(r.URL.Path, "/")
		if len(pathParts) > 2 && pathParts[2] != "" {
			bin = pathParts[2]
		}
	}

	if bin == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Missing 'bin' parameter. Example: GET /bin?bin=411111",
		})
		return
	}

	proxy := r.URL.Query().Get("proxy")
	binInfo := LookupBIN(bin, proxy)
	writeJSON(w, http.StatusOK, binInfo)
}

func sitesHandler(w http.ResponseWriter, r *http.Request) {
	sitesMu.RLock()
	defer sitesMu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"total_sites": len(defaultSites),
		"sites":       defaultSites,
	})
}

func sitesAddHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed. Use POST.",
		})
		return
	}

	var req struct {
		Site string `json:"site"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Site == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Invalid request. Body format: {\"site\": \"https://example.com/checkout\"}",
		})
		return
	}

	newSite := strings.TrimSpace(req.Site)
	if !strings.HasPrefix(newSite, "http://") && !strings.HasPrefix(newSite, "https://") {
		newSite = "https://" + newSite
	}

	sitesMu.Lock()
	// Prevent duplicates
	exists := false
	for _, s := range defaultSites {
		if s == newSite {
			exists = true
			break
		}
	}
	if !exists {
		defaultSites = append(defaultSites, newSite)
	}
	sitesMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "success",
		"message": "Site added to VBV pool successfully",
		"added":   newSite,
		"sites":   defaultSites,
	})
}

func sitesRemoveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Method not allowed. Use POST or DELETE.",
		})
		return
	}

	var req struct {
		Site string `json:"site"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Site == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Invalid request. Body format: {\"site\": \"https://example.com\"}",
		})
		return
	}

	target := strings.TrimSpace(req.Site)

	sitesMu.Lock()
	updated := make([]string, 0, len(defaultSites))
	removed := false
	for _, s := range defaultSites {
		if strings.EqualFold(s, target) || strings.EqualFold(strings.TrimPrefix(s, "https://"), strings.TrimPrefix(target, "https://")) {
			removed = true
		} else {
			updated = append(updated, s)
		}
	}
	defaultSites = updated
	sitesMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "success",
		"removed": removed,
		"sites":   defaultSites,
	})
}

func sitesTestHandler(w http.ResponseWriter, r *http.Request) {
	sitesMu.RLock()
	sitesCopy := append([]string{}, defaultSites...)
	sitesMu.RUnlock()

	type SiteTestResult struct {
		Site     string `json:"site"`
		Status   string `json:"status"`
		HttpCode int    `json:"http_code"`
		Elapsed  string `json:"elapsed"`
	}

	results := make([]SiteTestResult, len(sitesCopy))
	var wg sync.WaitGroup

	for i, siteURL := range sitesCopy {
		wg.Add(1)
		go func(idx int, target string) {
			defer wg.Done()
			start := time.Now()

			client, err := newClient("", 5*time.Second)
			if err != nil {
				client = http.DefaultClient
			}

			req, err := http.NewRequest("GET", target, nil)
			if err != nil {
				results[idx] = SiteTestResult{Site: target, Status: "INVALID_URL", HttpCode: 0, Elapsed: "0s"}
				return
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

			resp, err := client.Do(req)
			if err != nil {
				results[idx] = SiteTestResult{Site: target, Status: "OFFLINE", HttpCode: 0, Elapsed: fmt.Sprintf("%.2fs", time.Since(start).Seconds())}
				return
			}
			defer resp.Body.Close()

			results[idx] = SiteTestResult{
				Site:     target,
				Status:   "ONLINE",
				HttpCode: resp.StatusCode,
				Elapsed:  fmt.Sprintf("%.2fs", time.Since(start).Seconds()),
			}
		}(i, siteURL)
	}

	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{
		"total_tested": len(results),
		"results":      results,
	})
}

func writeJSON(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(data)
}
