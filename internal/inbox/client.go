package inbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Client struct {
	Server  string
	Agent   string
	KeyID   string
	Private ed25519.PrivateKey
	HTTP    *http.Client
}

func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("private key file must contain a base64 Ed25519 private key")
	}
	return ed25519.PrivateKey(key), nil
}

func GenerateKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	return public, private, err
}

func WriteKeyPair(privatePath, publicPath string) error {
	public, private, err := GenerateKeyPair()
	if err != nil {
		return err
	}
	privateFile, err := os.OpenFile(privatePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(privateFile, base64.StdEncoding.EncodeToString(private)); err != nil {
		privateFile.Close()
		os.Remove(privatePath)
		return err
	}
	if err := privateFile.Close(); err != nil {
		os.Remove(privatePath)
		return err
	}
	publicFile, err := os.OpenFile(publicPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		os.Remove(privatePath)
		return err
	}
	if _, err := fmt.Fprintln(publicFile, base64.StdEncoding.EncodeToString(public)); err != nil {
		publicFile.Close()
		os.Remove(privatePath)
		os.Remove(publicPath)
		return err
	}
	return publicFile.Close()
}

func (c *Client) Do(ctx context.Context, method, requestPath string, body []byte) ([]byte, int, error) {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if body == nil {
		body = []byte{}
	}
	timestamp := fmt.Sprintf("%d", time.Now().UTC().Unix())
	nonce, err := NewNonce()
	if err != nil {
		return nil, 0, err
	}
	signature := ed25519.Sign(c.Private, RequestSigningBytes(c.Agent, c.KeyID, method, requestPath, timestamp, nonce, body))
	endpoint := strings.TrimRight(c.Server, "/") + requestPath
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	c.setHeaders(req, timestamp, nonce, base64.StdEncoding.EncodeToString(signature))
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxRequestBytes+1))
	return data, response.StatusCode, err
}

func (c *Client) OpenEvents(ctx context.Context) (*http.Response, error) {
	timestamp := fmt.Sprintf("%d", time.Now().UTC().Unix())
	nonce, err := NewNonce()
	if err != nil {
		return nil, err
	}
	signature := ed25519.Sign(c.Private, RequestSigningBytes(c.Agent, c.KeyID, http.MethodGet, "/v1/events", timestamp, nonce, nil))
	endpoint := strings.TrimRight(c.Server, "/") + "/v1/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, timestamp, nonce, base64.StdEncoding.EncodeToString(signature))
	client := &http.Client{Timeout: 0}
	return client.Do(req)
}

func (c *Client) setHeaders(req *http.Request, timestamp, nonce, signature string) {
	req.Header.Set(HeaderAgent, c.Agent)
	req.Header.Set(HeaderKeyID, c.KeyID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, signature)
}

func PrintAPIError(data []byte, status int) error {
	var response ErrorResponse
	if json.Unmarshal(data, &response) == nil && response.Error.Code != "" {
		return fmt.Errorf("server rejected request (%s, HTTP %d): %s", response.Error.Code, status, response.Error.Message)
	}
	return fmt.Errorf("server returned HTTP %d: %s", status, strings.TrimSpace(string(data)))
}

func NewUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(value[0:4]), hex.EncodeToString(value[4:6]), hex.EncodeToString(value[6:8]), hex.EncodeToString(value[8:10]), hex.EncodeToString(value[10:16])), nil
}

func CopySSE(ctx context.Context, response *http.Response, output io.Writer) error {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, MaxRequestBytes))
		return PrintAPIError(data, response.StatusCode)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), MaxRequestBytes)
	var dataLines []string
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if line == "" && len(dataLines) > 0 {
			if _, err := fmt.Fprintln(output, strings.Join(dataLines, "")); err != nil {
				return err
			}
			dataLines = dataLines[:0]
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}
