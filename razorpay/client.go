package main

import (
	"net/http"
	"net/url"
	"time"
)

// newClient creates an HTTP client with optional proxy and custom transport timeout settings.
func newClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if proxyURL != "" {
		parsedProxy, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(parsedProxy)
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}
