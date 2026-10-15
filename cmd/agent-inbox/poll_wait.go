package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

const (
	maxPollWait      = 10 * time.Minute
	streamRetryDelay = 2 * time.Second
)

// runPollWait returns as soon as the inbox holds a message, or polls once more
// when the wait ends. It subscribes to the doorbell before each poll so a
// message that arrives between the two is not missed. The doorbell is only a
// prompt: delivery always comes from poll.
func runPollWait(ctx context.Context, client *inbox.Client, path string, waitFor time.Duration, stdout, stderr io.Writer) error {
	deadline := time.Now().Add(waitFor)
	for {
		events, err := openDoorbell(ctx, client, stderr)
		if err != nil {
			return err
		}
		data, err := pollOnce(ctx, client, path)
		if err != nil {
			closeStream(events)
			return err
		}
		ready, err := hasMessages(data)
		remaining := time.Until(deadline)
		if err != nil || ready || remaining <= 0 {
			closeStream(events)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, strings.TrimSpace(string(data)))
			return err
		}
		if events == nil {
			if err := sleepContext(ctx, min(streamRetryDelay, remaining)); err != nil {
				return err
			}
			continue
		}
		if err := waitForDoorbell(ctx, events, remaining); err != nil {
			return err
		}
	}
}

// openDoorbell returns nil without an error when the stream is temporarily
// unreachable, so the caller falls back to polling.
func openDoorbell(ctx context.Context, client *inbox.Client, stderr io.Writer) (*http.Response, error) {
	response, err := client.OpenEvents(ctx)
	if err != nil {
		var connectErr *inbox.EventStreamConnectError
		if errors.As(err, &connectErr) && retryableNetworkError(connectErr.Err) {
			fmt.Fprintln(stderr, "doorbell connection failed; polling without it:", err)
			return nil, nil
		}
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, inbox.MaxRequestBytes))
		return nil, inbox.PrintAPIError(data, response.StatusCode)
	}
	return response, nil
}

func pollOnce(ctx context.Context, client *inbox.Client, path string) ([]byte, error) {
	data, status, err := client.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, inbox.PrintAPIError(data, status)
	}
	return data, nil
}

func hasMessages(data []byte) (bool, error) {
	var response inbox.PollResponse
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
		scanner.Buffer(make([]byte, 4096), inbox.MaxRequestBytes)
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
