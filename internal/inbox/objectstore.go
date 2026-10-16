package inbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrObjectNotFound reports that the object store holds no object at a key.
var ErrObjectNotFound = errors.New("object not found")

// ObjectStore is the boundary to the private bucket that holds artifact bytes.
// Agents move bytes directly to and from it through short-lived presigned
// URLs; the inbox itself only reads objects back to verify them.
type ObjectStore interface {
	// PresignPut returns a URL that accepts a PUT of exactly size bytes to key
	// until now+ttl.
	PresignPut(key string, size int64, now time.Time, ttl time.Duration) (string, error)
	// PresignGet returns a URL that serves key until now+ttl.
	PresignGet(key string, now time.Time, ttl time.Duration) (string, error)
	// Stat returns the stored object's size, or ErrObjectNotFound.
	Stat(ctx context.Context, key string) (int64, error)
	// Open returns the stored object's bytes, or ErrObjectNotFound.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes key; deleting a missing object succeeds.
	Delete(ctx context.Context, key string) error
}

// S3Config names one bucket on an S3-compatible service such as Garage.
// Credentials come from the operator's environment, never from the repository.
type S3Config struct {
	Endpoint        string
	Bucket          string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// HTTP is used for the inbox's own Stat, Open, and Delete calls.
	HTTP *http.Client
}

// S3ObjectStore addresses objects path-style, as endpoint/bucket/key, and
// signs every request with SigV4.
type S3ObjectStore struct {
	endpoint *url.URL
	bucket   string
	signer   sigV4
	http     *http.Client
}

var s3BucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func NewS3ObjectStore(config S3Config) (*S3ObjectStore, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Hostname() == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, errors.New("object store endpoint must be an absolute HTTP or HTTPS URL with no path, query, or credentials")
	}
	// Clients omit a default port from Host, so a signed host must omit it too.
	if port := endpoint.Port(); (endpoint.Scheme == "https" && port == "443") || (endpoint.Scheme == "http" && port == "80") {
		endpoint.Host = endpoint.Hostname()
		if strings.Contains(endpoint.Host, ":") {
			endpoint.Host = "[" + endpoint.Host + "]"
		}
	}
	endpoint.Path = ""
	if !s3BucketPattern.MatchString(config.Bucket) {
		return nil, errors.New("object store bucket must be a valid S3 bucket name")
	}
	if config.Region == "" {
		return nil, errors.New("object store region is required")
	}
	if config.AccessKeyID == "" || config.SecretAccessKey == "" {
		return nil, errors.New("object store access key ID and secret access key are required")
	}
	client := config.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &S3ObjectStore{
		endpoint: endpoint, bucket: config.Bucket, http: noRedirectClient(client),
		signer: sigV4{accessKeyID: config.AccessKeyID, secretAccessKey: config.SecretAccessKey, sessionToken: config.SessionToken, region: config.Region},
	}, nil
}

// Host returns the endpoint host agents are sent to, for startup logs.
func (s *S3ObjectStore) Host() string { return s.endpoint.Host }

func (s *S3ObjectStore) objectURL(key string) *url.URL {
	target := *s.endpoint
	target.Path = "/" + s.bucket + "/" + key
	target.RawPath = canonicalURIPath(target.Path)
	return &target
}

func (s *S3ObjectStore) PresignPut(key string, size int64, now time.Time, ttl time.Duration) (string, error) {
	if err := checkPresignTTL(ttl); err != nil {
		return "", err
	}
	return s.signer.presign(http.MethodPut, s.objectURL(key), map[string]string{"content-length": strconv.FormatInt(size, 10)}, now, ttl), nil
}

func (s *S3ObjectStore) PresignGet(key string, now time.Time, ttl time.Duration) (string, error) {
	if err := checkPresignTTL(ttl); err != nil {
		return "", err
	}
	return s.signer.presign(http.MethodGet, s.objectURL(key), nil, now, ttl), nil
}

func checkPresignTTL(ttl time.Duration) error {
	if ttl < time.Second || ttl > maxPresignTTL {
		return fmt.Errorf("presigned URL lifetime must be between 1s and %s", maxPresignTTL)
	}
	return nil
}

func (s *S3ObjectStore) do(ctx context.Context, method, key string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.objectURL(key).String(), nil)
	if err != nil {
		return nil, err
	}
	s.signer.sign(req, emptyPayloadSHA256, time.Now())
	response, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("object store %s: %w", method, err)
	}
	return response, nil
}

func (s *S3ObjectStore) Stat(ctx context.Context, key string) (int64, error) {
	response, err := s.do(ctx, http.MethodHead, key)
	if err != nil {
		return 0, err
	}
	response.Body.Close()
	switch {
	case response.StatusCode == http.StatusNotFound:
		return 0, ErrObjectNotFound
	case response.StatusCode != http.StatusOK:
		return 0, fmt.Errorf("object store HEAD returned HTTP %d", response.StatusCode)
	case response.ContentLength < 0:
		return 0, errors.New("object store HEAD returned no content length")
	}
	return response.ContentLength, nil
}

func (s *S3ObjectStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	response, err := s.do(ctx, http.MethodGet, key)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusOK {
		return response.Body, nil
	}
	response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrObjectNotFound
	}
	return nil, fmt.Errorf("object store GET returned HTTP %d", response.StatusCode)
}

func (s *S3ObjectStore) Delete(ctx context.Context, key string) error {
	response, err := s.do(ctx, http.MethodDelete, key)
	if err != nil {
		return err
	}
	response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("object store DELETE returned HTTP %d", response.StatusCode)
}

// MemoryObjectStore is an in-process ObjectStore for tests and local trials.
// It serves its own presigned URLs: mount it as an HTTP handler and set
// BaseURL to where it is reachable. Presigned URLs carry an HMAC over the
// method, key, size, and expiry, so they can be neither altered nor reused
// past their lifetime.
type MemoryObjectStore struct {
	BaseURL string
	// Now is the clock that presigned URLs expire against.
	Now func() time.Time

	mu      sync.Mutex
	objects map[string][]byte
	secret  []byte
}

func NewMemoryObjectStore() *MemoryObjectStore {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic(err)
	}
	return &MemoryObjectStore{Now: time.Now, objects: map[string][]byte{}, secret: secret}
}

// Put stores data at key directly, as a test fixture.
func (m *MemoryObjectStore) Put(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = bytes.Clone(data)
}

// Object returns the bytes stored at key.
func (m *MemoryObjectStore) Object(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	return bytes.Clone(data), ok
}

func (m *MemoryObjectStore) presign(method, key string, size int64, now time.Time, ttl time.Duration) (string, error) {
	if err := checkPresignTTL(ttl); err != nil {
		return "", err
	}
	if m.BaseURL == "" {
		return "", errors.New("memory object store has no BaseURL")
	}
	expires := strconv.FormatInt(now.Add(ttl).Unix(), 10)
	sizeText := strconv.FormatInt(size, 10)
	query := url.Values{"expires": {expires}, "size": {sizeText}, "signature": {m.mac(method, key, sizeText, expires)}}
	return strings.TrimRight(m.BaseURL, "/") + canonicalURIPath("/"+key) + "?" + query.Encode(), nil
}

func (m *MemoryObjectStore) mac(method, key, size, expires string) string {
	return hex.EncodeToString(hmacSHA256(m.secret, strings.Join([]string{method, key, size, expires}, "\n")))
}

func (m *MemoryObjectStore) PresignPut(key string, size int64, now time.Time, ttl time.Duration) (string, error) {
	return m.presign(http.MethodPut, key, size, now, ttl)
}

func (m *MemoryObjectStore) PresignGet(key string, now time.Time, ttl time.Duration) (string, error) {
	return m.presign(http.MethodGet, key, -1, now, ttl)
}

func (m *MemoryObjectStore) Stat(_ context.Context, key string) (int64, error) {
	data, ok := m.Object(key)
	if !ok {
		return 0, ErrObjectNotFound
	}
	return int64(len(data)), nil
}

func (m *MemoryObjectStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := m.Object(key)
	if !ok {
		return nil, ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *MemoryObjectStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// ServeHTTP accepts the requests its presigned URLs authorize.
func (m *MemoryObjectStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	query := r.URL.Query()
	expires, size := query.Get("expires"), query.Get("size")
	expiry, err := strconv.ParseInt(expires, 10, 64)
	signature, _ := hex.DecodeString(query.Get("signature"))
	expected, _ := hex.DecodeString(m.mac(r.Method, key, size, expires))
	if err != nil || !hmac.Equal(signature, expected) {
		http.Error(w, "signature does not match", http.StatusForbidden)
		return
	}
	if m.Now().Unix() > expiry {
		http.Error(w, "request has expired", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPut:
		want, _ := strconv.ParseInt(size, 10, 64)
		if r.ContentLength != want {
			http.Error(w, "content length does not match the signed size", http.StatusForbidden)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, want+1))
		if err != nil || int64(len(data)) != want {
			http.Error(w, "body does not match the signed size", http.StatusBadRequest)
			return
		}
		m.Put(key, data)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		data, ok := m.Object(key)
		if !ok {
			http.Error(w, "no such key", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
