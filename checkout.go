package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
)

// ─── Decompression helper ────────────────────────────────────────────────────

func decompressBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
	case "gzip":
		gr, err := gzip.NewReader(resp.Body)
		if err == nil {
			resp.Body = gr
		}
	case "br":
		resp.Body = io.NopCloser(brotli.NewReader(resp.Body))
	case "deflate":
		resp.Body = flate.NewReader(resp.Body)
	}
}

// ─── Test / Bogus Gateway Detection ─────────────────────────────────────────

var testModePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bogus\s*gateway`),
	regexp.MustCompile(`(?i)test\s*mode`),
	regexp.MustCompile(`(?i)payments?.*\(test\s*mode\)`),
	regexp.MustCompile(`(?i)"paymentGateway"\s*:\s*"bogus"`),
	regexp.MustCompile(`(?i)"testMode"\s*:\s*true`),
	regexp.MustCompile(`(?i)"is_test"\s*:\s*true`),
	regexp.MustCompile(`(?i)data-test-mode\s*=\s*["']true`),
	regexp.MustCompile(`(?i)shopify.*payments?.*test`),
	regexp.MustCompile(`(?i)"provider"\s*:\s*"bogus"`),
}

func detectTestMode(body string) bool {
	if len(body) == 0 {
		return false
	}
	sample := body
	if len(sample) > 50000 {
		sample = sample[:50000]
	}
	for _, pat := range testModePatterns {
		if pat.MatchString(sample) {
			return true
		}
	}
	return false
}

func isBogusGatewayName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	return detectTestMode(name)
}

// ─── HTML Extraction Helpers (V2 — from newworking_main.go) ─────────────────

func extractStableID(checkoutHTML string) string {
	unescaped := html.UnescapeString(checkoutHTML)
	re := regexp.MustCompile(`"stableId"\s*:\s*"([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})"`)
	m := re.FindStringSubmatch(unescaped)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractCommitSha(checkoutHTML string) string {
	unescaped := html.UnescapeString(checkoutHTML)
	re := regexp.MustCompile(`"commitSha"\s*:\s*"([a-f0-9]{40})"`)
	m := re.FindStringSubmatch(unescaped)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSourceToken(checkoutHTML string) string {
	re := regexp.MustCompile(`<meta\s+name="serialized-sourceToken"\s+content="([^"]*)"`)
	m := re.FindStringSubmatch(checkoutHTML)
	if len(m) < 2 {
		return ""
	}
	val := html.UnescapeString(m[1])
	return strings.Trim(val, `"`)
}

func extractIdentificationSignature(checkoutHTML string) string {
	unescaped := html.UnescapeString(checkoutHTML)
	re := regexp.MustCompile(`checkoutCardsinkCallerIdentificationSignature":"([^"]+)"`)
	m := re.FindStringSubmatch(unescaped)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractPrivateAccessTokenID(checkoutHTML string) string {
	unescaped := html.UnescapeString(checkoutHTML)
	re := regexp.MustCompile(`"checkoutSessionIdentifier"\s*:\s*"([a-f0-9]+)"`)
	m := re.FindStringSubmatch(unescaped)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractActionsJSURL(checkoutHTML, shopURL string) string {
	re := regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/actions[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.js)`)
	m := re.FindStringSubmatch(checkoutHTML)
	if len(m) < 2 {
		return ""
	}
	return shopURL + m[1]
}

func extractProcessingJSURL(checkoutHTML, shopURL string) string {
	// PollForReceipt persisted query ID may live in several checkout bundles.
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/hydrate[A-Za-z0-9_.-]*\.js)`),
		regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/hooks-useHasOrdersFromMultipleShops[A-Za-z0-9_.-]*\.js)`),
		regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/useHasOrdersFromMultipleShops[A-Za-z0-9_.-]*\.js)`),
		regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/page-Processing[A-Za-z0-9_.-]*\.js)`),
	}
	for _, re := range patterns {
		if m := re.FindStringSubmatch(checkoutHTML); len(m) > 1 {
			return shopURL + m[1]
		}
	}
	return ""
}

func extractPollForReceiptJSURLs(checkoutHTML, shopURL string) []string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/hydrate[A-Za-z0-9_.-]*\.js)`),
		regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/hooks-useHasOrdersFromMultipleShops[A-Za-z0-9_.-]*\.js)`),
		regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/useHasOrdersFromMultipleShops[A-Za-z0-9_.-]*\.js)`),
		regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/page-Processing[A-Za-z0-9_.-]*\.js)`),
		regexp.MustCompile(`(/cdn/shopifycloud/checkout-web/assets/c1/[^"']+\.js)`),
	}
	seen := make(map[string]bool)
	var urls []string
	for _, re := range patterns {
		for _, m := range re.FindAllStringSubmatch(checkoutHTML, -1) {
			if len(m) < 2 {
				continue
			}
			u := shopURL + m[1]
			if !seen[u] {
				seen[u] = true
				urls = append(urls, u)
			}
		}
	}
	return urls
}

func extractProposalID(jsBody string) string {
	re := regexp.MustCompile(`id:\s*"([a-f0-9]{64})"\s*,\s*type:\s*"query"\s*,\s*name:\s*"Proposal"`)
	m := re.FindStringSubmatch(jsBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSubmitForCompletionID(jsBody string) string {
	re := regexp.MustCompile(`id:\s*"([a-f0-9]{64})"\s*,\s*type:\s*"mutation"\s*,\s*name:\s*"SubmitForCompletion"`)
	m := re.FindStringSubmatch(jsBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractPollForReceiptID(jsBody string) string {
	re := regexp.MustCompile(`id:\s*"([a-f0-9]{64})"\s*,\s*type:\s*"query"\s*,\s*name:\s*"PollForReceipt"`)
	m := re.FindStringSubmatch(jsBody)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// ─── Proposal Response Extraction Helpers ────────────────────────────────────

func extractQueueTokenStr(body string) string {
	re := regexp.MustCompile(`"queueToken"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractDeliveryHandleStr(body string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`"selectedDeliveryStrategy"\s*:\s*\{"handle"\s*:\s*"([^"]+)"\s*,\s*"__typename"\s*:\s*"CompleteDeliveryStrategy"`),
		regexp.MustCompile(`"handle"\s*:\s*"([^"]+)"\s*,\s*"phoneRequired"`),
		regexp.MustCompile(`"handle"\s*:\s*"([^"]+?)"\s*,\s*"[^"]*"\s*:\s*(?:true|false)\s*,\s*"amount"`),
		regexp.MustCompile(`"availableDeliveryStrategies"\s*:\s*\[\s*\{\s*"handle"\s*:\s*"([^"]+)"`),
	}
	for _, re := range patterns {
		if m := re.FindStringSubmatch(body); len(m) >= 2 {
			return m[1]
		}
	}
	return ""
}

func (cs *CheckoutSession) populateShippingFromProposalBody(body string) {
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		cs.ShippingHandle = extractDeliveryHandleStr(body)
		return
	}

	result := navigateMap(resp, "data", "session", "negotiate", "result")
	sp := getMap(result, "sellerProposal")
	dt := getMap(sp, "delivery")

	if _, ok := sp["isShippingRequired"]; ok {
		cs.ShippingRequired = getBool(sp, "isShippingRequired")
	}

	cs.ShippingHandle = ""
	shippingAmt := ""
	if getString(dt, "__typename") == "FilledDeliveryTerms" {
		lines := getSlice(dt, "deliveryLines")
		if len(lines) > 0 {
			firstLine, _ := lines[0].(map[string]any)

			if sel := getMap(firstLine, "selectedDeliveryStrategy"); sel != nil {
				cs.ShippingHandle = getString(sel, "handle")
				if amt := moneyAmountFromConstraint(getMap(sel, "amount")); amt != "" {
					shippingAmt = amt
				}
			}

			// Selected strategy in responses often only has handle — resolve
			// amount (and handle if missing) from availableDeliveryStrategies.
			for _, s := range getSlice(firstLine, "availableDeliveryStrategies") {
				sm, _ := s.(map[string]any)
				h := getString(sm, "handle")
				if h == "" {
					continue
				}
				if cs.ShippingHandle == "" {
					cs.ShippingHandle = h
				}
				if cs.ShippingHandle != "" && h != cs.ShippingHandle {
					continue
				}
				if amt := moneyAmountFromConstraint(getMap(sm, "amount")); amt != "" {
					shippingAmt = amt
				}
				if shippingAmt == "" {
					if amt := moneyAmountFromConstraint(getMap(sm, "amountAfterDiscounts")); amt != "" {
						shippingAmt = amt
					}
				}
				break
			}
		}
	}
	if cs.ShippingHandle == "" {
		cs.ShippingHandle = extractDeliveryHandleStr(body)
	}

	if shippingAmt == "" {
		shippingAmt = extractShippingAmountStr(body)
	}
	if shippingAmt != "" {
		cs.ShippingAmount = shippingAmt
	} else if cs.ShippingAmount == "" {
		cs.ShippingAmount = "0.00"
	}

	de := getMap(sp, "deliveryExpectations")
	if getString(de, "__typename") == "FilledDeliveryExpectationTerms" {
		var exps []map[string]string
		for _, item := range getSlice(de, "deliveryExpectations") {
			em, _ := item.(map[string]any)
			if sh := getString(em, "signedHandle"); sh != "" {
				exps = append(exps, map[string]string{"signedHandle": sh})
			}
		}
		if len(exps) > 0 {
			cs.DeliveryExps = exps
		}
	}
	if len(cs.DeliveryExps) == 0 {
		if signedHandles := extractSignedHandlesStr(body); len(signedHandles) > 0 {
			cs.DeliveryExps = nil
			for _, sh := range signedHandles {
				cs.DeliveryExps = append(cs.DeliveryExps, map[string]string{"signedHandle": sh})
			}
		}
	}

	totalAmount := ""
	if sp != nil {
		if ct := getMap(sp, "checkoutTotal"); ct != nil {
			if val := getMap(ct, "value"); val != nil {
				totalAmount = getString(val, "amount")
				if cur := getString(val, "currencyCode"); cur != "" {
					cs.CurrencyCode = cur
				}
			}
		}
		if totalAmount == "" {
			if pay := getMap(sp, "payment"); pay != nil {
				if ta := getMap(pay, "totalAmount"); ta != nil {
					if val := getMap(ta, "value"); val != nil {
						totalAmount = getString(val, "amount")
						if cur := getString(val, "currencyCode"); cur != "" {
							cs.CurrencyCode = cur
						}
					}
				}
			}
		}
	}
	if totalAmount == "" {
		totalAmount = extractCheckoutTotalStr(body)
	}
	if totalAmount == "" {
		totalAmount = extractSellerTotalStr(body)
	}
	if totalAmount != "" {
		cs.ActualTotal = totalAmount
	}

	if merch := getMap(sp, "merchandise"); getString(merch, "__typename") == "FilledMerchandiseTerms" {
		for _, line := range getSlice(merch, "merchandiseLines") {
			lm, _ := line.(map[string]any)
			if sid := getString(lm, "stableId"); sid != "" {
				if sid != cs.StableID {
					logCheckout(formatSiteLabel(cs.ShopURL), "Step3",
						"synced stableId %s -> %s", truncate(cs.StableID, 12), truncate(sid, 12))
				}
				cs.StableID = sid
				break
			}
		}
	}

	if gw := extractPaymentGateway(body); gw != "" {
		cs.PaymentGateway = gw
		fmt.Printf("  Payment gateway: %s\n", gw)
	}
}

func moneyAmountFromConstraint(amt map[string]any) string {
	if amt == nil {
		return ""
	}
	if getString(amt, "__typename") == "MoneyValueConstraint" || getMap(amt, "value") != nil {
		return getString(getMap(amt, "value"), "amount")
	}
	return ""
}

func extractPaymentGateway(body string) string {
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return ""
	}

	result := navigateMap(resp, "data", "session", "negotiate", "result")
	sp := getMap(result, "sellerProposal")
	payment := getMap(sp, "payment")
	lines := getSlice(payment, "availablePaymentLines")

	var fallback string
	for _, line := range lines {
		lm, ok := line.(map[string]any)
		if !ok {
			continue
		}
		pm := getMap(lm, "paymentMethod")
		if getString(pm, "__typename") != "PaymentProvider" {
			continue
		}
		name := paymentProviderDisplayName(pm)
		if name == "" {
			continue
		}
		if getBool(pm, "alternative") {
			if fallback == "" {
				fallback = name
			}
			continue
		}
		return name
	}
	return fallback
}

func paymentProviderDisplayName(pm map[string]any) string {
	if pm == nil {
		return ""
	}
	if name := strings.TrimSpace(getString(pm, "extensibilityDisplayName")); name != "" {
		return name
	}
	if name := strings.TrimSpace(getString(pm, "displayName")); name != "" {
		return name
	}
	return formatGatewayName(getString(pm, "name"))
}

func formatGatewayName(raw string) string {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "_", " "))
	if raw == "" {
		return ""
	}
	parts := strings.Fields(raw)
	for i, p := range parts {
		if len(p) == 0 {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, " ")
}

func extractSignedHandlesStr(body string) []string {
	re := regexp.MustCompile(`"signedHandle"\s*:\s*"([^"]+)"`)
	matches := re.FindAllStringSubmatch(body, -1)
	var handles []string
	for _, m := range matches {
		if len(m) >= 2 {
			handles = append(handles, m[1])
		}
	}
	return handles
}

func extractShippingAmountStr(body string) string {
	re := regexp.MustCompile(`"deliveryStrategyBreakdown"\s*:\s*\[\s*\{\s*"amount"\s*:\s*\{\s*"value"\s*:\s*\{\s*"amount"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractCheckoutTotalStr(body string) string {
	re := regexp.MustCompile(`"checkoutTotal"\s*:\s*\{[^}]*"value"\s*:\s*\{\s*"amount"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSellerTotalStr(body string) string {
	re := regexp.MustCompile(`"total"\s*:\s*\{\s*"value"\s*:\s*\{\s*"amount"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSellerCurrencyStr(body string) string {
	re := regexp.MustCompile(`"supportedCurrencies"\s*:\s*\["([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSellerCountryStr(body string) string {
	re := regexp.MustCompile(`"supportedCountries"\s*:\s*\["([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func extractSellerMerchandisePriceStr(body string) string {
	re := regexp.MustCompile(`"ContextualizedProductVariantMerchandise".*?"totalAmount"\s*:\s*\{\s*"value"\s*:\s*\{\s*"amount"\s*:\s*"([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// ─── Token / ID Generation ──────────────────────────────────────────────────

func generateAttemptToken(checkoutToken string) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return checkoutToken + "-" + string(b)
}

func generatePageID() string {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(rand.IntN(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ─── GraphQL Headers Builder (V2 — matches newworking_main.go) ──────────────

func (cs *CheckoutSession) graphqlHeaders() http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Accept-Language", "en-US")
	h.Set("Content-Type", "application/json")
	h.Set("Origin", cs.ShopURL)
	h.Set("Priority", "u=1, i")
	if cs.CheckoutURL != "" {
		h.Set("Referer", cs.CheckoutURL)
	}
	h.Set("Sec-CH-UA", cs.FP.SecCHUA)
	h.Set("Sec-CH-UA-Mobile", cs.FP.SecCHUAMobile)
	h.Set("Sec-CH-UA-Platform", cs.FP.SecCHUAPlatform)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("User-Agent", cs.FP.UserAgent)
	h.Set("shopify-checkout-client", "checkout-web/1.0")
	h.Set("shopify-checkout-source", fmt.Sprintf(`id="%s", type="cn"`, cs.CheckoutToken))
	h.Set("x-checkout-one-session-token", cs.SessionToken)
	if cs.BuildID != "" {
		h.Set("x-checkout-web-build-id", cs.BuildID)
	}
	h.Set("x-checkout-web-deploy-stage", "production")
	h.Set("x-checkout-web-server-handling", "fast")
	h.Set("x-checkout-web-server-rendering", "yes")
	if cs.SourceToken != "" {
		h.Set("x-checkout-web-source-id", cs.SourceToken)
	}
	return h
}

// graphqlHeadersPoll is like graphqlHeaders but uses CheckoutToken for source-id (matches new file polling)
func (cs *CheckoutSession) graphqlHeadersPoll() http.Header {
	h := cs.graphqlHeaders()
	h.Set("x-checkout-web-source-id", cs.CheckoutToken)
	return h
}

// ─── Session warming ─────────────────────────────────────────────────────────
// V2: Minimal — cart add + GET /checkout establishes session.

func (cs *CheckoutSession) WarmStorefrontSession() {
	if cfg.FastMode {
		return
	}
	req, err := http.NewRequest("GET", cs.ShopURL, nil)
	if err == nil {
		setBrowseHeaders(req, cs.FP, cs.ShopURL)
		resp, err := cs.Client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
	time.Sleep(checkoutDelay(200, 500))
}

// ─── Step 1: Add to Cart + Checkout + Extract All IDs + Private Token + JS IDs ─

const cartCreateMutation = `mutation cartCreate($input: CartInput!) {
  cartCreate(input: $input) {
    cart { id checkoutUrl }
    userErrors { message }
  }
}`

func mergeClientCookies(dst, src *http.Client, urls ...string) {
	if dst == nil || src == nil || dst.Jar == nil || src.Jar == nil {
		return
	}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if cookies := src.Jar.Cookies(u); len(cookies) > 0 {
			dst.Jar.SetCookies(u, cookies)
		}
	}
}

func (cs *CheckoutSession) ensureStorefrontCreds() {
	if cs.StorefrontToken != "" {
		return
	}
	tokenClient := newStandardClient(cs.ProxyURL, cs.Client.Timeout)
	token, gqlURL := fetchStorefrontAccessToken(tokenClient, cs.ShopURL, cs.FP)
	cs.StorefrontToken = token
	if gqlURL != "" {
		cs.GqlShopURL = gqlURL
	}
}

func (cs *CheckoutSession) cartCreateCheckoutURL(variantID string) (string, error) {
	cs.ensureStorefrontCreds()
	gqlBase := cs.GqlShopURL
	if gqlBase == "" {
		gqlBase = cs.ShopURL
	}
	if cs.StorefrontToken == "" {
		return "", fmt.Errorf("no storefront token")
	}

	payload := map[string]any{
		"query": cartCreateMutation,
		"variables": map[string]any{
			"input": map[string]any{
				"lines": []map[string]any{{
					"merchandiseId": "gid://shopify/ProductVariant/" + variantID,
					"quantity":      1,
				}},
			},
		},
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	tokenClient := newStandardClient(cs.ProxyURL, cs.Client.Timeout)
	req, err := http.NewRequest("POST", gqlBase+"/api/unstable/graphql.json", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	setAPIHeaders(req, cs.FP, gqlBase)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Storefront-Access-Token", cs.StorefrontToken)

	resp, err := tokenClient.Do(req)
	if err != nil {
		return "", err
	}
	respBody, _ := readRespBody(resp)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("cartCreate HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 120))
	}

	var parsed struct {
		Data struct {
			CartCreate struct {
				Cart struct {
					CheckoutURL string `json:"checkoutUrl"`
				} `json:"cart"`
				UserErrors []struct {
					Message string `json:"message"`
				} `json:"userErrors"`
			} `json:"cartCreate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("cartCreate parse: %w", err)
	}
	if len(parsed.Errors) > 0 {
		return "", fmt.Errorf("cartCreate gql: %s", parsed.Errors[0].Message)
	}
	if len(parsed.Data.CartCreate.UserErrors) > 0 {
		return "", fmt.Errorf("cartCreate: %s", parsed.Data.CartCreate.UserErrors[0].Message)
	}
	checkoutURL := parsed.Data.CartCreate.Cart.CheckoutURL
	if checkoutURL == "" {
		return "", fmt.Errorf("cartCreate: empty checkoutUrl")
	}

	mergeClientCookies(cs.Client, tokenClient, cs.ShopURL, gqlBase, checkoutURL)
	logDebug("CART", "%s | cartCreate OK variant=%s checkout=%s", formatSiteLabel(cs.ShopURL), variantID, truncate(checkoutURL, 60))
	return checkoutURL, nil
}

func (cs *CheckoutSession) Step1AddToCart() error {
	fmt.Println("[1/5] Storefront GraphQL cartCreate + checkout...")

	variantIDs := append([]string{cs.VariantID}, cs.AltVariantIDs...)
	var lastCartErr string
	var checkoutEntryURL string

	for _, variantID := range variantIDs {
		if variantID == "" {
			continue
		}


		checkoutURL, err := cs.cartCreateCheckoutURL(variantID)
		if err != nil {
			lastCartErr = err.Error()
			fmt.Printf("  cartCreate: %s variant=%s\n", lastCartErr, variantID)
			continue
		}
		cs.VariantID = variantID
		checkoutEntryURL = checkoutURL
		fmt.Printf("  cartCreate: OK variant=%s\n", variantID)
		break
	}

	if checkoutEntryURL == "" {
		if lastCartErr != "" {
			return fmt.Errorf("cartCreate failed: %s", lastCartErr)
		}
		return fmt.Errorf("cartCreate failed: no variant to add")
	}



	// ── GET checkout (follows redirects) ──
	checkoutReq, err := http.NewRequest("GET", checkoutEntryURL, nil)
	if err != nil {
		return fmt.Errorf("checkout build: %w", err)
	}
	setBrowseHeaders(checkoutReq, cs.FP, cs.ShopURL)

	checkoutResp, err := cs.Client.Do(checkoutReq)
	if err != nil {
		return fmt.Errorf("checkout GET: %w", err)
	}
	decompressBody(checkoutResp)
	defer checkoutResp.Body.Close()

	bodyBytes, _ := io.ReadAll(checkoutResp.Body)
	checkoutHTML := string(bodyBytes)
	finalURL := checkoutResp.Request.URL.String()

	// ── Test/bogus gateway detection ──
	if detectTestMode(checkoutHTML) {
		return fmt.Errorf("TEST_MODE_DETECTED")
	}

	// ── Extract checkout token from URL ──
	tokenRe := regexp.MustCompile(`/checkouts/cn/([^/?]+)`)
	if m := tokenRe.FindStringSubmatch(finalURL); len(m) > 1 {
		cs.CheckoutToken = m[1]
	}
	if cs.CheckoutToken == "" {
		if m := regexp.MustCompile(`/cart/c/([^/?]+)`).FindStringSubmatch(finalURL); len(m) > 1 {
			cs.CheckoutToken = m[1]
		}
	}
	if cs.CheckoutToken == "" {
		return fmt.Errorf("no checkout token in URL: %s", finalURL)
	}
	cs.CheckoutURL = finalURL
	fmt.Printf("  Token: %s\n", cs.CheckoutToken)

	// ── Extract session token (response header first, then HTML) ──
	cs.SessionToken = checkoutResp.Header.Get("x-checkout-one-session-token")
	if cs.SessionToken == "" {
		cs.SessionToken = checkoutResp.Header.Get("X-Checkout-One-Session-Token")
	}
	if cs.SessionToken == "" {
		cs.SessionToken = extractSessionToken(checkoutHTML)
	}
	if cs.SessionToken == "" {
		return fmt.Errorf("session token not found in HTML")
	}

	// ── Extract stableId, commitSha, sourceToken, identificationSignature ──
	cs.StableID = extractStableID(checkoutHTML)
	cs.MerchandiseID = cs.StableID
	cs.BuildID = extractCommitSha(checkoutHTML)
	cs.SourceToken = extractSourceToken(checkoutHTML)
	cs.IdentificationSignature = extractIdentificationSignature(checkoutHTML)

	if cs.BuildID == "" {
		cs.BuildID = extractBuildID(checkoutHTML)
	}
	if cs.StableID == "" {
		return fmt.Errorf("stableId not found in checkout HTML")
	}
	if cs.SourceToken == "" {
		return fmt.Errorf("sourceToken not found in checkout HTML")
	}
	fmt.Printf("  StableID: %s BuildID: %s...\n", truncate(cs.StableID, 12), truncate(cs.BuildID, 12))

	// ── Fetch private access token (sets session cookies) ──
	patID := extractPrivateAccessTokenID(checkoutHTML)
	if patID != "" {
		patURL := fmt.Sprintf("%s/private_access_tokens?id=%s&checkout_type=c1",
			cs.ShopURL, url.QueryEscape(patID))
		patReq, err := http.NewRequest("GET", patURL, nil)
		if err == nil {
			patReq.Header.Set("Accept", "*/*")
			patReq.Header.Set("Accept-Language", "en-US,en;q=0.9")
			patReq.Header.Set("Referer", cs.CheckoutURL)
			patReq.Header.Set("Sec-CH-UA", cs.FP.SecCHUA)
			patReq.Header.Set("Sec-CH-UA-Mobile", cs.FP.SecCHUAMobile)
			patReq.Header.Set("Sec-CH-UA-Platform", cs.FP.SecCHUAPlatform)
			patReq.Header.Set("Sec-Fetch-Dest", "empty")
			patReq.Header.Set("Sec-Fetch-Mode", "cors")
			patReq.Header.Set("Sec-Fetch-Site", "same-origin")
			patReq.Header.Set("User-Agent", cs.FP.UserAgent)
			patResp, err := cs.Client.Do(patReq)
			if err == nil {
				io.Copy(io.Discard, patResp.Body)
				patResp.Body.Close()
			}
		}
		fmt.Println("  Private access token: done")
	}

	// ── Extract JS URLs and fetch GraphQL operation IDs ──
	actionsURL := extractActionsJSURL(checkoutHTML, cs.ShopURL)
	if actionsURL == "" {
		return fmt.Errorf("actions JS URL not found")
	}
	processingURL := extractProcessingJSURL(checkoutHTML, cs.ShopURL)

	var actionsJS, processingJS string
	var actionsErr, processingErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		actionsJS, actionsErr = cs.fetchJS(actionsURL)
	}()
	if processingURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			processingJS, processingErr = cs.fetchJS(processingURL)
		}()
	}
	wg.Wait()
	if actionsErr != nil {
		return fmt.Errorf("fetch actions JS: %w", actionsErr)
	}

	cs.ProposalID = extractProposalID(actionsJS)
	cs.SubmitID = extractSubmitForCompletionID(actionsJS)
	if cs.ProposalID == "" || cs.SubmitID == "" {
		return fmt.Errorf("proposalID or submitID not found in actions JS")
	}

	cs.PollForReceiptID = extractPollForReceiptID(actionsJS)
	if cs.PollForReceiptID == "" && processingErr == nil && processingJS != "" {
		cs.PollForReceiptID = extractPollForReceiptID(processingJS)
	}
	if cs.PollForReceiptID == "" {
		for _, jsURL := range extractPollForReceiptJSURLs(checkoutHTML, cs.ShopURL) {
			jsBody, err := cs.fetchJS(jsURL)
			if err != nil {
				continue
			}
			if id := extractPollForReceiptID(jsBody); id != "" {
				cs.PollForReceiptID = id
				break
			}
		}
	}
	if cs.PollForReceiptID == "" {
		return fmt.Errorf("PollForReceipt ID not found in JS")
	}

	fmt.Printf("  Proposal: %s... Submit: %s... Poll: %s...\n",
		truncate(cs.ProposalID, 12), truncate(cs.SubmitID, 12), truncate(cs.PollForReceiptID, 12))

	return nil
}

// fetchJS fetches a JS file from the given URL
func (cs *CheckoutSession) fetchJS(jsURL string) (string, error) {
	req, err := http.NewRequest("GET", jsURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", cs.ShopURL)
	req.Header.Set("Sec-CH-UA", cs.FP.SecCHUA)
	req.Header.Set("Sec-CH-UA-Mobile", cs.FP.SecCHUAMobile)
	req.Header.Set("Sec-CH-UA-Platform", cs.FP.SecCHUAPlatform)
	req.Header.Set("Sec-Fetch-Dest", "script")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("User-Agent", cs.FP.UserAgent)

	resp, err := cs.Client.Do(req)
	if err != nil {
		return "", err
	}
	decompressBody(resp)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("JS fetch HTTP %d", resp.StatusCode)
	}
	return string(body), nil
}

// ─── Step 2: Tokenize card via PCI with identification signature ─────────────

func (cs *CheckoutSession) Step2TokenizeCard() error {
	fmt.Println("[2/5] Tokenizing card...")



	parsed, _ := url.Parse(cs.ShopURL)
	scopeHost := parsed.Host

	payload, _ := json.Marshal(map[string]any{
		"credit_card": map[string]any{
			"number":             cs.Card.Number,
			"month":              cs.Card.Month,
			"year":               cs.Card.Year,
			"verification_value": cs.Card.CVV,
			"start_month":        nil,
			"start_year":         nil,
			"issue_number":       "",
			"name":               cs.Card.Name,
		},
		"payment_session_scope": scopeHost,
	})

	endpoint := "https://checkout.pci.shopifyinc.com/sessions"

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("tokenize build: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://checkout.pci.shopifyinc.com")
	req.Header.Set("Referer", "https://checkout.pci.shopifyinc.com/build/a8e4a94/number-ltr.html?identifier=&locationURL=")
	req.Header.Set("Priority", "u=1, i")
	req.Header.Set("Sec-CH-UA", cs.FP.SecCHUA)
	req.Header.Set("Sec-CH-UA-Mobile", cs.FP.SecCHUAMobile)
	req.Header.Set("Sec-CH-UA-Platform", cs.FP.SecCHUAPlatform)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Storage-Access", "active")
	req.Header.Set("User-Agent", cs.FP.UserAgent)
	if cs.IdentificationSignature != "" {
		req.Header.Set("shopify-identification-signature", cs.IdentificationSignature)
	}

	tokenClient := newStandardClient(cs.ProxyURL, cfg.HTTPTimeoutShort)

	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := checkoutDelay(200, 500)
			if attempt > 1 {
				backoff += time.Duration(attempt) * 300 * time.Millisecond
			}
			fmt.Printf("  [RETRY] tokenize attempt %d/3\n", attempt+1)
			time.Sleep(backoff)
			req, _ = http.NewRequest("POST", endpoint, bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Accept-Language", "en-US,en;q=0.9")
			req.Header.Set("Origin", "https://checkout.pci.shopifyinc.com")
			req.Header.Set("Referer", "https://checkout.pci.shopifyinc.com/build/a8e4a94/number-ltr.html?identifier=&locationURL=")
			req.Header.Set("Sec-CH-UA", cs.FP.SecCHUA)
			req.Header.Set("Sec-CH-UA-Mobile", cs.FP.SecCHUAMobile)
			req.Header.Set("Sec-CH-UA-Platform", cs.FP.SecCHUAPlatform)
			req.Header.Set("Sec-Fetch-Dest", "empty")
			req.Header.Set("Sec-Fetch-Mode", "cors")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.Header.Set("Sec-Fetch-Storage-Access", "active")
			req.Header.Set("User-Agent", cs.FP.UserAgent)
			if cs.IdentificationSignature != "" {
				req.Header.Set("shopify-identification-signature", cs.IdentificationSignature)
			}
		}
		resp, err = tokenClient.Do(req)
		if err != nil {
			return fmt.Errorf("tokenize POST: %w", err)
		}
		if resp.StatusCode == 429 {
			resp.Body.Close()
			if attempt == 2 {
				return fmt.Errorf("tokenization rate limited: 429 (after 3 attempts)")
			}
			continue
		}
		break
	}
	defer resp.Body.Close()

	if resp.StatusCode == 403 {
		return fmt.Errorf("tokenization blocked: 403 Forbidden (proxy/IP blocked)")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("tokenization HTTP %d", resp.StatusCode)
	}

	var tokenResp struct {
		ID     string `json:"id"`
		Errors any    `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("tokenize JSON decode: %w", err)
	}
	if tokenResp.ID == "" {
		return fmt.Errorf("no card session ID (errors: %v)", tokenResp.Errors)
	}

	cs.CardSessionID = tokenResp.ID
	fmt.Printf("  PCI session: %s\n", tokenResp.ID)
	return nil
}

// ─── Proposal Helpers ────────────────────────────────────────────────────────

// patchPayload replaces hardcoded USD/US with detected currency/country
func (cs *CheckoutSession) patchPayload(payload string) string {
	currency := cs.CurrencyCode
	if currency == "" {
		currency = "USD"
	}
	country := cs.DetectedCountry
	if country == "" {
		country = cs.Addr.Country
	}
	if country == "" {
		country = "US"
	}

	if currency != "USD" {
		payload = strings.ReplaceAll(payload, `"currencyCode": "USD"`, `"currencyCode": "`+currency+`"`)
		payload = strings.ReplaceAll(payload, `"presentmentCurrency": "USD"`, `"presentmentCurrency": "`+currency+`"`)
	}
	if country != "US" {
		payload = strings.ReplaceAll(payload, `"phoneCountryCode": "US"`, `"phoneCountryCode": "`+country+`"`)
	}
	return payload
}

// sendProposalRaw sends a raw proposal payload and returns body
func (cs *CheckoutSession) proposalExpectedShippingPriceJSON() string {
	if cs.ShippingHandle == "" {
		return `{"any": true}`
	}
	currency := cs.CurrencyCode
	if currency == "" {
		currency = "USD"
	}
	amount := cs.ShippingAmount
	if amount == "" {
		amount = "0.0"
	}
	return fmt.Sprintf(`{"value": {"amount": %s, "currencyCode": %s}}`,
		strconv.Quote(amount), strconv.Quote(currency))
}

func (cs *CheckoutSession) sendProposalRaw(payload string) (string, error) {
	gqlURL := cs.ShopURL + "/checkouts/internal/graphql/persisted?operationName=Proposal"

	req, err := http.NewRequest("POST", gqlURL, strings.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("proposal build: %w", err)
	}
	req.Header = cs.graphqlHeaders()

	var body []byte
	for attempt := range 3 {
		if attempt > 0 {
			wait := 0.5 + float64(attempt)*0.5
			if cfg.FastMode {
				wait = 0.3 + float64(attempt)*0.3
			}
			time.Sleep(time.Duration(wait * float64(time.Second)))
			req, _ = http.NewRequest("POST", gqlURL, strings.NewReader(payload))
			req.Header = cs.graphqlHeaders()
		}
		resp, err := cs.Client.Do(req)
		if err != nil {
			if attempt < 2 {
				continue
			}
			return "", fmt.Errorf("proposal POST: %w", err)
		}
		decompressBody(resp)
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 429 {
			if attempt < 2 {
				continue
			}
			return "", fmt.Errorf("proposal rate limited: 429")
		}
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("proposal HTTP %d", resp.StatusCode)
		}
		break
	}

	return string(body), nil
}

// ─── Step 3: Five Proposal Rounds ────────────────────────────────────────────

func (cs *CheckoutSession) Step3Proposal() error {
	fmt.Println("[3/5] Proposals...")

	email := cs.Addr.Email

	// ── Round 1: Initial proposal (empty address, no email) ──
	fmt.Println("  [3.1] Initial proposal...")
	payload1 := cs.buildProposal1()
	body1, err := cs.sendProposalRaw(payload1)
	if err != nil {
		return fmt.Errorf("proposal round 1: %w", err)
	}

	if cur := extractSellerCurrencyStr(body1); cur != "" {
		cs.CurrencyCode = cur
	}
	if ctr := extractSellerCountryStr(body1); ctr != "" {
		cs.DetectedCountry = ctr
	}

	qt1 := extractQueueTokenStr(body1)
	if qt1 == "" {
		return fmt.Errorf("queueToken not found in proposal round 1")
	}

	// ── Round 2: Email + address combined ──
	fmt.Println("  [3.2] Address proposal...")
	payload2 := cs.buildProposal3(qt1, email)
	body2, err := cs.sendProposalRaw(payload2)
	if err != nil {
		return fmt.Errorf("proposal round 2: %w", err)
	}
	qt2 := extractQueueTokenStr(body2)
	if qt2 == "" {
		return fmt.Errorf("queueToken not found in proposal round 2")
	}
	cs.populateShippingFromProposalBody(body2)

	// ── Round 3: Lock delivery handle ──
	fmt.Println("  [3.3] Delivery lock...")
	payload3 := cs.buildProposal3(qt2, email)
	body3, err := cs.sendProposalRaw(payload3)
	if err != nil {
		return fmt.Errorf("proposal round 3: %w", err)
	}

	cs.QueueToken = extractQueueTokenStr(body3)
	if cs.QueueToken == "" {
		return fmt.Errorf("queueToken not found in final proposal")
	}
	cs.populateShippingFromProposalBody(body3)
	cs.logDeliveryState("final", body3, payload3)

	// Poll if shipping data is missing OR if total is missing/pending
	needsPoll := (cs.ShippingRequired && (cs.ShippingHandle == "" || len(cs.DeliveryExps) == 0)) ||
		cs.ActualTotal == "" ||
		cs.proposalTermsPending(body3)

	if needsPoll {
		for pollAttempt := 1; pollAttempt <= 3; pollAttempt++ {
			time.Sleep(checkoutDelay(100, 200))
			body, err := cs.sendProposalAndUpdate(cs.QueueToken, email)
			if err != nil {
				break
			}
			if cs.ActualTotal != "" && !cs.proposalTermsPending(body) {
				if cs.ShippingRequired && cs.ShippingHandle == "" {
					continue
				}
				break
			}
		}
	}

	if cs.ShippingRequired && cs.ShippingHandle == "" {
		return fmt.Errorf("no shipping handle obtained")
	}

	fmt.Printf("  Handle: %s Shipping: %s Total: %s Required: %v\n",
		truncate(cs.ShippingHandle, 30), cs.ShippingAmount, cs.ActualTotal, cs.ShippingRequired)

	return nil
}

// buildProposal1 builds the initial proposal payload (no email, no queueToken, empty address)
func (cs *CheckoutSession) buildProposal1() string {
	p := fmt.Sprintf(`{
  "variables": {
    "sessionInput": {"sessionToken": %s},
    "queueToken": null,
    "discounts": {"lines": [], "acceptUnexpectedDiscounts": true},
    "delivery": {
      "deliveryLines": [{
        "destination": {
          "partialStreetAddress": {
            "address1": "", "city": "", "countryCode": "US",
            "lastName": "", "phone": "", "oneTimeUse": false
          }
        },
        "selectedDeliveryStrategy": {
          "deliveryStrategyMatchingConditions": {
            "estimatedTimeInTransit": {"any": true},
            "shipments": {"any": true}
          },
          "options": {}
        },
        "targetMerchandiseLines": {"any": true},
        "deliveryMethodTypes": ["SHIPPING"],
        "expectedTotalPrice": {"any": true},
        "destinationChanged": true
      }],
      "noDeliveryRequired": [],
      "useProgressiveRates": false,
      "prefetchShippingRatesStrategy": null,
      "supportsSplitShipping": true
    },
    "deliveryExpectations": {"deliveryExpectationLines": []},
    "merchandise": {
      "merchandiseLines": [{
        "stableId": %s,
        "merchandise": {
          "productVariantReference": {
            "id": "gid://shopify/ProductVariantMerchandise/%s",
            "variantId": "gid://shopify/ProductVariant/%s",
            "properties": [], "sellingPlanId": null, "sellingPlanDigest": null
          }
        },
        "quantity": {"items": {"value": 1}},
        "expectedTotalPrice": {"any": true},
        "lineComponentsSource": null,
        "lineComponents": []
      }]
    },
    "memberships": {"memberships": []},
    "payment": {
      "totalAmount": {"any": true},
      "paymentLines": [],
      "billingAddress": {
        "streetAddress": {
          "address1": "", "city": "", "countryCode": "US",
          "lastName": "", "phone": ""
        }
      }
    },
    "buyerIdentity": {
      "customer": {"presentmentCurrency": "USD", "countryCode": "US"},
      "phoneCountryCode": "US",
      "marketingConsent": [],
      "shopPayOptInPhone": {"countryCode": "US"},
      "rememberMe": false
    },
    "tip": {"tipLines": []},
    "poNumber": null,
    "taxes": {
      "proposedAllocations": null,
      "proposedTotalAmount": {"any": true},
      "proposedTotalIncludedAmount": null,
      "proposedMixedStateTotalAmount": null,
      "proposedExemptions": []
    },
    "note": {"message": null, "customAttributes": []},
    "localizationExtension": {"fields": []},
    "nonNegotiableTerms": null,
    "scriptFingerprint": {
      "signature": null, "signatureUuid": null,
      "lineItemScriptChanges": [], "paymentScriptChanges": [], "shippingScriptChanges": []
    },
    "optionalDuties": {"buyerRefusesDuties": false},
    "cartMetafields": []
  },
  "operationName": "Proposal",
  "id": %s
}`,
		strconv.Quote(cs.SessionToken),
		strconv.Quote(cs.StableID), cs.VariantID, cs.VariantID,
		strconv.Quote(cs.ProposalID))
	return cs.patchPayload(p)
}

// buildProposal2 builds the email proposal payload (with queueToken + email)
func (cs *CheckoutSession) buildProposal2(queueToken, email string) string {
	p := fmt.Sprintf(`{
  "variables": {
    "sessionInput": {"sessionToken": %s},
    "queueToken": %s,
    "discounts": {"lines": [], "acceptUnexpectedDiscounts": true},
    "delivery": {
      "deliveryLines": [{
        "destination": {
          "partialStreetAddress": {
            "address1": "", "city": "", "countryCode": "US",
            "lastName": "", "phone": "", "oneTimeUse": false
          }
        },
        "selectedDeliveryStrategy": {
          "deliveryStrategyMatchingConditions": {
            "estimatedTimeInTransit": {"any": true},
            "shipments": {"any": true}
          },
          "options": {}
        },
        "targetMerchandiseLines": {"any": true},
        "deliveryMethodTypes": ["SHIPPING"],
        "expectedTotalPrice": {"any": true},
        "destinationChanged": true
      }],
      "noDeliveryRequired": [],
      "useProgressiveRates": false,
      "prefetchShippingRatesStrategy": null,
      "supportsSplitShipping": true
    },
    "deliveryExpectations": {"deliveryExpectationLines": []},
    "merchandise": {
      "merchandiseLines": [{
        "stableId": %s,
        "merchandise": {
          "productVariantReference": {
            "id": "gid://shopify/ProductVariantMerchandise/%s",
            "variantId": "gid://shopify/ProductVariant/%s",
            "properties": [], "sellingPlanId": null, "sellingPlanDigest": null
          }
        },
        "quantity": {"items": {"value": 1}},
        "expectedTotalPrice": {"any": true},
        "lineComponentsSource": null,
        "lineComponents": []
      }]
    },
    "memberships": {"memberships": []},
    "payment": {
      "totalAmount": {"any": true},
      "paymentLines": [],
      "billingAddress": {
        "streetAddress": {
          "address1": "", "city": "", "countryCode": "US",
          "lastName": "", "phone": ""
        }
      }
    },
    "buyerIdentity": {
      "customer": {"presentmentCurrency": "USD", "countryCode": "US"},
      "email": %s,
      "emailChanged": true,
      "phoneCountryCode": "US",
      "marketingConsent": [],
      "shopPayOptInPhone": {"countryCode": "US"},
      "rememberMe": false
    },
    "tip": {"tipLines": []},
    "poNumber": null,
    "taxes": {
      "proposedAllocations": null,
      "proposedTotalAmount": {"any": true},
      "proposedTotalIncludedAmount": null,
      "proposedMixedStateTotalAmount": null,
      "proposedExemptions": []
    },
    "note": {"message": null, "customAttributes": []},
    "localizationExtension": {"fields": []},
    "nonNegotiableTerms": null,
    "scriptFingerprint": {
      "signature": null, "signatureUuid": null,
      "lineItemScriptChanges": [], "paymentScriptChanges": [], "shippingScriptChanges": []
    },
    "optionalDuties": {"buyerRefusesDuties": false},
    "cartMetafields": []
  },
  "operationName": "Proposal",
  "id": %s
}`,
		strconv.Quote(cs.SessionToken), strconv.Quote(queueToken),
		strconv.Quote(cs.StableID), cs.VariantID, cs.VariantID,
		strconv.Quote(email),
		strconv.Quote(cs.ProposalID))
	return cs.patchPayload(p)
}

func (cs *CheckoutSession) buildSelectedDeliveryStrategyJSON() string {
	if cs.ShippingHandle != "" {
		return fmt.Sprintf(`{
          "deliveryStrategyByHandle": {
            "handle": %s,
            "customDeliveryRate": false
          },
          "options": {}
        }`, strconv.Quote(cs.ShippingHandle))
	}
	return `{
          "deliveryStrategyMatchingConditions": {
            "estimatedTimeInTransit": {"any": true},
            "shipments": {"any": true}
          },
          "options": {}
        }`
}

func (cs *CheckoutSession) buildDeliveryExpectationsJSON() string {
	var handleLines []string
	for _, exp := range cs.DeliveryExps {
		if h := exp["signedHandle"]; h != "" {
			handleLines = append(handleLines, fmt.Sprintf(`{"signedHandle":%s}`, strconv.Quote(h)))
		}
	}
	if len(handleLines) == 0 {
		return `{"deliveryExpectationLines": []}`
	}
	return fmt.Sprintf(`{"deliveryExpectationLines": [%s]}`, strings.Join(handleLines, ","))
}

func (cs *CheckoutSession) proposalDeliveryDestinationJSON(addr Address, country, province, phone string) string {
	if cs.ShippingHandle != "" {
		return fmt.Sprintf(`{
          "streetAddress": {
            "address1": %s, "address2": "",
            "city": %s, "countryCode": %s,
            "postalCode": %s, "firstName": %s,
            "lastName": %s, "zoneCode": %s,
            "phone": %s, "oneTimeUse": false
          }
        }`,
			strconv.Quote(addr.Address1), strconv.Quote(addr.City), strconv.Quote(country),
			strconv.Quote(addr.Zip), strconv.Quote(addr.FirstName),
			strconv.Quote(addr.LastName), strconv.Quote(province),
			strconv.Quote(phone))
	}
	return fmt.Sprintf(`{
          "partialStreetAddress": {
            "address1": %s, "address2": "",
            "city": %s, "countryCode": %s,
            "postalCode": %s, "firstName": %s,
            "lastName": %s, "zoneCode": %s,
            "phone": %s, "oneTimeUse": false
          }
        }`,
		strconv.Quote(addr.Address1), strconv.Quote(addr.City), strconv.Quote(country),
		strconv.Quote(addr.Zip), strconv.Quote(addr.FirstName),
		strconv.Quote(addr.LastName), strconv.Quote(province),
		strconv.Quote(phone))
}

func (cs *CheckoutSession) proposalTargetMerchandiseJSON() string {
	if cs.ShippingHandle != "" && cs.StableID != "" {
		return fmt.Sprintf(`{"lines": [{"stableId": %s}]}`, strconv.Quote(cs.StableID))
	}
	return `{"any": true}`
}

func (cs *CheckoutSession) proposalDestinationChanged() string {
	if cs.ShippingHandle != "" {
		return "false"
	}
	return "true"
}

func (cs *CheckoutSession) buildDeliveryBlockJSON(addr Address, country, province, phone string) string {
	if !cs.ShippingRequired && cs.StableID != "" {
		return fmt.Sprintf(`{
      "deliveryLines": [],
      "noDeliveryRequired": [{"stableId": %s}],
      "useProgressiveRates": false,
      "prefetchShippingRatesStrategy": null,
      "supportsSplitShipping": true
    }`, strconv.Quote(cs.StableID))
	}

	deliveryStrategy := cs.buildSelectedDeliveryStrategyJSON()
	destination := cs.proposalDeliveryDestinationJSON(addr, country, province, phone)
	targetMerchandise := cs.proposalTargetMerchandiseJSON()
	destinationChanged := cs.proposalDestinationChanged()
	expectedShippingPrice := cs.proposalExpectedShippingPriceJSON()

	return fmt.Sprintf(`{
      "deliveryLines": [{
        "destination": %s,
        "selectedDeliveryStrategy": %s,
        "targetMerchandiseLines": %s,
        "deliveryMethodTypes": ["SHIPPING"],
        "expectedTotalPrice": %s,
        "destinationChanged": %s
      }],
      "noDeliveryRequired": [],
      "useProgressiveRates": false,
      "prefetchShippingRatesStrategy": null,
      "supportsSplitShipping": true
    }`, destination, deliveryStrategy, targetMerchandise, expectedShippingPrice, destinationChanged)
}

func (cs *CheckoutSession) buildSubmitDeliveryBlockJSON(addr Address, country, province, phone string) string {
	if !cs.ShippingRequired && cs.StableID != "" {
		return fmt.Sprintf(`{
        "deliveryLines": [],
        "noDeliveryRequired": [{"stableId": %s}],
        "useProgressiveRates": false,
        "prefetchShippingRatesStrategy": null,
        "supportsSplitShipping": true
      }`, strconv.Quote(cs.StableID))
	}

	expectedShippingPrice := cs.proposalExpectedShippingPriceJSON()
	return fmt.Sprintf(`{
        "deliveryLines": [{
          "destination": {
            "streetAddress": {
              "address1": %s, "address2": "",
              "city": %s, "countryCode": %s,
              "postalCode": %s, "firstName": %s,
              "lastName": %s, "zoneCode": %s,
              "phone": %s, "oneTimeUse": false
            }
          },
          "selectedDeliveryStrategy": {
            "deliveryStrategyByHandle": {
              "handle": %s,
              "customDeliveryRate": false
            },
            "options": {}
          },
          "targetMerchandiseLines": {
            "lines": [{"stableId": %s}]
          },
          "deliveryMethodTypes": ["SHIPPING"],
          "expectedTotalPrice": %s,
          "destinationChanged": false
        }],
        "noDeliveryRequired": [],
        "useProgressiveRates": false,
        "prefetchShippingRatesStrategy": null,
        "supportsSplitShipping": true
      }`,
		strconv.Quote(addr.Address1), strconv.Quote(addr.City), strconv.Quote(country),
		strconv.Quote(addr.Zip), strconv.Quote(addr.FirstName),
		strconv.Quote(addr.LastName), strconv.Quote(province),
		strconv.Quote(phone),
		strconv.Quote(cs.ShippingHandle),
		strconv.Quote(cs.StableID),
		expectedShippingPrice)
}

func (cs *CheckoutSession) sendProposalAndUpdate(queueToken, email string) (string, error) {
	payload := cs.buildProposal3(queueToken, email)
	body, err := cs.sendProposalRaw(payload)
	if err != nil {
		return "", err
	}
	if qt := extractQueueTokenStr(body); qt != "" {
		cs.QueueToken = qt
	}
	cs.populateShippingFromProposalBody(body)
	cs.logDeliveryState("proposal", body, payload)
	return body, nil
}

// logDeliveryState dumps the negotiated delivery line vs what we proposed/submit.
func (cs *CheckoutSession) logDeliveryState(phase, responseBody, requestPayload string) {
	site := formatSiteLabel(cs.ShopURL)
	var resp map[string]any
	_ = json.Unmarshal([]byte(responseBody), &resp)
	result := navigateMap(resp, "data", "session", "negotiate", "result")
	sp := getMap(result, "sellerProposal")
	dt := getMap(sp, "delivery")
	de := getMap(sp, "deliveryExpectations")

	shipReq := "?"
	if _, ok := sp["isShippingRequired"]; ok {
		shipReq = fmt.Sprintf("%v", getBool(sp, "isShippingRequired"))
	}

	var lineInfo string
	lines := getSlice(dt, "deliveryLines")
	if len(lines) > 0 {
		first, _ := lines[0].(map[string]any)
		sel := getMap(first, "selectedDeliveryStrategy")
		avail := getSlice(first, "availableDeliveryStrategies")
		var availHandles []string
		for _, a := range avail {
			am, _ := a.(map[string]any)
			h := getString(am, "handle")
			amt := getString(getMap(getMap(am, "amount"), "value"), "amount")
			mt := getString(am, "methodType")
			title := getString(am, "title")
			availHandles = append(availHandles, fmt.Sprintf("%s(amt=%s,mt=%s,title=%s)", truncate(h, 16), amt, mt, title))
		}
		dest := getMap(first, "destinationAddress")
		lineInfo = fmt.Sprintf("sel=%s destCity=%s destZip=%s destPhone=%s methods=%v avail=[%s]",
			truncate(getString(sel, "handle"), 20),
			getString(dest, "city"), getString(dest, "postalCode"), getString(dest, "phone"),
			first["deliveryMethodTypes"], strings.Join(availHandles, " | "))
	}

	expType := getString(de, "__typename")
	expCount := len(getSlice(de, "deliveryExpectations"))
	if expCount == 0 {
		expCount = len(cs.DeliveryExps)
	}

	logCheckout(site, "DELIVERY-"+phase,
		"shipReq=%s delivery=%s handle=%s shipAmt=%s total=%s stableId=%s exps=%s/%d line={%s}",
		shipReq, getString(dt, "__typename"), truncate(cs.ShippingHandle, 20),
		cs.ShippingAmount, cs.ActualTotal, truncate(cs.StableID, 12),
		expType, expCount, lineInfo)

	if os.Getenv("DUMP_DELIVERY") != "" {
		slug := strings.ReplaceAll(site, "/", "_")
		_ = os.WriteFile(fmt.Sprintf("/tmp/delivery_%s_%s_req.json", slug, phase), []byte(requestPayload), 0644)
		_ = os.WriteFile(fmt.Sprintf("/tmp/delivery_%s_%s_resp.json", slug, phase), []byte(responseBody), 0644)
	}
}

func (cs *CheckoutSession) proposalTermsPending(body string) bool {
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return false
	}
	result := navigateMap(resp, "data", "session", "negotiate", "result")
	sp := getMap(result, "sellerProposal")
	for _, key := range []string{"delivery", "deliveryExpectations", "checkoutTotal"} {
		if getString(getMap(sp, key), "__typename") == "PendingTerms" {
			return true
		}
	}
	return false
}

// buildProposal3 builds the full address proposal payload
func (cs *CheckoutSession) buildProposal3(queueToken, email string) string {
	addr := cs.Addr
	country := addr.Country
	if country == "" {
		country = "US"
	}
	province := addr.Province
	phone := addr.Phone
	if phone == "" {
		phone = "+12125550100"
	}

	deliveryBlock := cs.buildDeliveryBlockJSON(addr, country, province, phone)
	deliveryExpectations := cs.buildDeliveryExpectationsJSON()

	p := fmt.Sprintf(`{
  "variables": {
    "sessionInput": {"sessionToken": %s},
    "queueToken": %s,
    "discounts": {"lines": [], "acceptUnexpectedDiscounts": true},
    "delivery": %s,
    "deliveryExpectations": %s,
    "merchandise": {
      "merchandiseLines": [{
        "stableId": %s,
        "merchandise": {
          "productVariantReference": {
            "id": "gid://shopify/ProductVariantMerchandise/%s",
            "variantId": "gid://shopify/ProductVariant/%s",
            "properties": [], "sellingPlanId": null, "sellingPlanDigest": null
          }
        },
        "quantity": {"items": {"value": 1}},
        "expectedTotalPrice": {"any": true},
        "lineComponentsSource": null,
        "lineComponents": []
      }]
    },
    "memberships": {"memberships": []},
    "payment": {
      "totalAmount": {"any": true},
      "paymentLines": [],
      "billingAddress": {
        "streetAddress": {
          "address1": %s, "address2": "",
          "city": %s, "countryCode": %s,
          "postalCode": %s, "firstName": %s,
          "lastName": %s, "zoneCode": %s,
          "phone": %s
        }
      }
    },
    "buyerIdentity": {
      "customer": {"presentmentCurrency": "USD", "countryCode": "US"},
      "email": %s,
      "emailChanged": false,
      "phoneCountryCode": "US",
      "marketingConsent": [],
      "shopPayOptInPhone": {"countryCode": "US"},
      "rememberMe": false
    },
    "tip": {"tipLines": []},
    "poNumber": null,
    "taxes": {
      "proposedAllocations": null,
      "proposedTotalAmount": {"any": true},
      "proposedTotalIncludedAmount": null,
      "proposedMixedStateTotalAmount": null,
      "proposedExemptions": []
    },
    "note": {"message": null, "customAttributes": []},
    "localizationExtension": {"fields": []},
    "nonNegotiableTerms": null,
    "scriptFingerprint": {
      "signature": null, "signatureUuid": null,
      "lineItemScriptChanges": [], "paymentScriptChanges": [], "shippingScriptChanges": []
    },
    "optionalDuties": {"buyerRefusesDuties": false},
    "cartMetafields": []
  },
  "operationName": "Proposal",
  "id": %s
}`,
		strconv.Quote(cs.SessionToken), strconv.Quote(queueToken),
		deliveryBlock,
		deliveryExpectations,
		strconv.Quote(cs.StableID), cs.VariantID, cs.VariantID,
		strconv.Quote(addr.Address1), strconv.Quote(addr.City), strconv.Quote(country),
		strconv.Quote(addr.Zip), strconv.Quote(addr.FirstName),
		strconv.Quote(addr.LastName), strconv.Quote(province),
		strconv.Quote(phone),
		strconv.Quote(email),
		strconv.Quote(cs.ProposalID))
	return cs.patchPayload(p)
}

// ─── Step 4: Submit for completion ───────────────────────────────────────────

type SubmitResult struct {
	ReceiptID string
	Code      string
	Message   string
	Response  map[string]any
	Total     string
}

func (cs *CheckoutSession) refreshProposalTotals() error {
	if cs.QueueToken == "" {
		return fmt.Errorf("no queue token for refresh")
	}
	email := cs.Addr.Email
	prevTotal := cs.ActualTotal
	if _, err := cs.sendProposalAndUpdate(cs.QueueToken, email); err != nil {
		return err
	}
	logCheckout(formatSiteLabel(cs.ShopURL), "Step4-prep",
		"refreshed totals: shipping=$%s total=$%s (was $%s) handle=%s exps=%d gateway=%s variant=%s",
		cs.ShippingAmount, cs.ActualTotal, prevTotal,
		truncate(cs.ShippingHandle, 24), len(cs.DeliveryExps), cs.PaymentGateway, cs.VariantID)
	return nil
}

func (cs *CheckoutSession) Step4Submit() SubmitResult {
	fmt.Println("[4/5] Submitting for completion...")
	return cs.step4SubmitOnce()
}

func (cs *CheckoutSession) step4SubmitOnce() SubmitResult {
	addr := cs.Addr
	country := addr.Country
	if country == "" {
		country = "US"
	}
	province := addr.Province
	phone := addr.Phone
	if phone == "" {
		phone = "+12125550100"
	}
	email := addr.Email

	var handleLines []string
	for _, exp := range cs.DeliveryExps {
		h := exp["signedHandle"]
		if h != "" {
			handleLines = append(handleLines, fmt.Sprintf(`{"signedHandle":%s}`, strconv.Quote(h)))
		}
	}
	signedHandlesJSON := "[]"
	if len(handleLines) > 0 {
		signedHandlesJSON = "[" + strings.Join(handleLines, ",") + "]"
	}

	totalAmount := cs.ActualTotal
	if totalAmount == "" {
		totalAmount = cs.ProductPrice
	}
	if totalAmount == "" {
		totalAmount = "1.00"
	}

	currency := cs.CurrencyCode
	if currency == "" {
		currency = "USD"
	}

	attemptToken := generateAttemptToken(cs.CheckoutToken)
	pageID := generatePageID()
	deliveryBlock := cs.buildSubmitDeliveryBlockJSON(addr, country, province, phone)

	gqlPayload := fmt.Sprintf(`{
  "variables": {
    "input": {
      "sessionInput": {"sessionToken": %s},
      "queueToken": %s,
      "discounts": {"lines": [], "acceptUnexpectedDiscounts": true},
      "delivery": %s,
      "deliveryExpectations": {
        "deliveryExpectationLines": %s
      },
      "merchandise": {
        "merchandiseLines": [{
          "stableId": %s,
          "merchandise": {
            "productVariantReference": {
              "id": "gid://shopify/ProductVariantMerchandise/%s",
              "variantId": "gid://shopify/ProductVariant/%s",
              "properties": [], "sellingPlanId": null, "sellingPlanDigest": null
            }
          },
          "quantity": {"items": {"value": 1}},
          "expectedTotalPrice": {"any": true},
          "lineComponentsSource": null,
          "lineComponents": []
        }]
      },
      "memberships": {"memberships": []},
      "payment": {
        "totalAmount": {"value": {"amount": %s, "currencyCode": %s}},
        "paymentLines": [{
          "paymentMethod": {
            "directPaymentMethod": {
              "sessionId": %s,
              "billingAddress": {
                "streetAddress": {
                  "address1": %s, "address2": "",
                  "city": %s, "countryCode": %s,
                  "postalCode": %s, "firstName": %s,
                  "lastName": %s, "zoneCode": %s,
                  "phone": %s
                }
              },
              "cardSource": null
            },
            "giftCardPaymentMethod": null,
            "redeemablePaymentMethod": null,
            "walletPaymentMethod": null,
            "walletsPlatformPaymentMethod": null,
            "localPaymentMethod": null,
            "paymentOnDeliveryMethod": null,
            "paymentOnDeliveryMethod2": null,
            "manualPaymentMethod": null,
            "customPaymentMethod": null,
            "offsitePaymentMethod": null,
            "customOnsitePaymentMethod": null,
            "deferredPaymentMethod": null,
            "customerCreditCardPaymentMethod": null,
            "paypalBillingAgreementPaymentMethod": null,
            "remotePaymentInstrument": null
          },
          "amount": {"value": {"amount": %s, "currencyCode": %s}}
        }],
        "billingAddress": {
          "streetAddress": {
            "address1": %s, "address2": "",
            "city": %s, "countryCode": %s,
            "postalCode": %s, "firstName": %s,
            "lastName": %s, "zoneCode": %s,
            "phone": %s
          }
        }
      },
      "buyerIdentity": {
        "customer": {"presentmentCurrency": %s, "countryCode": "US"},
        "email": %s,
        "emailChanged": false,
        "phoneCountryCode": "US",
        "marketingConsent": [],
        "shopPayOptInPhone": {"countryCode": "US"},
        "rememberMe": false
      },
      "tip": {"tipLines": []},
      "taxes": {
        "proposedAllocations": null,
        "proposedTotalAmount": {"any": true},
        "proposedTotalIncludedAmount": null,
        "proposedMixedStateTotalAmount": null,
        "proposedExemptions": []
      },
      "note": {"message": null, "customAttributes": []},
      "localizationExtension": {"fields": []},
      "nonNegotiableTerms": null,
      "scriptFingerprint": {
        "signature": null, "signatureUuid": null,
        "lineItemScriptChanges": [], "paymentScriptChanges": [], "shippingScriptChanges": []
      },
      "optionalDuties": {"buyerRefusesDuties": false},
      "cartMetafields": []
    },
    "attemptToken": %s,
    "metafields": [],
    "analytics": {
      "requestUrl": %s,
      "pageId": %s
    }
  },
  "operationName": "SubmitForCompletion",
  "id": %s
}`,
		strconv.Quote(cs.SessionToken), strconv.Quote(cs.QueueToken),
		deliveryBlock,
		signedHandlesJSON,
		strconv.Quote(cs.StableID), cs.VariantID, cs.VariantID,
		strconv.Quote(totalAmount), strconv.Quote(currency),
		strconv.Quote(cs.CardSessionID),
		strconv.Quote(addr.Address1), strconv.Quote(addr.City), strconv.Quote(country),
		strconv.Quote(addr.Zip), strconv.Quote(addr.FirstName),
		strconv.Quote(addr.LastName), strconv.Quote(province),
		strconv.Quote(phone),
		strconv.Quote(totalAmount), strconv.Quote(currency),
		strconv.Quote(addr.Address1), strconv.Quote(addr.City), strconv.Quote(country),
		strconv.Quote(addr.Zip), strconv.Quote(addr.FirstName),
		strconv.Quote(addr.LastName), strconv.Quote(province),
		strconv.Quote(phone),
		strconv.Quote(currency), strconv.Quote(email),
		strconv.Quote(attemptToken), strconv.Quote(cs.CheckoutURL), strconv.Quote(pageID),
		strconv.Quote(cs.SubmitID))

	gqlPayload = cs.patchPayload(gqlPayload)

	logCheckout(formatSiteLabel(cs.ShopURL), "DELIVERY-submit",
		"handle=%s shipAmt=%s total=%s stableId=%s exps=%d shipReq=%v dest=%s,%s %s phone=%s",
		truncate(cs.ShippingHandle, 20), cs.ShippingAmount, cs.ActualTotal,
		truncate(cs.StableID, 12), len(cs.DeliveryExps), cs.ShippingRequired,
		addr.City, province, addr.Zip, phone)
	if os.Getenv("DUMP_DELIVERY") != "" {
		slug := strings.ReplaceAll(formatSiteLabel(cs.ShopURL), "/", "_")
		_ = os.WriteFile(fmt.Sprintf("/tmp/delivery_%s_submit_req.json", slug), []byte(gqlPayload), 0644)
	}

	gqlURL := cs.ShopURL + "/checkouts/internal/graphql/persisted?operationName=SubmitForCompletion"

	req, _ := http.NewRequest("POST", gqlURL, strings.NewReader(gqlPayload))
	req.Header = cs.graphqlHeaders()

	resp, err := cs.Client.Do(req)
	if err != nil {
		return SubmitResult{Code: "HTTP_ERROR", Message: err.Error(), Total: cs.ActualTotal}
	}
	decompressBody(resp)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return SubmitResult{Code: fmt.Sprintf("HTTP_%d", resp.StatusCode), Message: fmt.Sprintf("HTTP %d", resp.StatusCode), Total: cs.ActualTotal}
	}

	var response map[string]any
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &response); err != nil {
		return SubmitResult{Code: "JSON_ERROR", Message: err.Error(), Total: cs.ActualTotal}
	}

	result := getMap(response, "data")
	submitResult := getMap(result, "submitForCompletion")
	resultType := getString(submitResult, "__typename")
	fmt.Printf("  Submit result: %s\n", resultType)

	switch resultType {
	case "SubmitSuccess", "SubmitAlreadyAccepted", "SubmittedForCompletion":
		receipt := getMap(submitResult, "receipt")
		receiptID := getString(receipt, "id")
		bodyStr := string(body)
		re := regexp.MustCompile(`"sessionToken"\s*:\s*"([^"]+)"`)
		if m := re.FindStringSubmatch(bodyStr); len(m) > 1 {
			cs.ReceiptSessionToken = m[1]
		}
		if receiptID != "" {
			fmt.Printf("  Receipt ID: %s\n", receiptID)
			return SubmitResult{ReceiptID: receiptID, Code: "SUBMIT_SUCCESS", Response: response, Total: cs.ActualTotal}
		}
		return SubmitResult{ReceiptID: "ACCEPTED", Code: "SUBMIT_ACCEPTED", Response: response, Total: cs.ActualTotal}

	case "SubmitRejected":
		errors := getSlice(submitResult, "errors")
		var codes, msgs []string
		for _, e := range errors {
			em, _ := e.(map[string]any)
			code := getString(em, "code")
			msg := getString(em, "localizedMessage")
			codes = append(codes, code)
			msgs = append(msgs, msg)
			logCheckout(formatSiteLabel(cs.ShopURL), "Step4",
				"REJECTED code=%s msg=%s total=$%s variant=%s handle=%s shipAmt=%s stableId=%s exps=%d",
				code, msg, cs.ActualTotal, cs.VariantID,
				truncate(cs.ShippingHandle, 20), cs.ShippingAmount,
				truncate(cs.StableID, 12), len(cs.DeliveryExps))
		}
		if os.Getenv("DUMP_DELIVERY") != "" {
			slug := strings.ReplaceAll(formatSiteLabel(cs.ShopURL), "/", "_")
			_ = os.WriteFile(fmt.Sprintf("/tmp/delivery_%s_submit_resp.json", slug), body, 0644)
		}
		primaryCode := "SUBMIT_REJECTED"
		if len(codes) > 0 {
			primaryCode = codes[0]
		}
		return SubmitResult{Code: primaryCode, Message: strings.Join(msgs, " | "), Response: response, Total: cs.ActualTotal}

	case "SubmitFailed":
		reason := getString(submitResult, "reason")
		return SubmitResult{Code: "SUBMIT_FAILED", Message: reason, Response: response, Total: cs.ActualTotal}

	case "Throttled":
		return SubmitResult{Code: "THROTTLED", Message: "Throttled by Shopify", Response: response, Total: cs.ActualTotal}

	default:
		return SubmitResult{Code: "UNEXPECTED_RESULT", Message: resultType, Response: response, Total: cs.ActualTotal}
	}
}

// ─── Step 5: Poll for receipt (GET-based, V2) ───────────────────────────────

func (cs *CheckoutSession) Step5PollReceipt(receiptID string) (bool, string, map[string]any) {
	fmt.Println("[5/5] Polling receipt...")

	if !strings.HasPrefix(receiptID, "gid://shopify/") {
		return false, `"code": "INVALID_RECEIPT_ID"`, nil
	}

	sessionToken := cs.ReceiptSessionToken
	if sessionToken == "" {
		sessionToken = cs.SessionToken
	}

	gqlURL := cs.ShopURL + "/checkouts/internal/graphql/persisted"

	var lastResponse map[string]any
	errorStrikes := 0

	for attempt := 1; attempt <= cfg.PollReceiptMax; attempt++ {
		fmt.Printf("  Poll %d/%d...\n", attempt, cfg.PollReceiptMax)

		varsJSON := fmt.Sprintf(`{"receiptId":%s,"sessionToken":%s}`,
			strconv.Quote(receiptID), strconv.Quote(sessionToken))

		params := url.Values{}
		params.Set("operationName", "PollForReceipt")
		params.Set("variables", varsJSON)
		params.Set("id", cs.PollForReceiptID)

		fullURL := gqlURL + "?" + params.Encode()

		req, err := http.NewRequest("GET", fullURL, nil)
		if err != nil {
			time.Sleep(cfg.ShortSleep)
			continue
		}
		req.Header = cs.graphqlHeadersPoll()

		resp, err := cs.Client.Do(req)
		if err != nil {
			time.Sleep(cfg.ShortSleep)
			continue
		}
		decompressBody(resp)

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			fmt.Printf("  [ERROR] HTTP %d\n", resp.StatusCode)
			time.Sleep(cfg.ShortSleep)
			continue
		}

		var response map[string]any
		if err := json.Unmarshal(body, &response); err != nil {
			time.Sleep(cfg.ShortSleep)
			continue
		}
		lastResponse = response

		if _, hasErrors := response["errors"]; hasErrors {
			if _, hasData := response["data"]; !hasData {
				errorStrikes++
				errs := getSlice(response, "errors")
				errMsg := ""
				if len(errs) > 0 {
					if em, ok := errs[0].(map[string]any); ok {
						errMsg = getString(em, "message")
					}
				}
				logCheckout(formatSiteLabel(cs.ShopURL), "Step5",
					"poll GraphQL errors strike=%d msg=%s", errorStrikes, truncate(errMsg, 80))
				if errorStrikes >= 2 {
					return false, "POLL_GRAPHQL_ERROR", response
				}
				time.Sleep(cfg.ShortSleep)
				continue
			}
		}

		receipt := navigateMap(response, "data", "receipt")
		rType := getString(receipt, "__typename")

		switch rType {
		case "ProcessedReceipt":
			fmt.Println("  Order completed (ProcessedReceipt)")
			return true, "SUCCESS", response

		case "ActionRequiredReceipt":
			fmt.Println("  3-D Secure or action required")
			return false, `"code": "ACTION_REQUIRED"`, response

		case "FailedReceipt":
			code := extractFailureCode(receipt)
			fmt.Printf("  FailedReceipt: %s\n", code)
			return false, code, response

		case "ProcessingReceipt", "":
			pollDelay := getFloat(receipt, "pollDelay")
			if pollDelay == 0 {
				pollDelay = 2000
			}
			waitSec := pollDelay / 1000.0
			if waitSec > cfg.MaxWaitSeconds {
				waitSec = cfg.MaxWaitSeconds
			}
			// Shopify often returns pollDelay=100ms; that's too aggressive and
			// burns the poll budget before payment finishes → UNKNOWN timeout.
			const minPollWaitSec = 0.25
			if waitSec < minPollWaitSec {
				waitSec = minPollWaitSec
			}
			fmt.Printf("  Processing... typename=%s delay=%.0fms wait=%.1fs\n",
				rType, pollDelay, waitSec)
			time.Sleep(time.Duration(waitSec * float64(time.Second)))

		default:
			fmt.Printf("  Unexpected receipt typename=%s\n", rType)
			time.Sleep(cfg.ShortSleep)
		}
	}

	fmt.Println("  Poll timeout")
	if lastResponse != nil {
		receipt := navigateMap(lastResponse, "data", "receipt")
		rType := getString(receipt, "__typename")
		logCheckout(formatSiteLabel(cs.ShopURL), "Step5",
			"poll timeout after %d attempts lastTypename=%s", cfg.PollReceiptMax, rType)
		if rType == "FailedReceipt" {
			return false, extractFailureCode(receipt), lastResponse
		}
		if rType == "ProcessedReceipt" {
			return true, "SUCCESS", lastResponse
		}
		if rType == "ActionRequiredReceipt" {
			return false, "ACTION_REQUIRED", lastResponse
		}
		// Still processing — not an unknown failure; surface as timeout so
		// callers don't treat it like a mystery error.
		return false, "PROCESSING_TIMEOUT", lastResponse
	}
	return false, "TIMEOUT", nil
}

// ─── Session token extraction (multiple patterns) ────────────────────────────

var sessionTokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`<meta\s+name="serialized-sessionToken"\s+content="([^"]+)"`),
	regexp.MustCompile(`<meta\s+name="serialized-session-token"\s+content="([^"]+)"`),
	regexp.MustCompile(`(?i)<meta\s+name="[^"]*session[^"]*token[^"]*"\s+content="([^"]+)"`),
	regexp.MustCompile(`(?i)serialized-sessionToken["'\s]*:\s*["']([^"']+)["']`),
	regexp.MustCompile(`(?i)sessionToken["'\s]*:\s*["']([^"']+)["']`),
}

func extractSessionToken(checkoutHTML string) string {
	if m := regexp.MustCompile(`name="serialized-sessionToken"\s+content="&quot;([^&]+)&quot;"`).FindStringSubmatch(checkoutHTML); len(m) > 1 {
		return m[1]
	}
	for _, re := range sessionTokenPatterns {
		m := re.FindStringSubmatch(checkoutHTML)
		if m != nil && len(m[1]) > 20 {
			token := strings.Trim(m[1], `"'`)
			token = htmlUnescape(token)
			token = strings.Trim(token, `"'`)
			if len(token) > 20 {
				return token
			}
		}
	}
	return ""
}

// ─── Build ID extraction (fallback for commitSha) ───────────────────────────

var buildIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`/_next/static/([a-zA-Z0-9_-]{8,64})/_buildManifest\.js`),
	regexp.MustCompile(`"buildId"\s*:\s*"([a-zA-Z0-9_-]{8,64})"`),
	regexp.MustCompile(`/_next/static/([a-zA-Z0-9_-]{8,64})/`),
}

func extractBuildID(checkoutHTML string) string {
	for _, re := range buildIDPatterns {
		m := re.FindStringSubmatch(checkoutHTML)
		if m != nil {
			return m[1]
		}
	}
	return ""
}

// ─── Failure code extraction ─────────────────────────────────────────────────

func extractFailureCode(receipt map[string]any) string {
	pe := getMap(receipt, "processingError")
	code := getString(pe, "code")
	if code != "" {
		return code
	}
	return "UNKNOWN"
}

// ─── Map / JSON navigation helpers ───────────────────────────────────────────

func navigateMap(m map[string]any, keys ...string) map[string]any {
	current := m
	for _, k := range keys {
		next, ok := current[k].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func getMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, ok := m[key].(map[string]any)
	if !ok {
		return nil
	}
	return v
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if ok {
		return s
	}
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return fmt.Sprintf("%v", v)
}

func getFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return val
	case json.Number:
		f, _ := val.Float64()
		return f
	case int:
		return float64(val)
	}
	return 0
}

func getBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key].(bool)
	return ok && v
}

func getSlice(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	v, ok := m[key].([]any)
	if !ok {
		return nil
	}
	return v
}

// ─── String helpers ──────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func htmlUnescape(s string) string {
	replacer := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&#x27;", "'",
		"&#x2F;", "/",
	)
	return replacer.Replace(s)
}

func parsePrice(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "$")
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

func formatAmount(amount string) string {
	if amount == "" {
		return "$0"
	}
	if strings.HasPrefix(amount, "$") {
		return amount
	}
	return "$" + amount
}
