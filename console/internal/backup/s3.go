package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bucket is an S3-compatible bucket, reached with Signature Version 4: Amazon's, or any other that answers as it does,
// such as Backblaze B2, Cloudflare R2, Wasabi, OVHcloud, or Garage or MinIO on a NAS.
type Bucket struct {
	Endpoint  string // https://host[:port], or http on the unit's own network
	Region    string
	Name      string
	AccessKey string
	SecretKey string
	PathStyle bool // https://host/bucket/key rather than https://bucket.host/key
	Client    *http.Client

	mu   sync.Mutex
	skew time.Duration // how far the bucket's clock is from the unit's, learned from a refusal
}

// Object is one thing in a bucket, as a listing gives it.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// PartSize is how much of a backup goes in each request. A part is held in memory while it's sent, and S3 takes at
// most 10,000 parts, so a backup can be up to 156 GiB.
const PartSize = 16 << 20

const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (b *Bucket) client() *http.Client {
	if b.Client != nil {
		return b.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (b *Bucket) endpoint() (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(b.Endpoint, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" {
		return nil, errors.New("that isn't a bucket's address")
	}
	return u, nil
}

func (b *Bucket) target(key string, query url.Values) (*url.URL, error) {
	u, err := b.endpoint()
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(u.Path, "/")
	if b.PathStyle {
		u.Path = base + "/" + b.Name
		if key != "" {
			u.Path += "/" + key
		}
	} else {
		u.Host = b.Name + "." + u.Host
		u.Path = base + "/" + key
	}
	u.RawPath = encodePath(u.Path)
	u.RawQuery = canonicalQuery(query)
	return u, nil
}

// encodePath is a path as SigV4 wants it: each segment percent-encoded but for RFC 3986's unreserved characters.
func encodePath(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = encode(s)
	}
	return strings.Join(segments, "/")
}

func encode(s string) string {
	var out strings.Builder
	for _, c := range []byte(s) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			out.WriteByte(c)
		} else {
			fmt.Fprintf(&out, "%%%02X", c)
		}
	}
	return out.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		values := append([]string(nil), q[k]...)
		sort.Strings(values)
		for _, v := range values {
			parts = append(parts, encode(k)+"="+encode(v))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// sign adds SigV4's headers to a request whose body has the given hash.
func (b *Bucket) sign(req *http.Request, payloadHash string, at time.Time) {
	stamp := at.UTC().Format("20060102T150405Z")
	day := stamp[:8]
	region := b.Region
	if region == "" {
		region = "us-east-1"
	}
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if req.Header.Get("Content-Type") != "" {
		signed = append(signed, "content-type")
		sort.Strings(signed)
	}
	var headers strings.Builder
	for _, name := range signed {
		value := req.Host
		if name != "host" {
			value = strings.TrimSpace(req.Header.Get(name))
		}
		headers.WriteString(name + ":" + value + "\n")
	}
	canonical := strings.Join([]string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, headers.String(),
		strings.Join(signed, ";"), payloadHash}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA256([]byte("AWS4"+b.SecretKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		b.AccessKey, scope, strings.Join(signed, ";"), hex.EncodeToString(hmacSHA256(key, toSign))))
}

// Refusal is what a bucket said when it refused a request.
type Refusal struct {
	Status  int
	Code    string
	Message string
}

func (r *Refusal) Error() string {
	switch r.Code {
	case "NoSuchBucket":
		return "there's no such bucket"
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "AccessDenied", "InvalidToken":
		return "the bucket refused the key (" + r.Code + ")"
	case "RequestTimeTooSkewed":
		return "the bucket refused the unit's clock"
	}
	if r.Code != "" {
		return fmt.Sprintf("the bucket said %s: %s", r.Code, r.Message)
	}
	return fmt.Sprintf("the bucket answered %d", r.Status)
}

func refusalOf(resp *http.Response) *Refusal {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	xml.Unmarshal(data, &e)
	return &Refusal{Status: resp.StatusCode, Code: e.Code, Message: e.Message}
}

// do sends one request with a body held in memory. A refusal of the unit's clock is learned from and tried again
// once: a unit with no time server can be minutes out, and a bucket refuses more than 15.
func (b *Bucket) do(ctx context.Context, method, key string, query url.Values, body []byte, contentType string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		u, err := b.target(key, query)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.URL = u
		req.ContentLength = int64(len(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		hash := emptyHash
		if len(body) > 0 {
			sum := sha256.Sum256(body)
			hash = hex.EncodeToString(sum[:])
		}
		b.mu.Lock()
		skew := b.skew
		b.mu.Unlock()
		b.sign(req, hash, time.Now().Add(skew))
		resp, err := b.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("the bucket didn't answer: %w", unwrapURL(err))
		}
		if resp.StatusCode < 300 {
			return resp, nil
		}
		refusal := refusalOf(resp)
		resp.Body.Close()
		if refusal.Code == "RequestTimeTooSkewed" && attempt == 0 {
			if at, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
				b.mu.Lock()
				b.skew = time.Until(at)
				b.mu.Unlock()
				continue
			}
		}
		return nil, refusal
	}
}

func unwrapURL(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

// List is everything in the bucket under prefix.
func (b *Bucket) List(ctx context.Context, prefix string) ([]Object, error) {
	var objects []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := b.do(ctx, http.MethodGet, "", q, nil, "")
		if err != nil {
			return nil, err
		}
		var result struct {
			Contents []struct {
				Key          string `xml:"Key"`
				Size         int64  `xml:"Size"`
				LastModified string `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return nil, errors.New("the bucket's listing couldn't be read")
		}
		for _, c := range result.Contents {
			at, _ := time.Parse(time.RFC3339, c.LastModified)
			objects = append(objects, Object{Key: c.Key, Size: c.Size, Modified: at})
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			return objects, nil
		}
		token = result.NextContinuationToken
	}
}

// Put stores a small object.
func (b *Bucket) Put(ctx context.Context, key string, data []byte, contentType string) error {
	resp, err := b.do(ctx, http.MethodPut, key, nil, data, contentType)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Read is a small object, up to limit bytes.
func (b *Bucket) Read(ctx context.Context, key string, limit int64) ([]byte, error) {
	body, _, err := b.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("that object is bigger than it should be")
	}
	return data, err
}

// Get opens an object, and says how big it is.
func (b *Bucket) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := b.do(ctx, http.MethodGet, key, nil, nil, "")
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

func (b *Bucket) Delete(ctx context.Context, key string) error {
	resp, err := b.do(ctx, http.MethodDelete, key, nil, nil, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Upload sends what r gives, as a multipart upload, and says how big it was and its SHA-256. Before it finishes the
// upload, it asks done whether what it sent was all good: until then, the object isn't there. Any failure leaves
// nothing in the bucket.
func (b *Bucket) Upload(ctx context.Context, key string, r io.Reader, done func() error, progress func(int64)) (int64, string, error) {
	resp, err := b.do(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, "application/octet-stream")
	if err != nil {
		return 0, "", err
	}
	var started struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&started)
	resp.Body.Close()
	if err != nil || started.UploadID == "" {
		return 0, "", errors.New("the bucket didn't start the upload")
	}
	abort := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if resp, err := b.do(ctx, http.MethodDelete, key, url.Values{"uploadId": {started.UploadID}}, nil, ""); err == nil {
			resp.Body.Close()
		}
	}

	type part struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	var parts []part
	hash := sha256.New()
	var size int64
	buffer := make([]byte, PartSize)
	for number := 1; ; number++ {
		n, readErr := io.ReadFull(r, buffer)
		if n == 0 && number > 1 {
			break
		}
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			abort()
			return 0, "", readErr
		}
		if number > 10000 {
			abort()
			return 0, "", errors.New("that backup is too big for one object")
		}
		hash.Write(buffer[:n])
		size += int64(n)
		var etag string
		for attempt := 0; ; attempt++ {
			resp, err := b.do(ctx, http.MethodPut, key, url.Values{"partNumber": {strconv.Itoa(number)}, "uploadId": {started.UploadID}},
				buffer[:n], "")
			if err == nil {
				etag = resp.Header.Get("ETag")
				resp.Body.Close()
				break
			}
			if attempt == 2 || ctx.Err() != nil {
				abort()
				return 0, "", err
			}
			time.Sleep(time.Duration(attempt+1) * 5 * time.Second)
		}
		parts = append(parts, part{Number: number, ETag: etag})
		if progress != nil {
			progress(size)
		}
		if readErr != nil {
			break
		}
	}
	if err := done(); err != nil {
		abort()
		return 0, "", err
	}
	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}{Parts: parts})
	resp, err = b.do(ctx, http.MethodPost, key, url.Values{"uploadId": {started.UploadID}}, body, "application/xml")
	if err != nil {
		abort()
		return 0, "", err
	}
	// S3 can answer 200 and still have failed, with an error in the body.
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if bytes.Contains(data, []byte("<Error>")) {
		abort()
		var e struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		xml.Unmarshal(data, &e)
		return 0, "", &Refusal{Status: 200, Code: e.Code, Message: e.Message}
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}
