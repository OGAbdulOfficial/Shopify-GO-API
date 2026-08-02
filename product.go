package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ─── Auto-detect cheapest available product via Storefront GraphQL only ─────

type productProbeOutcome struct {
	products        []jsonProduct
	apiError        string
	storefrontToken string
	gqlShopURL      string
}

func autoDetectProduct(client *http.Client, shopURL string, fp Fingerprint, proxyURL string) *Product {
	p, _ := autoDetectProductResult(client, shopURL, fp, proxyURL)
	return p
}

func autoDetectProductResult(client *http.Client, shopURL string, fp Fingerprint, proxyURL string) (*Product, string) {
	host := formatSiteLabel(shopURL)
	logDebug("PRODUCT", "%s | storefront GraphQL only (max $%.0f)", host, cfg.MaxPrice)

	out := probeStorefrontGraphQL(client, shopURL, fp, proxyURL)
	if strings.Contains(out.apiError, "HTTP 429") {
		logDebug("PRODUCT", "%s | storefront-graphql → rate limited", host)
		return nil, "rate_limited"
	}
	if out.apiError != "" {
		logDebug("PRODUCT", "%s | storefront-graphql → error: %s", host, out.apiError)
		return nil, "no_products"
	}
	if len(out.products) == 0 {
		logDebug("PRODUCT", "%s | storefront-graphql → empty", host)
		return nil, "no_products"
	}
	if p := productWithAlternates(out.products, 20); p != nil {
		p.StorefrontToken = out.storefrontToken
		p.GqlShopURL = out.gqlShopURL
		logDebug("PRODUCT", "%s | storefront-graphql → OK %s $%s variant=%s alts=%d",
			host, p.Title, p.PriceStr, p.VariantID, len(p.AltVariantIDs))
		return p, ""
	}
	logDebug("PRODUCT", "%s | storefront-graphql → nothing under $%.0f", host, cfg.MaxPrice)
	return nil, "no_products"
}

var storefrontTokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)storefrontAccessToken\\":\\"([a-f0-9]{32})`),
	regexp.MustCompile(`(?i)accessToken\\":\\"([a-f0-9]{32})`),
	regexp.MustCompile(`(?i)storefrontAccessToken["'\s:]+([a-f0-9]{32})`),
	regexp.MustCompile(`(?i)publicStorefrontToken["'\s:]+([a-f0-9]{32})`),
	regexp.MustCompile(`(?i)accessToken["'\s:]+([a-f0-9]{32})`),
	regexp.MustCompile(`(?i)x-shopify-storefront-access-token["'\s:]+([a-f0-9]{32})`),
}

var myshopifyDomainPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)myshopifyDomain["'\s:]+([a-z0-9-]+\.myshopify\.com)`),
	regexp.MustCompile(`Shopify\.shop\s*=\s*"([a-z0-9-]+\.myshopify\.com)"`),
	regexp.MustCompile(`(?i)"permanent_domain":"([a-z0-9-]+\.myshopify\.com)"`),
}

func extractStorefrontAccessToken(html string) string {
	for _, re := range storefrontTokenPatterns {
		if m := re.FindStringSubmatch(html); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

func extractMyshopifyDomain(html string) string {
	for _, re := range myshopifyDomainPatterns {
		if m := re.FindStringSubmatch(html); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

func fetchStorefrontAccessToken(client *http.Client, shopURL string, fp Fingerprint) (string, string) {
	host := formatSiteLabel(shopURL)
	gqlShopURL := shopURL
	// /cart first — homepage often 429 but cart still serves the token
	for _, path := range []string{"/cart", "/", "/collections/all"} {
		req, err := http.NewRequest("GET", shopURL+path, nil)
		if err != nil {
			logDebug("TOKEN", "%s | %s → request error: %v", host, path, err)
			continue
		}
		setBrowseHeaders(req, fp, shopURL)

		resp, err := client.Do(req)
		if err != nil {
			logDebug("TOKEN", "%s | %s → dial error: %v", host, path, err)
			continue
		}
		body, readErr := readRespBody(resp)
		if readErr != nil {
			logDebug("TOKEN", "%s | %s → read error: %v (HTTP %d)", host, path, readErr, resp.StatusCode)
			continue
		}
		html := string(body)
		tok := extractStorefrontAccessToken(html)
		if tok != "" {
			if resp.Request != nil && resp.Request.URL != nil {
				gqlShopURL = resp.Request.URL.Scheme + "://" + resp.Request.URL.Host
			} else if dom := extractMyshopifyDomain(html); dom != "" {
				gqlShopURL = "https://" + dom
			}
			logDebug("TOKEN", "%s | %s → OK token=%s... body=%d gql=%s", host, path, truncate(tok, 8), len(body), formatSiteLabel(gqlShopURL))
			return tok, gqlShopURL
		}
		logDebug("TOKEN", "%s | %s → HTTP %d body=%d no token", host, path, resp.StatusCode, len(body))
	}
	logDebug("TOKEN", "%s | no storefront token in HTML", host)
	return "", gqlShopURL
}

const storefrontProductsQuery = `query StorefrontProducts($first: Int!) {
  products(first: $first, sortKey: PRICE) {
    edges {
      node {
        title
        variants(first: 20) {
          edges {
            node {
              id
              price { amount }
              availableForSale
            }
          }
        }
      }
    }
  }
}`

func probeStorefrontGraphQL(client *http.Client, shopURL string, fp Fingerprint, proxyURL string) productProbeOutcome {
	tokenClient := newStandardClient(proxyURL, client.Timeout)
	token, gqlShopURL := fetchStorefrontAccessToken(tokenClient, shopURL, fp)
	if token == "" {
		logProductAPI(shopURL, "storefront-graphql", "/", 0, 0, "no storefront token in HTML")
		return productProbeOutcome{apiError: "no storefront token"}
	}

	limit := 50
	if cfg.FastMode {
		limit = 30
	}
	payload := map[string]any{
		"query": storefrontProductsQuery,
		"variables": map[string]any{
			"first": limit,
		},
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return productProbeOutcome{apiError: "marshal error"}
	}

	gqlURL := gqlShopURL + "/api/unstable/graphql.json"
	req, err := http.NewRequest("POST", gqlURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return productProbeOutcome{apiError: "request error: " + err.Error()}
	}
	setAPIHeaders(req, fp, gqlShopURL)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Storefront-Access-Token", token)

	resp, err := client.Do(req)
	if err != nil {
		logProductAPIAudit(shopURL, "storefront-graphql", "/api/unstable/graphql.json", nil, nil, "dial error: "+err.Error())
		return productProbeOutcome{apiError: "dial error: " + err.Error()}
	}
	respBody, readErr := readRespBody(resp)
	if resp.StatusCode != 200 {
		logProductAPIAudit(shopURL, "storefront-graphql", "/api/unstable/graphql.json", resp, respBody,
			fmt.Sprintf("NON-200 err=%v", readErr))
		return productProbeOutcome{apiError: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if readErr != nil {
		return productProbeOutcome{apiError: "read error"}
	}

	var parsed struct {
		Data struct {
			Products struct {
				Edges []struct {
					Node struct {
						Title    string `json:"title"`
						Variants struct {
							Edges []struct {
								Node struct {
									ID               string `json:"id"`
									AvailableForSale bool   `json:"availableForSale"`
									Price            struct {
										Amount string `json:"amount"`
									} `json:"price"`
								} `json:"node"`
							} `json:"edges"`
						} `json:"variants"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"products"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		logProductAPIAudit(shopURL, "storefront-graphql", "/api/unstable/graphql.json", resp, respBody, "json parse error")
		return productProbeOutcome{apiError: "json parse error"}
	}
	if len(parsed.Errors) > 0 {
		msg := parsed.Errors[0].Message
		logProductAPIAudit(shopURL, "storefront-graphql", "/api/unstable/graphql.json", resp, respBody, "gql: "+msg)
		return productProbeOutcome{apiError: "gql: " + msg}
	}

	products := make([]jsonProduct, 0, len(parsed.Data.Products.Edges))
	for _, edge := range parsed.Data.Products.Edges {
		node := edge.Node
		if len(node.Variants.Edges) == 0 {
			continue
		}
		jp := jsonProduct{Title: node.Title}
		for _, ve := range node.Variants.Edges {
			vn := ve.Node
			vid := variantIDFromGID(vn.ID)
			if vid == "" {
				continue
			}
			avail := vn.AvailableForSale
			jp.Variants = append(jp.Variants, struct {
				ID               json.Number `json:"id"`
				Price            string      `json:"price"`
				Available        *bool       `json:"available"`
				InventoryQty     *int        `json:"inventory_quantity"`
				InventoryPolicy  string      `json:"inventory_policy"`
				RequiresShipping *bool       `json:"requires_shipping"`
			}{
				ID:        json.Number(vid),
				Price:     vn.Price.Amount,
				Available: &avail,
			})
		}
		if len(jp.Variants) > 0 {
			products = append(products, jp)
		}
	}
	note := fmt.Sprintf("token=%s... raw=%d", truncate(token, 8), len(products))
	logProductAPIAudit(shopURL, "storefront-graphql", "/api/unstable/graphql.json", resp, respBody, note)
	logProductAPI(shopURL, "storefront-graphql", "/api/unstable/graphql.json", 200, len(products), note)
	return productProbeOutcome{products: products, storefrontToken: token, gqlShopURL: gqlShopURL}
}

func variantIDFromGID(gid string) string {
	const prefix = "gid://shopify/ProductVariant/"
	if strings.HasPrefix(gid, prefix) {
		return strings.TrimPrefix(gid, prefix)
	}
	if idx := strings.LastIndex(gid, "/"); idx >= 0 {
		return gid[idx+1:]
	}
	return ""
}

func readRespBody(resp *http.Response) ([]byte, error) {
	decompressBody(resp)
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

type jsonProduct struct {
	ID       json.Number `json:"id"`
	Title    string      `json:"title"`
	Variants []struct {
		ID               json.Number `json:"id"`
		Price            string      `json:"price"`
		Available        *bool       `json:"available"`
		InventoryQty     *int        `json:"inventory_quantity"`
		InventoryPolicy  string      `json:"inventory_policy"`
		RequiresShipping *bool       `json:"requires_shipping"`
	} `json:"variants"`
}

func productWithAlternates(products []jsonProduct, max int) *Product {
	type candidate struct {
		pid, vid, priceStr, title string
		price                     float64
		requiresShipping          bool
	}
	var candidates []candidate

	for _, p := range products {
		for _, v := range p.Variants {
			price := parsePrice(v.Price)
			if price <= 0 {
				continue
			}
			if cfg.MinPrice > 0 && price < cfg.MinPrice {
				continue
			}
			if cfg.MaxPrice > 0 && price > cfg.MaxPrice {
				continue
			}

			available := false
			if v.Available != nil {
				available = *v.Available
			} else if v.InventoryQty != nil {
				available = *v.InventoryQty > 0
			}
			if !available && strings.EqualFold(v.InventoryPolicy, "continue") {
				available = true
			}
			if !available {
				continue
			}

			candidates = append(candidates, candidate{
				pid:              p.ID.String(),
				vid:              v.ID.String(),
				priceStr:         v.Price,
				title:            p.Title,
				price:            price,
				requiresShipping: v.RequiresShipping == nil || *v.RequiresShipping,
			})
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].price != candidates[j].price {
			return candidates[i].price < candidates[j].price
		}
		return candidates[i].requiresShipping
	})

	if max <= 0 {
		max = 20
	}
	if len(candidates) > max {
		candidates = candidates[:max]
	}

	primary := candidates[0]
	product := &Product{
		ID:        primary.pid,
		VariantID: primary.vid,
		Price:     primary.price,
		PriceStr:  primary.priceStr,
		Title:     primary.title,
	}
	seen := map[string]bool{primary.vid: true}
	for _, c := range candidates[1:] {
		if seen[c.vid] {
			continue
		}
		seen[c.vid] = true
		product.AltVariantIDs = append(product.AltVariantIDs, c.vid)
	}
	return product
}

func setBrowseHeaders(req *http.Request, fp Fingerprint, shopURL string) {
	req.Header.Set("User-Agent", fp.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Sec-CH-UA", fp.SecCHUA)
	req.Header.Set("Sec-CH-UA-Mobile", fp.SecCHUAMobile)
	req.Header.Set("Sec-CH-UA-Platform", fp.SecCHUAPlatform)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	if shopURL != "" {
		req.Header.Set("Referer", shopURL+"/")
	}
}

func setAPIHeaders(req *http.Request, fp Fingerprint, shopURL string) {
	req.Header.Set("User-Agent", fp.UserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Sec-CH-UA", fp.SecCHUA)
	req.Header.Set("Sec-CH-UA-Mobile", fp.SecCHUAMobile)
	req.Header.Set("Sec-CH-UA-Platform", fp.SecCHUAPlatform)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if shopURL != "" {
		req.Header.Set("Referer", shopURL+"/")
	}
}

func checkoutDelay(minMS, maxMS int) time.Duration {
	if cfg.FastMode {
		minMS = max(minMS/4, 15)
		maxMS = max(maxMS/4, 40)
	}
	return jitter(minMS, maxMS)
}

func jitter(minMS, maxMS int) time.Duration {
	ms := minMS + rand.IntN(maxMS-minMS+1)
	return time.Duration(ms) * time.Millisecond
}
