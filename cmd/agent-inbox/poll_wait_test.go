package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

// fakeInbox serves an empty inbox until ring is called, then rings any open
// doorbell stream and returns one message from later polls.
type fakeInbox struct {
	polls     atomic.Int32
	streams   atomic.Int32
	delivered atomic.Bool
	bell      chan struct{}
}

func newFakeInbox(t *testing.T) (*fakeInbox, *httptest.Server) {
	f := &fakeInbox{bell: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/events":
			f.streams.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-f.bell:
				_, _ = io.WriteString(w, "id: 1\nevent: doorbell\ndata: {\"message_id\":\"m1\",\"sequence\":1}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			case <-r.Context().Done():
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages":
			f.polls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if f.delivered.Load() {
				_, _ = io.WriteString(w, `{"messages":[{"id":"m1","sequence":1}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"messages":[]}`)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	return f, server
}

func (f *fakeInbox) ring() {
	f.delivered.Store(true)
	close(f.bell)
}

func testKey(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "agent.key")
	if err := inbox.WriteKeyPair(key, filepath.Join(dir, "agent.pub")); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestPollWaitReturnsAfterDoorbell(t *testing.T) {
	f, server := newFakeInbox(t)
	key := testKey(t)
	var out bytes.Buffer
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- run([]string{"poll", "--server", server.URL, "--agent", "agent-a", "--key", key, "--wait", "20s"}, &out, io.Discard)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.polls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	f.ring()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("poll --wait did not return after the doorbell")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("poll --wait took %s", elapsed)
	}
	if !strings.Contains(out.String(), `"id":"m1"`) {
		t.Fatalf("poll --wait printed %q, want the delivered message", out.String())
	}
	if got := f.polls.Load(); got != 2 {
		t.Fatalf("poll --wait polled %d times, want 2 (before and after the doorbell)", got)
	}
}

func TestPollWaitTimesOutThenPolls(t *testing.T) {
	f, server := newFakeInbox(t)
	key := testKey(t)
	var out bytes.Buffer
	start := time.Now()
	if err := run([]string{"poll", "--server", server.URL, "--agent", "agent-a", "--key", key, "--wait", "300ms"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("poll --wait returned after %s, before the wait ended", elapsed)
	}
	if strings.TrimSpace(out.String()) != `{"messages":[]}` {
		t.Fatalf("poll --wait printed %q, want the empty inbox", out.String())
	}
	if got := f.polls.Load(); got < 2 {
		t.Fatalf("poll --wait polled %d times, want a final poll after the wait", got)
	}
}

func TestPollWaitReturnsImmediatelyWhenInboxHasMessages(t *testing.T) {
	f, server := newFakeInbox(t)
	f.ring()
	key := testKey(t)
	var out bytes.Buffer
	start := time.Now()
	if err := run([]string{"poll", "--server", server.URL, "--agent", "agent-a", "--key", key, "--wait", "20s"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second || f.polls.Load() != 1 {
		t.Fatalf("poll --wait waited although a message was ready: polls=%d", f.polls.Load())
	}
}

func TestPollWaitReturnsRejectedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"authentication_failed","message":"request could not be authenticated"}}`)
	}))
	defer server.Close()
	err := run([]string{"poll", "--server", server.URL, "--agent", "agent-a", "--key", testKey(t), "--wait", "5s"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "authentication_failed") {
		t.Fatalf("poll --wait did not report the rejected stream: %v", err)
	}
}

func TestPollWaitRejectsOutOfRangeDurations(t *testing.T) {
	for _, value := range []string{"-1s", "11m"} {
		err := run([]string{"poll", "--server", "http://127.0.0.1:1", "--agent", "agent-a", "--key", testKey(t), "--wait", value}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--wait must be between") {
			t.Fatalf("poll --wait %s returned %v", value, err)
		}
	}
}

func TestArtifactRefPrintsHashAndSize(t *testing.T) {
	dir := t.TempDir()
	content := []byte("draft spec v1\n")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	for _, c := range []struct {
		name, file string
		extra      []string
		wantType   string
	}{
		{name: "explicit type", file: "draft.md", extra: []string{"--media-type", "text/markdown"}, wantType: "text/markdown"},
		{name: "type from extension", file: "draft.json", wantType: "application/json"},
		{name: "unknown extension", file: "draft.bin-unknown", wantType: "application/octet-stream"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.file)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			uri := "https://files.example.com/" + c.file
			var out bytes.Buffer
			args := append([]string{"artifact-ref", "--file", path, "--uri", uri}, c.extra...)
			if err := run(args, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("%s|%s|%s|%d\n", uri, c.wantType, digest, len(content))
			if out.String() != want {
				t.Fatalf("artifact-ref printed %q, want %q", out.String(), want)
			}
		})
	}
}

func TestArtifactRefRejectsUnsafeInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"artifact-ref", "--file", path},
		{"artifact-ref", "--uri", "https://files.example.com/a"},
		{"artifact-ref", "--file", path, "--uri", "relative/path"},
		{"artifact-ref", "--file", path, "--uri", "https://user:pass@files.example.com/a"},
		{"artifact-ref", "--file", path, "--uri", "data:text/plain,hi"},
		{"artifact-ref", "--file", path, "--uri", "https://files.example.com/a|b"},
		{"artifact-ref", "--file", path, "--uri", "https://files.example.com/a", "--media-type", "text/plain|x"},
		{"artifact-ref", "--file", filepath.Join(t.TempDir(), "missing"), "--uri", "https://files.example.com/a"},
	} {
		if err := run(args, io.Discard, io.Discard); err == nil {
			t.Fatalf("artifact-ref accepted %v", args[1:])
		}
	}
}
