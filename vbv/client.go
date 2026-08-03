package main

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"time"
)

// newClient creates an HTTP client with optional proxy and custom transport settings.
func newClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	if proxyURL != "" {
		parsedProxy, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(parsedProxy)
	}

	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}
