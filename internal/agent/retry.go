package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// RetryNotice is told about each retry so front-ends can show it — a
// silent backoff looks exactly like a hang.
type RetryNotice func(attempt int, wait time.Duration, reason string)

type retryNoticeKey struct{}

// WithRetryNotice attaches a retry observer to ctx.
func WithRetryNotice(ctx context.Context, fn RetryNotice) context.Context {
	return context.WithValue(ctx, retryNoticeKey{}, fn)
}

// debugOut receives one line per HTTP attempt when COMETCLI_DEBUG is set.
var debugOut io.Writer = os.Stderr

func debugf(format string, args ...any) {
	if v := os.Getenv("COMETCLI_DEBUG"); v != "" && v != "0" {
		fmt.Fprintf(debugOut, "[debug] "+format+"\n", args...)
	}
}

// retryPolicy bounds how hard doHTTP tries before surfacing an error.
type retryPolicy struct {
	Max  int           // retries after the first attempt
	Base time.Duration // first backoff
	Cap  time.Duration // longest single wait
}

var defaultRetry = retryPolicy{Max: 4, Base: time.Second, Cap: 30 * time.Second}

// sleep is swapped out in tests.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryable reports whether a status is worth another attempt: timeouts,
// conflicts, rate limits, and server-side failures (incl. Anthropic 529).
func retryable(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusConflict ||
		code == http.StatusTooManyRequests || code >= 500
}

// doHTTP sends a JSON POST, retrying connection errors and retryable
// statuses with jittered exponential backoff, honoring Retry-After. It
// only retries before a body is read, so a stream is never replayed
// half-consumed. The returned response is the caller's to close.
func doHTTP(ctx context.Context, hc *http.Client, url string, body []byte, hdr map[string]string) (*http.Response, error) {
	return doHTTPWith(ctx, hc, url, body, hdr, defaultRetry)
}

func doHTTPWith(ctx context.Context, hc *http.Client, url string, body []byte, hdr map[string]string, rp retryPolicy) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		for k, v := range hdr {
			if v != "" {
				req.Header.Set(k, v)
			}
		}
		start := time.Now()
		resp, err := hc.Do(req)
		var wait time.Duration
		var reason string
		switch {
		case err != nil:
			debugf("POST %s attempt %d: %v (%s)", redactURL(url), attempt+1, err, time.Since(start).Round(time.Millisecond))
			if ctx.Err() != nil || !transient(err) || attempt >= rp.Max {
				return nil, err
			}
			reason = "connection error"
		case retryable(resp.StatusCode) && attempt < rp.Max:
			debugf("POST %s attempt %d: HTTP %d (%s)", redactURL(url), attempt+1, resp.StatusCode, time.Since(start).Round(time.Millisecond))
			wait = retryAfter(resp.Header)
			reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
			resp.Body.Close()
		default:
			debugf("POST %s attempt %d: HTTP %d, headers in %s", redactURL(url), attempt+1, resp.StatusCode, time.Since(start).Round(time.Millisecond))
			return resp, nil
		}
		if wait == 0 {
			wait = backoff(rp, attempt)
		}
		if wait > rp.Cap {
			wait = rp.Cap
		}
		if fn, ok := ctx.Value(retryNoticeKey{}).(RetryNotice); ok && fn != nil {
			fn(attempt+1, wait, reason)
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// transient reports whether a transport error may succeed on retry:
// refused/reset connections and DNS hiccups. Timeouts are not retried —
// the client timeout is already minutes long, and repeating it would
// turn one slow request into a silent multi-minute hang.
func transient(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	var oe *net.OpError
	var de *net.DNSError
	return errors.As(err, &oe) || errors.As(err, &de) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrUnexpectedEOF)
}

// redactURL drops the query string (some APIs take keys there).
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

func backoff(rp retryPolicy, attempt int) time.Duration {
	d := rp.Base << attempt
	if d <= 0 || d > rp.Cap {
		d = rp.Cap
	}
	// full jitter in [d/2, d)
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// retryAfter reads retry-after-ms, then Retry-After (seconds or HTTP date).
func retryAfter(h http.Header) time.Duration {
	if v := h.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
