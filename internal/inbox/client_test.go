package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientDoReadsFullMaximumPollResponse(t *testing.T) {
	payload := json.RawMessage(`"` + strings.Repeat("x", MaxPayloadBytes-2) + `"`)
	messages := make([]DeliveredMessage, 100)
	for index := range messages {
		id, err := NewUUID()
		if err != nil {
			t.Fatal(err)
		}
		messages[index] = DeliveredMessage{
			Envelope: Envelope{
				Type: EnvelopeType, ID: id, TaskID: id, ThreadID: id,
				SenderID: "agent-a", RecipientID: "agent-b", KeyID: "primary", Kind: "instruction",
				CreatedAt: "2026-01-01T00:00:00Z", ContentIsData: true, Payload: payload, Artifacts: []Artifact{},
			},
			TenantID: "tenant-one", Sequence: int64(index + 1), AcceptedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	want, err := json.Marshal(PollResponse{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) <= MaxRequestBytes {
		t.Fatalf("poll fixture is only %d bytes", len(want))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(want)
	}))
	defer server.Close()
	_, private, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{Server: server.URL, Agent: "agent-b", KeyID: "primary", Private: private}
	got, status, err := client.Do(context.Background(), http.MethodGet, "/v1/messages?limit=100", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("maximum poll response failed: HTTP %d err=%v", status, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("client returned %d bytes, want complete %d-byte poll response", len(got), len(want))
	}
}
