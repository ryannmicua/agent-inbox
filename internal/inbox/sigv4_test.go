package inbox

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// The examples AWS publishes for Signature Version 4 on S3 ("Signature
// Calculations for the Authorization Header" and "Authenticating Requests:
// Using Query Parameters"). They share one example key pair, region, and time.
var awsS3Example = sigV4{accessKeyID: "AKIAIOSFODNN7EXAMPLE", secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", region: "us-east-1"}

var awsS3ExampleTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

func TestSigV4MatchesAWSHeaderExamples(t *testing.T) {
	cases := []struct {
		name, method, url, payloadHash string
		headers                        map[string]string
		signedHeaders, signature       string
	}{
		{
			name: "GET object", method: http.MethodGet, url: "https://examplebucket.s3.amazonaws.com/test.txt",
			payloadHash: emptyPayloadSHA256, headers: map[string]string{"Range": "bytes=0-9"},
			signedHeaders: "host;range;x-amz-content-sha256;x-amz-date",
			signature:     "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name: "PUT object", method: http.MethodPut, url: "https://examplebucket.s3.amazonaws.com/test$file.text",
			payloadHash:   "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			headers:       map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"},
			signedHeaders: "date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class",
			signature:     "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name: "GET bucket lifecycle", method: http.MethodGet, url: "https://examplebucket.s3.amazonaws.com/?lifecycle",
			payloadHash: emptyPayloadSHA256, signedHeaders: "host;x-amz-content-sha256;x-amz-date",
			signature: "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name: "list objects", method: http.MethodGet, url: "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J",
			payloadHash: emptyPayloadSHA256, signedHeaders: "host;x-amz-content-sha256;x-amz-date",
			signature: "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(c.method, c.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range c.headers {
				req.Header.Set(name, value)
			}
			awsS3Example.sign(req, c.payloadHash, awsS3ExampleTime)
			want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=" + c.signedHeaders + ", Signature=" + c.signature
			if got := req.Header.Get("Authorization"); got != want {
				t.Fatalf("Authorization\n got %s\nwant %s", got, want)
			}
		})
	}
}

func TestSigV4MatchesAWSPresignedExample(t *testing.T) {
	target, err := url.Parse("https://examplebucket.s3.amazonaws.com/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	got := awsS3Example.presign(http.MethodGet, target, nil, awsS3ExampleTime, 24*time.Hour)
	want := "https://examplebucket.s3.amazonaws.com/test.txt" +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got != want {
		t.Fatalf("presigned URL\n got %s\nwant %s", got, want)
	}
}

func TestSigV4EscapesKeysAndOrdersQuery(t *testing.T) {
	if got := canonicalURIPath("/inbox/tenant:one/space/a b+c"); got != "/inbox/tenant%3Aone/space/a%20b%2Bc" {
		t.Fatalf("canonical path %q", got)
	}
	query := url.Values{"X-Amz-Date2": {"b"}, "X-Amz-Date": {"a"}, "k": {"2", "1"}}
	if got := canonicalQueryString(query); got != "X-Amz-Date=a&X-Amz-Date2=b&k=1&k=2" {
		t.Fatalf("canonical query %q", got)
	}
}
