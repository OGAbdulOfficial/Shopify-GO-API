package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Lock-free proxy rotator ─────────────────────────────────────────────────

type ProxyRotator struct {
	proxies []string
	index   atomic.Uint64
	mu      sync.RWMutex
}

func NewProxyRotator(proxies []string) *ProxyRotator {
	return &ProxyRotator{proxies: proxies}
}

func (pr *ProxyRotator) Next() string {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	if len(pr.proxies) == 0 {
		return ""
	}
	idx := pr.index.Add(1) - 1
	return pr.proxies[idx%uint64(len(pr.proxies))]
}

func (pr *ProxyRotator) Len() int {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	return len(pr.proxies)
}

func (pr *ProxyRotator) Remove(proxyURL string) {
	normalized := normalizeProxy(proxyURL)
	pr.mu.Lock()
	defer pr.mu.Unlock()
	filtered := make([]string, 0, len(pr.proxies))
	for _, p := range pr.proxies {
		if normalizeProxy(p) != normalized {
			filtered = append(filtered, p)
		}
	}
	pr.proxies = filtered
}

// ─── Proxy normalization ─────────────────────────────────────────────────────

func normalizeProxy(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}

	lower := strings.ToLower(p)
	if strings.HasPrefix(lower, "socks5://") || strings.HasPrefix(lower, "socks4://") {
		return normalizeProxyURL(p)
	}
	if strings.HasPrefix(lower, "socks5:") || strings.HasPrefix(lower, "socks4:") {
		return normalizeProxyURL("socks5://" + p)
	}

	// host:port:user:pass (no @ sign)
	if !strings.Contains(p, "@") && !strings.Contains(p, "://") {
		parts := strings.Split(p, ":")
		if len(parts) == 4 {
			host, port, user, pass := parts[0], parts[1], parts[2], parts[3]
			if host != "" && port != "" && user != "" {
				u := &url.URL{
					Scheme: "http",
					Host:   net.JoinHostPort(host, port),
					User:   url.UserPassword(user, pass),
				}
				return u.String()
			}
		}
	}

	// user:pass@host:port or host:port
	if !strings.Contains(p, "://") {
		p = "http://" + p
	}

	return normalizeProxyURL(p)
}

func normalizeProxyURL(p string) string {
	parsed, err := url.Parse(p)
	if err != nil || parsed.Host == "" {
		return ""
	}

	// HTTP CONNECT proxies use the http scheme even when labeled https://
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http":
		parsed.Scheme = "http"
	case "socks5", "socks4":
		// keep socks schemes as-is
	default:
		parsed.Scheme = "http"
	}

	if parsed.User != nil {
		user := parsed.User.Username()
		pass, _ := parsed.User.Password()
		if user != "" {
			if pass != "" {
				parsed.User = url.UserPassword(user, pass)
			} else {
				parsed.User = url.User(user)
			}
		}
	}

	return parsed.String()
}

// testProxyConnectivity checks whether a proxy can reach the public internet.
func testProxyConnectivity(proxyURL string) bool {
	proxyURL = normalizeProxy(proxyURL)
	if proxyURL == "" {
		return false
	}

	client := newStandardClient(proxyURL, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org?format=text", nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoCheck/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(body)) != ""
}

// ─── Load proxies from file ──────────────────────────────────────────────────

func loadProxies(filename string) []string {
	if filename == "" {
		return nil
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil
	}
	defer f.Close()

	var proxies []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		normalized := normalizeProxy(line)
		if normalized != "" {
			proxies = append(proxies, normalized)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Printf("❌ [ERROR] Error reading proxies: %v\n", err)
	}
	return proxies
}

// ─── Find proxy file ─────────────────────────────────────────────────────────

func findProxyFile() string {
	candidates := []string{"working_proxies.txt", "px.txt"}
	for _, name := range candidates {
		if _, err := os.Stat(name); err == nil {
			fmt.Printf("[PROXY] Found proxy file: %s\n", name)
			return name
		}
	}
	return ""
}
