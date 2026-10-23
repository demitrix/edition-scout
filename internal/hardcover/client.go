// Package hardcover is a small, rate-limited client for Hardcover's GraphQL
// API (https://hardcover.app). Hardcover allows about 60 requests a minute
// per token; requests are spaced to stay under that, slow down further when
// Hardcover pushes back, and fail fast when a caller's deadline can't be met.
package hardcover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	endpoint     = "https://api.hardcover.app/v1/graphql"
	baseInterval = 1100 * time.Millisecond // ~55 requests a minute
	maxInterval  = 15 * time.Second
	maxAttempts  = 4
)

// ErrRateLimited means the request wasn't sent (or was refused) because of
// Hardcover's rate limit and the caller's deadline didn't allow waiting.
var ErrRateLimited = errors.New("hardcover rate limited")

// ErrNotConfigured means no API token is set.
var ErrNotConfigured = errors.New("hardcover API token not set")

type Client struct {
	token     string
	userAgent string
	http      *http.Client

	mu        sync.Mutex
	next      time.Time
	interval  time.Duration
	successes int

	statsMu sync.Mutex
	stats   Stats
}

// Stats counts requests since start, for the status page.
type Stats struct {
	Requests    int       `json:"requests"`
	Errors      int       `json:"errors"`
	RateLimited int       `json:"rateLimited"`
	LastError   string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitempty"`
	IntervalMs  int64     `json:"intervalMs"`
}

func New(token, userAgent string) *Client {
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "Bearer "))
	return &Client{token: token, userAgent: userAgent, http: &http.Client{Timeout: 30 * time.Second}, interval: baseInterval}
}

func (c *Client) Configured() bool { return c.token != "" }

func (c *Client) Stats() Stats {
	c.statsMu.Lock()
	s := c.stats
	c.statsMu.Unlock()
	c.mu.Lock()
	s.IntervalMs = c.interval.Milliseconds()
	c.mu.Unlock()
	return s
}

// wait reserves the next request slot, sleeping until it. It refuses (with
// ErrRateLimited) when the slot lies beyond ctx's deadline.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	now := time.Now()
	start := now
	if c.next.After(start) {
		start = c.next
	}
	if dl, ok := ctx.Deadline(); ok && start.After(dl) {
		c.mu.Unlock()
		return ErrRateLimited
	}
	c.next = start.Add(c.interval)
	c.mu.Unlock()
	if d := time.Until(start); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// penalize slows requests down after a refusal; succeed speeds them back up.
func (c *Client) penalize(hint time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.successes = 0
	c.interval *= 2
	if c.interval > maxInterval {
		c.interval = maxInterval
	}
	if hint > 0 {
		if t := time.Now().Add(hint); t.After(c.next) {
			c.next = t
		}
	}
}

func (c *Client) succeed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.interval <= baseInterval {
		return
	}
	if c.successes++; c.successes >= 10 {
		c.successes = 0
		c.interval /= 2
		if c.interval < baseInterval {
			c.interval = baseInterval
		}
	}
}

func (c *Client) record(err error) {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	c.stats.Requests++
	if err != nil {
		c.stats.Errors++
		if errors.Is(err, ErrRateLimited) {
			c.stats.RateLimited++
		}
		c.stats.LastError, c.stats.LastErrorAt = err.Error(), time.Now()
	}
}

type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// Query runs a GraphQL query and decodes the whole response body into out.
func (c *Client) Query(ctx context.Context, query string, vars map[string]any, out any) (err error) {
	if c.token == "" {
		return ErrNotConfigured
	}
	defer func() { c.record(err) }()
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := c.wait(ctx); err != nil {
			if last != nil && errors.Is(err, ErrRateLimited) {
				return fmt.Errorf("%w: %v", ErrRateLimited, last)
			}
			return err
		}
		status, header, raw, err := c.roundTrip(ctx, body)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			last = err
			continue
		}
		if status == http.StatusTooManyRequests || status >= 500 {
			hint, _ := strconv.Atoi(header.Get("Retry-After"))
			c.penalize(time.Duration(hint) * time.Second)
			last = fmt.Errorf("hardcover HTTP %d", status)
			if status == http.StatusTooManyRequests {
				last = fmt.Errorf("%w (HTTP 429)", ErrRateLimited)
			}
			continue
		}
		if status != http.StatusOK {
			return fmt.Errorf("hardcover HTTP %d: %s", status, truncate(string(raw), 200))
		}
		var env struct {
			Errors []gqlError `json:"errors"`
		}
		if json.Unmarshal(raw, &env) == nil && len(env.Errors) > 0 {
			msg := env.Errors[0].Message
			if strings.Contains(strings.ToLower(msg), "throttl") || strings.Contains(strings.ToLower(msg), "rate limit") {
				c.penalize(0)
				last = fmt.Errorf("%w: %s", ErrRateLimited, msg)
				continue
			}
			return fmt.Errorf("hardcover GraphQL: %s", msg)
		}
		c.succeed()
		return json.Unmarshal(raw, out)
	}
	return last
}

func (c *Client) roundTrip(ctx context.Context, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return resp.StatusCode, resp.Header, raw, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
