package inbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sigV4 signs S3 requests with AWS Signature Version 4, either as a presigned
// URL (query authentication) or as an Authorization header. It implements only
// what the object store needs: the s3 service, path-style URLs, and S3's
// single-encoded canonical paths. The test file checks it against the examples
// AWS publishes for S3.
type sigV4 struct {
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
	region          string
}

const (
	sigV4Algorithm     = "AWS4-HMAC-SHA256"
	sigV4TimeFormat    = "20060102T150405Z"
	unsignedPayload    = "UNSIGNED-PAYLOAD"
	emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	// maxPresignTTL is the longest validity SigV4 allows a presigned URL.
	maxPresignTTL = 7 * 24 * time.Hour
)

// presign returns rawURL with query authentication for method, valid for ttl
// from now. headers are extra headers the eventual request must carry exactly
// (lowercase names); host is always signed.
func (s sigV4) presign(method string, target *url.URL, headers map[string]string, now time.Time, ttl time.Duration) string {
	now = now.UTC()
	names, canonicalHeaders := canonicalHeaderBlock(target.Host, headers)
	query := target.Query()
	query.Set("X-Amz-Algorithm", sigV4Algorithm)
	query.Set("X-Amz-Credential", s.accessKeyID+"/"+s.scope(now))
	query.Set("X-Amz-Date", now.Format(sigV4TimeFormat))
	query.Set("X-Amz-Expires", strconv.FormatInt(int64(ttl/time.Second), 10))
	query.Set("X-Amz-SignedHeaders", names)
	if s.sessionToken != "" {
		query.Set("X-Amz-Security-Token", s.sessionToken)
	}
	canonicalQuery := canonicalQueryString(query)
	path := canonicalURIPath(target.Path)
	request := strings.Join([]string{method, path, canonicalQuery, canonicalHeaders, names, unsignedPayload}, "\n")
	signature := s.signature(now, request)
	return target.Scheme + "://" + target.Host + path + "?" + canonicalQuery + "&X-Amz-Signature=" + signature
}

// sign adds header authentication to req for a body whose SHA-256 is
// payloadHash. Every header already set on req is signed, plus host.
func (s sigV4) sign(req *http.Request, payloadHash string, now time.Time) {
	now = now.UTC()
	req.Header.Set("X-Amz-Date", now.Format(sigV4TimeFormat))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if s.sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.sessionToken)
	}
	headers := make(map[string]string, len(req.Header))
	for name, values := range req.Header {
		headers[strings.ToLower(name)] = strings.Join(values, ",")
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	names, canonicalHeaders := canonicalHeaderBlock(host, headers)
	request := strings.Join([]string{req.Method, canonicalURIPath(req.URL.Path), canonicalQueryString(req.URL.Query()), canonicalHeaders, names, payloadHash}, "\n")
	req.Header.Set("Authorization", sigV4Algorithm+" Credential="+s.accessKeyID+"/"+s.scope(now)+", SignedHeaders="+names+", Signature="+s.signature(now, request))
}

func (s sigV4) scope(now time.Time) string {
	return now.Format("20060102") + "/" + s.region + "/s3/aws4_request"
}

func (s sigV4) signature(now time.Time, canonicalRequest string) string {
	digest := sha256.Sum256([]byte(canonicalRequest))
	toSign := strings.Join([]string{sigV4Algorithm, now.Format(sigV4TimeFormat), s.scope(now), hex.EncodeToString(digest[:])}, "\n")
	key := hmacSHA256([]byte("AWS4"+s.secretAccessKey), now.Format("20060102"))
	for _, part := range []string{s.region, "s3", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	return hex.EncodeToString(hmacSHA256(key, toSign))
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// canonicalHeaderBlock returns the sorted signed header names and the
// canonical header lines (each newline-terminated) for host plus headers.
func canonicalHeaderBlock(host string, headers map[string]string) (string, string) {
	values := map[string]string{"host": host}
	for name, value := range headers {
		values[strings.ToLower(name)] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var block strings.Builder
	for _, name := range names {
		block.WriteString(name + ":" + strings.Join(strings.Fields(values[name]), " ") + "\n")
	}
	return strings.Join(names, ";"), block.String()
}

// canonicalURIPath URI-encodes each path segment once, as S3 expects.
func canonicalURIPath(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = sigV4Escape(segment)
	}
	return strings.Join(segments, "/")
}

func canonicalQueryString(query url.Values) string {
	type pair struct{ name, value string }
	pairs := make([]pair, 0, len(query))
	for name, values := range query {
		for _, value := range values {
			pairs = append(pairs, pair{sigV4Escape(name), sigV4Escape(value)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].name != pairs[j].name {
			return pairs[i].name < pairs[j].name
		}
		return pairs[i].value < pairs[j].value
	})
	encoded := make([]string, len(pairs))
	for i, p := range pairs {
		encoded[i] = p.name + "=" + p.value
	}
	return strings.Join(encoded, "&")
}

// sigV4Escape percent-encodes every byte except the RFC 3986 unreserved
// characters, with uppercase hex.
func sigV4Escape(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			out.WriteByte(c)
			continue
		}
		out.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return out.String()
}
