package inbox

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestWebhookFailureLogOmitsURLCredentials(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const rawURL = "https://operator-name:operator-password@hooks.example.test/services/path-secret?token=query-secret"
	requestedURL := make(chan string, 1)
	notifier := WebhookNotifier{
		URL: rawURL,
		Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requestedURL <- request.URL.String()
			return nil, errors.New("connection refused")
		})},
	}
	server := &Server{store: store, notifier: notifier}

	previousOutput := log.Writer()
	var output bytes.Buffer
	log.SetOutput(&output)
	defer log.SetOutput(previousOutput)
	server.notify(Notification{Event: "message.accepted"})

	select {
	case requested := <-requestedURL:
		if !strings.Contains(requested, "/services/path-secret") || !strings.Contains(requested, "token=query-secret") {
			t.Fatalf("credentialed webhook URL was not sent: %q", requested)
		}
	default:
		t.Fatal("credentialed webhook URL was not sent")
	}
	logged := output.String()
	for _, secret := range []string{"operator-name", "operator-password", "path-secret", "query-secret"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("webhook log disclosed %q: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, "https://hooks.example.test") || strings.Contains(logged, "/services") {
		t.Fatalf("webhook failure log omitted the safe endpoint: %s", logged)
	}
}
