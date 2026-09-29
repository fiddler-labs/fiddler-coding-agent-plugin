package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/config"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
)

// DefaultTimeout bounds a normal export. It is deliberately short: Claude Code
// blocks the developer's session until the hook process exits, so a slow or
// unreachable endpoint stalls the session for up to this duration on every
// emitting hook. Two seconds bounds that worst-case while allowing the
// happy-path POST to complete.
const DefaultTimeout = 2 * time.Second

// httpClient is shared across exports within a single hook process. It has no
// client-level timeout; each export bounds its total duration with a per-request
// context deadline instead (see Send / SendWithTimeout), so callers under a
// tighter hook budget can shorten it.
//
// The transport adds short per-phase timeouts underneath that overall deadline.
// A context deadline alone lets one phase consume the whole budget: an
// unreachable endpoint (wrong host, no route) makes the dial hang for the full
// deadline, so the session stalls that long at turn end. The dial timeout caps
// connecting at ~1s so a dead endpoint fails fast, while a live endpoint still
// gets the full context deadline to upload a large batch.
var httpClient = newHTTPClient()

// newHTTPClient clones http.DefaultTransport (preserving proxy-from-environment,
// HTTP/2, and the default connection pool) and overrides only the connect-phase
// timeouts, so a dead endpoint fails in ~1s while the overall context deadline
// still governs total request time.
func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: time.Second}).DialContext
	t.TLSHandshakeTimeout = 2 * time.Second
	t.ResponseHeaderTimeout = 5 * time.Second
	return &http.Client{Transport: t}
}

// Send serializes and POSTs the given spans to the configured Fiddler
// OTLP/HTTP endpoint under DefaultTimeout. Returns an error on failure but
// callers should treat errors as non-fatal (fail open).
func Send(cfg *config.Config, spans []Span) error {
	return SendWithTimeout(cfg, spans, DefaultTimeout)
}

// SendWithTimeout is Send with an explicit deadline. The SessionEnd flush uses
// a deadline below the ~1.5s SessionEnd hook budget so a slow endpoint makes
// the export fail open cleanly rather than being SIGKILLed mid-POST.
func SendWithTimeout(cfg *config.Config, spans []Span, timeout time.Duration) error {
	payload := BuildPayload(cfg, spans)
	body, err := MarshalPayload(payload)
	if err != nil {
		return fmt.Errorf("marshal OTLP payload: %w", err)
	}

	// gzip the OTLP/JSON body. The turn-end batch carries one chat span per LLM
	// call, each re-including the cumulative gen_ai.input.messages history, so the
	// payload is highly repetitive and compresses well — this cuts bytes on the
	// wire (and export time) far more than it costs to compress. OTLP/HTTP signals
	// this with Content-Encoding: gzip.
	gzBody, err := gzipBytes(body)
	if err != nil {
		return fmt.Errorf("gzip OTLP payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	url := cfg.Endpoint + "/v1/traces"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(gzBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Fiddler-Application-Id", cfg.AppID)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("OTLP POST to %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("OTLP POST rejected: HTTP %d", resp.StatusCode)
	}

	filelog.Info("OTLP POST ok: HTTP %d, %d span(s), %d->%d bytes gzip to %s",
		resp.StatusCode, len(spans), len(body), len(gzBody), url)
	return nil
}

// gzipBytes returns data gzip-compressed. Used for the OTLP request body.
func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
