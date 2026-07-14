package market

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// artifactStorage keeps artifact bytes outside the database. ObjectID is the
// value persisted in artifacts.path: an absolute local path or an S3 key.
type artifactStorage interface {
	ObjectID(relative string) (string, error)
	Put(context.Context, string, string, string, string) error
	PresignGet(context.Context, string) (string, error)
	LocalPath(objectID string) (string, bool)
}

func newArtifactStorage(cfg Config) (artifactStorage, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.ArtifactStorage)) {
	case "", "local":
		if strings.TrimSpace(cfg.ArtifactRoot) == "" {
			return nil, errors.New("artifact root is required for local storage")
		}
		return newLocalArtifactStorage(cfg.ArtifactRoot)
	case "s3":
		return newS3ArtifactStorage(cfg)
	default:
		return nil, fmt.Errorf("unsupported artifact storage %q", cfg.ArtifactStorage)
	}
}

type localArtifactStorage struct{ root string }

func newLocalArtifactStorage(root string) (*localArtifactStorage, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &localArtifactStorage{root: abs}, nil
}

func (s *localArtifactStorage) ObjectID(relative string) (string, error) {
	clean, err := cleanArtifactRelativePath(relative)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, filepath.FromSlash(clean)), nil
}

func (s *localArtifactStorage) Put(_ context.Context, objectID, sourcePath, _ string, _ string) error {
	if err := os.MkdirAll(filepath.Dir(objectID), 0o755); err != nil {
		return err
	}
	return copyFile(sourcePath, objectID)
}

func (s *localArtifactStorage) PresignGet(_ context.Context, _ string) (string, error) {
	return "", errors.New("local artifact storage does not support presigned URLs")
}

func (s *localArtifactStorage) LocalPath(objectID string) (string, bool) {
	resolved, err := filepath.Abs(objectID)
	if err != nil || !strings.HasPrefix(resolved, s.root+string(filepath.Separator)) {
		return "", false
	}
	return resolved, true
}

type s3ArtifactStorage struct {
	bucket       string
	region       string
	prefix       string
	endpoint     *url.URL
	accessKeyID  string
	secretKey    string
	sessionToken string
	presignTTL   time.Duration
	client       *http.Client
}

func newS3ArtifactStorage(cfg Config) (*s3ArtifactStorage, error) {
	if strings.TrimSpace(cfg.S3Bucket) == "" {
		return nil, errors.New("S3 bucket is required when MARKET_ARTIFACT_STORAGE=s3")
	}
	if strings.TrimSpace(cfg.S3AccessKeyID) == "" || strings.TrimSpace(cfg.S3SecretAccessKey) == "" {
		return nil, errors.New("S3 access key ID and secret access key are required when MARKET_ARTIFACT_STORAGE=s3")
	}
	region := strings.TrimSpace(cfg.S3Region)
	if region == "" {
		region = "us-east-1"
	}
	endpointText := strings.TrimSpace(cfg.S3Endpoint)
	if endpointText == "" {
		endpointText = "https://" + cfg.S3Bucket + ".s3." + region + ".amazonaws.com"
	}
	endpoint, err := url.Parse(endpointText)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q", endpointText)
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.S3Prefix), "/")
	if prefix != "" {
		if _, err := cleanArtifactRelativePath(prefix); err != nil {
			return nil, fmt.Errorf("invalid S3 prefix: %w", err)
		}
	}
	ttl := cfg.S3PresignTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if ttl > 7*24*time.Hour {
		return nil, errors.New("S3 presign TTL cannot exceed 7 days")
	}
	return &s3ArtifactStorage{
		bucket: cfg.S3Bucket, region: region, prefix: prefix, endpoint: endpoint,
		accessKeyID: cfg.S3AccessKeyID, secretKey: cfg.S3SecretAccessKey,
		sessionToken: cfg.S3SessionToken, presignTTL: ttl, client: &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

func (s *s3ArtifactStorage) ObjectID(relative string) (string, error) {
	clean, err := cleanArtifactRelativePath(relative)
	if err != nil {
		return "", err
	}
	if s.prefix == "" {
		return clean, nil
	}
	return s.prefix + "/" + clean, nil
}

func (s *s3ArtifactStorage) Put(ctx context.Context, objectID, sourcePath, contentType, checksum string) error {
	file, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer file.Close()
	reqURL := s.objectURL(objectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL.String(), file)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-amz-content-sha256", checksum)
	if err := s.signRequest(req, checksum, time.Now().UTC()); err != nil {
		return err
	}
	response, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("S3 put %q failed: %s: %s", objectID, response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *s3ArtifactStorage) PresignGet(_ context.Context, objectID string) (string, error) {
	u := s.objectURL(objectID)
	now := time.Now().UTC()
	date := now.Format("20060102")
	stamp := now.Format("20060102T150405Z")
	credential := s.accessKeyID + "/" + date + "/" + s.region + "/s3/aws4_request"
	query := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    credential,
		"X-Amz-Date":          stamp,
		"X-Amz-Expires":       fmt.Sprintf("%d", int(s.presignTTL.Seconds())),
		"X-Amz-SignedHeaders": "host",
	}
	if s.sessionToken != "" {
		query["X-Amz-Security-Token"] = s.sessionToken
	}
	canonicalQueryText := canonicalQuery(query)
	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		u.EscapedPath(),
		canonicalQueryText,
		"host:" + u.Host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	credentialScope := date + "/" + s.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", stamp, credentialScope, sha256Hex(canonicalRequest),
	}, "\n")
	query["X-Amz-Signature"] = hex.EncodeToString(hmacSHA256(s.signingKey(date), stringToSign))
	u.RawQuery = canonicalQuery(query)
	return u.String(), nil
}

func (s *s3ArtifactStorage) LocalPath(_ string) (string, bool) { return "", false }

func (s *s3ArtifactStorage) objectURL(objectID string) *url.URL {
	u := *s.endpoint
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/") + "/" + objectID
	return &u
}

func (s *s3ArtifactStorage) signRequest(req *http.Request, payloadHash string, now time.Time) error {
	date := now.Format("20060102")
	stamp := now.Format("20060102T150405Z")
	req.Header.Set("x-amz-date", stamp)
	if s.sessionToken != "" {
		req.Header.Set("x-amz-security-token", s.sessionToken)
	}
	headers := map[string]string{"host": req.URL.Host, "x-amz-content-sha256": payloadHash, "x-amz-date": stamp}
	if s.sessionToken != "" {
		headers["x-amz-security-token"] = s.sessionToken
	}
	canonicalHeaders, signedHeaders := canonicalHeaders(headers)
	canonicalRequest := strings.Join([]string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, canonicalHeaders, signedHeaders, payloadHash}, "\n")
	credentialScope := date + "/" + s.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", stamp, credentialScope, sha256Hex(canonicalRequest)}, "\n")
	signature := hex.EncodeToString(hmacSHA256(s.signingKey(date), stringToSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.accessKeyID+"/"+credentialScope+", SignedHeaders="+signedHeaders+", Signature="+signature)
	return nil
}

func (s *s3ArtifactStorage) signingKey(date string) []byte {
	dateKey := hmacSHA256([]byte("AWS4"+s.secretKey), date)
	regionKey := hmacSHA256(dateKey, s.region)
	serviceKey := hmacSHA256(regionKey, "s3")
	return hmacSHA256(serviceKey, "aws4_request")
}

func cleanArtifactRelativePath(value string) (string, error) {
	clean := strings.Trim(path.Clean("/"+strings.TrimSpace(value)), "/")
	if clean == "" || clean == "." || strings.Contains(clean, "..") {
		return "", errors.New("invalid artifact path")
	}
	return clean, nil
}

func canonicalHeaders(headers map[string]string) (string, string) {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, strings.ToLower(key))
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+":"+strings.Join(strings.Fields(headers[key]), " "))
	}
	return strings.Join(lines, "\n") + "\n", strings.Join(keys, ";")
}

func canonicalQuery(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, awsQueryEscape(key)+"="+awsQueryEscape(values[key]))
	}
	return strings.Join(parts, "&")
}

func awsQueryEscape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(value))
	return h.Sum(nil)
}
