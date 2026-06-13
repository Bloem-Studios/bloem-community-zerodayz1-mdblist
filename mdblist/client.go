// Package mdblist is the outbound client for the MDBList API
// (https://api.mdblist.com). It authenticates with an apikey query parameter,
// is aware of MDBList's rate-limit headers, and self-throttles so a depleted
// daily quota costs zero wasted requests — important once the account is
// downgraded to a cheaper tier after the initial ingest.
package mdblist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	defaultBaseURL  = "https://api.mdblist.com"
	requestTimeout  = 15 * time.Second
	maxResponseBody = 1 << 20 // 1 MiB
)

// ErrRateLimited is returned when the daily quota is exhausted — either because
// MDBList replied 429, or because the client is in a self-imposed cooldown after
// observing X-RateLimit-Remaining: 0. The caller should treat it as transient.
var ErrRateLimited = errors.New("mdblist: rate limited")

// Client talks to one MDBList account. It is safe for concurrent use.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client

	mu sync.Mutex
	// cooldownUntil is the time before which no request will be attempted; set
	// from Retry-After (429) or X-RateLimit-Reset when remaining hits 0.
	cooldownUntil time.Time
	// nextAllowed paces requests when minInterval > 0 (the requests_per_second
	// throttle), spreading a backfill instead of bursting through the quota.
	minInterval time.Duration
	nextAllowed time.Time
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the API base URL (used in tests).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.httpClient = hc } }

// WithRequestsPerSecond caps the outbound request rate. <= 0 means unlimited.
func WithRequestsPerSecond(rps float64) Option {
	return func(c *Client) {
		if rps > 0 {
			c.minInterval = time.Duration(float64(time.Second) / rps)
		} else {
			c.minInterval = 0
		}
	}
}

// NewClient builds a Client for the given API key.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:     apiKey,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: requestTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// CoolingDown reports whether the client is currently in a rate-limit cooldown,
// and until when. Useful for logging/observability.
func (c *Client) CoolingDown() (bool, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.cooldownUntil), c.cooldownUntil
}

// GetMedia fetches a single title. provider is one of imdb|tmdb|tvdb|trakt|mal;
// mediaType is movie|show. A nil *MediaInfo with nil error means "not found"
// (terminal — do not retry). ErrRateLimited means "try again later".
func (c *Client) GetMedia(ctx context.Context, provider, mediaType, id string) (*MediaInfo, error) {
	if c.apiKey == "" {
		return nil, errors.New("mdblist: api key not configured")
	}

	if err := c.gate(ctx); err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/%s/%s/%s?apikey=%s",
		c.baseURL,
		url.PathEscape(provider),
		url.PathEscape(mediaType),
		url.PathEscape(id),
		url.QueryEscape(c.apiKey),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("mdblist: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mdblist: request failed: %w", err)
	}
	defer resp.Body.Close()

	c.observeRateLimit(resp.Header)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		c.enterCooldown(retryAfter(resp.Header))
		return nil, ErrRateLimited
	// Terminal "no usable result" cases: a missing title (404) or a malformed /
	// unprocessable ID (400/422). Retrying won't change the outcome, so report
	// not-found rather than a retriable error to avoid an endless refresh loop.
	case resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusBadRequest,
		resp.StatusCode == http.StatusUnprocessableEntity:
		return nil, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		// 401/403 (auth) and 5xx (server) — surface so it's retried/visible.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return nil, fmt.Errorf("mdblist: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var info MediaInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&info); err != nil {
		return nil, fmt.Errorf("mdblist: decode response: %w", err)
	}

	// MDBList returns a 200 error envelope for some unknown-id cases.
	if info.Error != "" || (info.Title == "" && info.IDs.IMDB == "" && info.IDs.MDBList == "") {
		return nil, nil
	}
	return &info, nil
}

// gate enforces the cooldown and the optional request-rate throttle. It returns
// ErrRateLimited immediately (no network call) while in cooldown.
func (c *Client) gate(ctx context.Context) error {
	c.mu.Lock()
	now := time.Now()
	if now.Before(c.cooldownUntil) {
		c.mu.Unlock()
		return ErrRateLimited
	}
	var wait time.Duration
	if c.minInterval > 0 {
		if c.nextAllowed.After(now) {
			wait = c.nextAllowed.Sub(now)
		}
		c.nextAllowed = now.Add(wait).Add(c.minInterval)
	}
	c.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// observeRateLimit records the remaining-quota headers and, when remaining hits
// zero, pre-emptively enters cooldown until the reset time so subsequent calls
// short-circuit without spending a request.
func (c *Client) observeRateLimit(h http.Header) {
	remaining, hasRemaining := atoiHeader(h, "X-RateLimit-Remaining")
	reset, hasReset := atoiHeader(h, "X-RateLimit-Reset")
	if hasRemaining && remaining <= 0 && hasReset {
		c.enterCooldownAt(time.Unix(reset, 0))
	}
}

func (c *Client) enterCooldown(d time.Duration) {
	if d <= 0 {
		d = time.Minute
	}
	c.enterCooldownAt(time.Now().Add(d))
}

func (c *Client) enterCooldownAt(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.cooldownUntil) {
		c.cooldownUntil = t
	}
}

// retryAfter parses the Retry-After header (seconds form).
func retryAfter(h http.Header) time.Duration {
	if v, ok := atoiHeader(h, "Retry-After"); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	return 0
}

func atoiHeader(h http.Header, key string) (int64, bool) {
	raw := h.Get(key)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
