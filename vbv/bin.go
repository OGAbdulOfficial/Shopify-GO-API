package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type BINInfo struct {
	BIN         string `json:"bin"`
	Scheme      string `json:"scheme"`
	Brand       string `json:"brand"`
	Type        string `json:"type"`
	Level       string `json:"level"`
	Bank        string `json:"bank"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	CountryFlag string `json:"country_flag"`
}

var (
	binCache = make(map[string]BINInfo)
	binMu    sync.RWMutex
)

// ExtractBIN retrieves the first 6-8 digits of a card number.
func ExtractBIN(cardNumber string) string {
	cleaned := regexp.MustCompile(`\D`).ReplaceAllString(cardNumber, "")
	if len(cleaned) >= 8 {
		return cleaned[:8]
	}
	if len(cleaned) >= 6 {
		return cleaned[:6]
	}
	return cleaned
}

// LuhnCheck verifies whether a card number is mathematically valid using Luhn's algorithm.
func LuhnCheck(cardNumber string) bool {
	cleaned := regexp.MustCompile(`\D`).ReplaceAllString(cardNumber, "")
	if len(cleaned) < 12 || len(cleaned) > 19 {
		return false
	}

	sum := 0
	alternate := false

	for i := len(cleaned) - 1; i >= 0; i-- {
		n, err := strconv.Atoi(string(cleaned[i]))
		if err != nil {
			return false
		}

		if alternate {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}

		sum += n
		alternate = !alternate
	}

	return sum%10 == 0
}

// LookupBIN fetches detailed information about a card BIN.
func LookupBIN(bin string, proxyURL string) BINInfo {
	if len(bin) < 6 {
		return BINInfo{BIN: bin}
	}
	sixDigit := bin[:6]

	binMu.RLock()
	if info, exists := binCache[sixDigit]; exists {
		binMu.RUnlock()
		return info
	}
	binMu.RUnlock()

	client, err := newClient(proxyURL, 4*time.Second)
	if err != nil {
		client = http.DefaultClient
	}

	// Try Source 1: bins.antipublic.cc/bins/{bin}
	info := fetchAntipublicBIN(client, sixDigit)

	// Try Source 2: handyapi BIN lookup
	if info.Bank == "" && info.Scheme == "" {
		info = fetchHandyAPIBIN(client, sixDigit)
	}

	// Try Source 3: binlist.net
	if info.Bank == "" && info.Scheme == "" {
		info = fetchBinlistBIN(client, sixDigit)
	}

	// Offline Fallback heuristic if external APIs are down or rate limited
	if info.Scheme == "" || info.Brand == "" {
		info.Scheme = detectBrand(sixDigit)
		info.Brand = info.Scheme
	}
	if info.Type == "" {
		info.Type = "CREDIT"
	}
	if info.Level == "" {
		info.Level = "CLASSIC"
	}
	if info.BIN == "" {
		info.BIN = sixDigit
	}

	// Cache result
	binMu.Lock()
	binCache[sixDigit] = info
	binMu.Unlock()

	return info
}

func fetchAntipublicBIN(client *http.Client, bin string) BINInfo {
	url := fmt.Sprintf("https://bins.antipublic.cc/bins/%s", bin)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return BINInfo{}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return BINInfo{}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return BINInfo{}
	}

	var data struct {
		BIN         string `json:"bin"`
		Brand       string `json:"brand"`
		Type        string `json:"type"`
		Level       string `json:"level"`
		Bank        string `json:"bank"`
		Country     string `json:"country_name"`
		CountryCode string `json:"country_code"`
		CountryFlag string `json:"country_flag"`
	}

	if err := json.Unmarshal(body, &data); err != nil {
		return BINInfo{}
	}

	return BINInfo{
		BIN:         bin,
		Scheme:      strings.ToUpper(data.Brand),
		Brand:       strings.ToUpper(data.Brand),
		Type:        strings.ToUpper(data.Type),
		Level:       strings.ToUpper(data.Level),
		Bank:        strings.ToUpper(data.Bank),
		Country:     strings.ToUpper(data.Country),
		CountryCode: strings.ToUpper(data.CountryCode),
		CountryFlag: data.CountryFlag,
	}
}

func fetchHandyAPIBIN(client *http.Client, bin string) BINInfo {
	url := fmt.Sprintf("https://data.handyapi.com/bin/%s", bin)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return BINInfo{}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return BINInfo{}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return BINInfo{}
	}

	var data struct {
		Status string `json:"Status"`
		Scheme string `json:"Scheme"`
		Type   string `json:"Type"`
		Issuer string `json:"Issuer"`
		CardTier string `json:"CardTier"`
		Country struct {
			Name string `json:"Name"`
			A2   string `json:"A2"`
		} `json:"Country"`
	}

	if err := json.Unmarshal(body, &data); err != nil || data.Status != "SUCCESS" {
		return BINInfo{}
	}

	flag := getCountryFlag(data.Country.A2)

	return BINInfo{
		BIN:         bin,
		Scheme:      strings.ToUpper(data.Scheme),
		Brand:       strings.ToUpper(data.Scheme),
		Type:        strings.ToUpper(data.Type),
		Level:       strings.ToUpper(data.CardTier),
		Bank:        strings.ToUpper(data.Issuer),
		Country:     strings.ToUpper(data.Country.Name),
		CountryCode: strings.ToUpper(data.Country.A2),
		CountryFlag: flag,
	}
}

func fetchBinlistBIN(client *http.Client, bin string) BINInfo {
	url := fmt.Sprintf("https://lookup.binlist.net/%s", bin)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return BINInfo{}
	}
	req.Header.Set("Accept-Version", "3")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return BINInfo{}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return BINInfo{}
	}

	var data struct {
		Scheme  string `json:"scheme"`
		Type    string `json:"type"`
		Brand   string `json:"brand"`
		Country struct {
			Name   string `json:"name"`
			Alpha2 string `json:"alpha2"`
			Emoji  string `json:"emoji"`
		} `json:"country"`
		Bank struct {
			Name string `json:"name"`
		} `json:"bank"`
	}

	if err := json.Unmarshal(body, &data); err != nil {
		return BINInfo{}
	}

	return BINInfo{
		BIN:         bin,
		Scheme:      strings.ToUpper(data.Scheme),
		Brand:       strings.ToUpper(data.Brand),
		Type:        strings.ToUpper(data.Type),
		Level:       "",
		Bank:        strings.ToUpper(data.Bank.Name),
		Country:     strings.ToUpper(data.Country.Name),
		CountryCode: strings.ToUpper(data.Country.Alpha2),
		CountryFlag: data.Country.Emoji,
	}
}

func detectBrand(cardNumber string) string {
	switch {
	case strings.HasPrefix(cardNumber, "4"):
		return "VISA"
	case strings.HasPrefix(cardNumber, "51"), strings.HasPrefix(cardNumber, "52"),
		strings.HasPrefix(cardNumber, "53"), strings.HasPrefix(cardNumber, "54"),
		strings.HasPrefix(cardNumber, "55"), strings.HasPrefix(cardNumber, "22"),
		strings.HasPrefix(cardNumber, "23"), strings.HasPrefix(cardNumber, "27"):
		return "MASTERCARD"
	case strings.HasPrefix(cardNumber, "34"), strings.HasPrefix(cardNumber, "37"):
		return "AMEX"
	case strings.HasPrefix(cardNumber, "6011"), strings.HasPrefix(cardNumber, "65"):
		return "DISCOVER"
	case strings.HasPrefix(cardNumber, "35"):
		return "JCB"
	case strings.HasPrefix(cardNumber, "60"), strings.HasPrefix(cardNumber, "81"), strings.HasPrefix(cardNumber, "82"):
		return "RUPAY"
	default:
		return "UNKNOWN"
	}
}

func getCountryFlag(countryCode string) string {
	if len(countryCode) != 2 {
		return "🌐"
	}
	countryCode = strings.ToUpper(countryCode)
	rune1 := rune(countryCode[0]) + 127397
	rune2 := rune(countryCode[1]) + 127397
	return string([]rune{rune1, rune2})
}
