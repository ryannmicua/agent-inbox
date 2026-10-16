package inbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// MaxPollWait caps how long a client waits for its inbox to fill.
	MaxPollWait      = 10 * time.Minute
	streamRetryDelay = 2 * time.Second
)

// PollWait polls path and returns the response as soon as the inbox holds a
// message, or after one more poll when the wait ends. It subscribes to the
// doorbell before each poll so a message that arrives between the two is not
// missed. The doorbell is only a prompt: delivery always comes from poll. When
// the doorbell is temporarily unreachable it says so on warn and keeps polling.
func (c *Client) PollWait(ctx context.Context, path string, waitFor time.Duration, warn io.Writer) ([]byte, error) {
	deadline := time.Now().Add(waitFor)
	for {
		events, err := c.openDoorbell(ctx, warn)
		if err != nil {
			return nil, err
		}
		data, err := c.pollOnce(ctx, path)
		if err != nil {
			closeStream(events)
			return nil, err
		}
		ready, err := hasMessages(data)
		remaining := time.Until(deadline)
		if err != nil || ready || remaining <= 0 {
			closeStream(events)
			if err != nil {
				return nil, err
			}
			return data, nil
		}
		if events == nil {
			if err := sleepContext(ctx, min(streamRetryDelay, remaining)); err != nil {
				return nil, err
			}
			continue
		}
		if err := waitForDoorbell(ctx, events, remaining); err != nil {
			return nil, err
		}
	}
}

// RetryableNetworkError reports whether err is a transient network failure
// worth retrying, as opposed to cancellation or a refused request.
func RetryableNetworkError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial" || opErr.Op == "read" || opErr.Op == "write"
	}
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}

// openDoorbell returns nil without an error when the stream is temporarily
// unreachable, so the caller falls back to polling.
func (c *Client) openDoorbell(ctx context.Context, warn io.Writer) (*http.Response, error) {
	response, err := c.OpenEvents(ctx)
	if err != nil {
		var connectErr *EventStreamConnectError
		if errors.As(err, &connectErr) && RetryableNetworkError(connectErr.Err) {
			fmt.Fprintln(warn, "doorbell connection failed; polling without it:", err)
			return nil, nil
		}
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, MaxRequestBytes))
		return nil, PrintAPIError(data, response.StatusCode)
	}
	return response, nil
}

func (c *Client) pollOnce(ctx context.Context, path string) ([]byte, error) {
	data, status, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, PrintAPIError(data, status)
	}
	return data, nil
}

func hasMessages(data []byte) (bool, error) {
	var response PollResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return false, fmt.Errorf("unexpected poll response: %w", err)
	}
	return len(response.Messages) > 0, nil
}

// waitForDoorbell returns when a doorbell event arrives, the stream ends, or
// the timeout passes. It always closes the stream.
func waitForDoorbell(ctx context.Context, events *http.Response, timeout time.Duration) error {
	rang := make(chan struct{})
	go func() {
		defer close(rang)
		scanner := bufio.NewScanner(events.Body)
		scanner.Buffer(make([]byte, 4096), MaxRequestBytes)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data:") {
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case <-rang:
	case <-timer.C:
	case <-ctx.Done():
		err = ctx.Err()
	}
	events.Body.Close()
	<-rang
	return err
}

func closeStream(events *http.Response) {
	if events != nil {
		events.Body.Close()
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
