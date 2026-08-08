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

	"gocheck/stripe"
)

// serverSem limits concurrent checkout flows to prevent resource exhaustion.
// 200 concurrent flows is well within Go's goroutine capacity.
var serverSem = make(chan struct{}, 200)

// ─── API Request / Response types ────────────────────────────────────────────

// CheckRequest is the JSON body for POST /api/check (single card)
type CheckRequest struct {
	Card    string   `json:"card"`              // "num|mm|yyyy|cvv"
	Sites   []string `json:"sites"`             // list of Shopify site URLs
	Proxies []string `json:"proxies,omitempty"` // list of proxy URLs

	// Optional fields
	ProductCache       map[string]CachedProduct `json:"product_cache,omitempty"`
	AddressesFile      string                   `json:"addresses_file,omitempty"`
	TestSitesBlacklist []string                 `json:"test_sites,omitempty"`
}

// BatchCheckRequest is the JSON body for POST /api/check/batch (multiple cards)
type BatchCheckRequest struct {
	Cards   []string `json:"cards"`             // ["num|mm|yyyy|cvv", ...]
	Sites   []string `json:"sites"`             // list of Shopify site URLs
	Proxies []string `json:"proxies,omitempty"` // list of proxy URLs

	// Optional fields
	ProductCache       map[string]CachedProduct `json:"product_cache,omitempty"`
	AddressesFile      string                   `json:"addresses_file,omitempty"`
	TestSitesBlacklist []string                 `json:"test_sites,omitempty"`
	MaxWorkers         int                      `json:"max_workers,omitempty"` // default 4
}

// BatchCheckResponse wraps results for multiple cards
type BatchCheckResponse struct {
	Results    []SingleResponse `json:"results"`
	TotalCards int              `json:"total_cards"`
	Elapsed    float64          `json:"elapsed"`
}

// ─── HTTP Handlers ───────────────────────────────────────────────────────────

// handleHealth returns a simple health check
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "gocheck-api",
	})
}

// handleCheckSingle processes a single card check via the API
func handleCheckSingle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed, use POST"}`, http.StatusMethodNotAllowed)
		return
	}

	var req CheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid JSON: " + err.Error(),
		})
		return
	}

	// Validate required fields
	if req.Card == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "card is required (format: num|mm|yyyy|cvv)",
		})
		return
	}
	if len(req.Sites) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "sites array is required and must not be empty",
		})
		return
	}

	// Acquire server-level concurrency slot
	serverSem <- struct{}{}
	defer func() { <-serverSem }()

	// Build proxy string for backward compat
	proxyStr := ""
	if len(req.Proxies) > 0 {
		proxyStr = req.Proxies[0]
	}

	if req.ProductCache == nil {
		req.ProductCache = make(map[string]CachedProduct)
	}

	// Default to addresses.txt if no addresses file specified (API callers
	// like STALIN dashboard don't know the server's file path)
	addrFile := req.AddressesFile
	if addrFile == "" {
		if _, err := os.Stat("addresses.txt"); err == nil {
			addrFile = "addresses.txt"
		}
	}

	// Process the card using the existing engine
	result := processSingleMultiSite(
		req.Card,
		req.Sites,
		proxyStr,
		req.Proxies,
		addrFile,
		req.ProductCache,
		req.TestSitesBlacklist,
	)

	writeJSON(w, http.StatusOK, result)
}

// handleCheckBatch processes multiple cards in parallel via the API
func handleCheckBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed, use POST"}`, http.StatusMethodNotAllowed)
		return
	}

	var req BatchCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid JSON: " + err.Error(),
		})
		return
	}

	// Validate required fields
	if len(req.Cards) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "cards array is required and must not be empty",
		})
		return
	}
	if len(req.Sites) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "sites array is required and must not be empty",
		})
		return
	}



	maxWorkers := req.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = 4
	}
	if maxWorkers > 16 {
		maxWorkers = 16
	}

	proxyStr := ""
	if len(req.Proxies) > 0 {
		proxyStr = req.Proxies[0]
	}

	if req.ProductCache == nil {
		req.ProductCache = make(map[string]CachedProduct)
	}

	// Default to addresses.txt if no addresses file specified
	batchAddrFile := req.AddressesFile
	if batchAddrFile == "" {
		if _, err := os.Stat("addresses.txt"); err == nil {
			batchAddrFile = "addresses.txt"
		}
	}

	start := time.Now()

	// Process cards in parallel with worker pool
	results := make([]SingleResponse, len(req.Cards))
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup

	for i, cardLine := range req.Cards {
		wg.Add(1)
		sem <- struct{}{}

		go func(idx int, card string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					results[idx] = SingleResponse{
						Status:    "error",
						Code:      "INTERNAL_ERROR",
						Error:     fmt.Sprintf("worker panic: %v", r),
						ErrorType: "unknown",
					}
				}
			}()

			results[idx] = processSingleMultiSite(
				card,
				req.Sites,
				proxyStr,
				req.Proxies,
				batchAddrFile,
				req.ProductCache,
				req.TestSitesBlacklist,
			)
		}(i, cardLine)
	}

	wg.Wait()

	resp := BatchCheckResponse{
		Results:    results,
		TotalCards: len(req.Cards),
		Elapsed:    time.Since(start).Seconds(),
	}

	writeJSON(w, http.StatusOK, resp)
}
func handleGetWorkingSites(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed, use GET"})
		return
	}

	sites := loadSites("")
	
	if len(sites) == 0 {
		log.Printf("No working sites found")
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no working sites found. Set SITES_API_BASE_URL or pass sites in POST /api/check"})
		return
	}

	writeJSON(w, http.StatusOK, sites)
}


// ─── Middleware ───────────────────────────────────────────────────────────────

// corsMiddleware adds CORS headers to allow cross-origin requests
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs incoming requests (skipped for /shopify — that path
// prints only the compact legacy JSON response).
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shopify" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		log.Printf("[API] %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
		log.Printf("[API] %s %s completed in %.2fs", r.Method, r.URL.Path, time.Since(start).Seconds())
	})
}

// ─── Server setup ────────────────────────────────────────────────────────────

func runAPIServer() {
	port := os.Getenv("PORT") // Railway sets PORT automatically
	if port == "" {
		port = os.Getenv("GOCHECK_PORT")
	}
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()

	// Routes
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/shopify", handleShopifyLegacy)
	mux.HandleFunc("/api/check", handleCheckSingle)
	mux.HandleFunc("/api/check/batch", handleCheckBatch)
	mux.HandleFunc("/sites/working", handleGetWorkingSites)
	mux.HandleFunc("/stripe/check", stripe.HandleStripeCheck)
	mux.HandleFunc("/stripe/batch", stripe.HandleStripeBatch)

	// Apply middleware
	handler := corsMiddleware(loggingMiddleware(mux))

	addr := ":" + port
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println("  GoCheck API Server")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  Listening on http://0.0.0.0%s\n", addr)
	fmt.Println()
	fmt.Println("  Endpoints:")
	fmt.Println("    GET  /health           - Health check")
	fmt.Println("    GET  /shopify          - Legacy Shopify check (site, cc, proxy query params)")
	fmt.Println("    POST /api/check        - Check a single card")
	fmt.Println("    POST /api/check/batch  - Check multiple cards in parallel")
	fmt.Println()
	fmt.Println(strings.Repeat("─", 70))
	fmt.Println()
	fmt.Println("  Example (single card):")
	fmt.Println(`    curl -X POST http://localhost:` + port + `/api/check \`)
	fmt.Println(`      -H "Content-Type: application/json" \`)
	fmt.Println(`      -d '{`)
	fmt.Println(`        "card": "4111111111111111|12|2028|123",`)
	fmt.Println(`        "sites": ["https://example-shop.com"],`)
	fmt.Println(`        "proxies": ["http://user:pass@ip:port"]`)
	fmt.Println(`      }'`)
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))

	log.Fatal(http.ListenAndServe(addr, handler))
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v) //nolint:errcheck
}
