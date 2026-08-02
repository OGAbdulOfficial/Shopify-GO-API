package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	// ─── Single-card JSON mode (for bot integration) ─────────────────────
	if len(os.Args) > 1 && os.Args[1] == "-single" {
		runSingleMode()
		return
	}

	// ─── API server mode ─────────────────────────────────────────────────
	if len(os.Args) > 1 && os.Args[1] == "-api" {
		SetQuietLogs(true)
		cfg.SummaryOnly = true
		cfg.SiteRemoval = false
		runAPIServer()
		return
	}

	fmt.Println(strings.Repeat("=", 70))
	fmt.Println("  ⚡  GoCheck — Shopify Checkout Engine")
	fmt.Println(strings.Repeat("=", 70))

	// ─── Load sites ──────────────────────────────────────────────────────
	sites := loadSites("")
	if len(sites) == 0 {
		fmt.Println("[FATAL] No sites found. Set SITES_API_BASE_URL or use -api mode with sites in the request body.")
		os.Exit(1)
	}
	fmt.Printf("[OK] Loaded %d site(s)\n", len(sites))

	// ─── Load cards ──────────────────────────────────────────────────────
	cardsFile := strings.TrimSpace(os.Getenv("CARDS_FILE"))
	cards := loadCards(cardsFile)
	if len(cards) == 0 {
		fmt.Println("[FATAL] No cards found. Set CARDS_FILE or use -api mode with cards in the request body.")
		os.Exit(1)
	}
	fmt.Printf("[OK] Loaded %d card(s)\n", len(cards))

	// ─── Load addresses ──────────────────────────────────────────────────
	addresses := loadAddresses("addresses.txt")
	fmt.Printf("[OK] Loaded %d address(es)\n", len(addresses))

	// ─── Load proxies ────────────────────────────────────────────────────
	proxyFile := findProxyFile()
	var proxies *ProxyRotator
	if proxyFile != "" {
		raw := loadProxies(proxyFile)
		if len(raw) > 0 {
			proxies = NewProxyRotator(raw)
			fmt.Printf("[OK] Loaded %d proxies from %s\n", len(raw), proxyFile)
		}
	}
	if proxies == nil {
		proxies = NewProxyRotator(nil) // empty rotator — direct connect
		fmt.Println("[WARN] No proxies loaded — running direct connections")
	}

	// ─── Summary ─────────────────────────────────────────────────────────
	fmt.Println(strings.Repeat("─", 70))
	fmt.Printf("Workers: %d  |  FastMode: %v  |  SingleProxy: %v\n",
		cfg.ParallelWorkers, cfg.FastMode, cfg.SingleProxyAttempt)
	fmt.Println(strings.Repeat("─", 70))

	start := time.Now()

	// ─── Run ─────────────────────────────────────────────────────────────
	runParallel(cards, sites, proxies, addresses)

	elapsed := time.Since(start).Seconds()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  Done — %d cards processed in %.1fs\n", len(cards), elapsed)
	fmt.Println(strings.Repeat("=", 70))
}
