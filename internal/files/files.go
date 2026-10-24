// Package files opens the optional filesystem storage (a gocloud.dev/blob
// bucket) and holds the small helpers shared by the API (which serves and
// signs file URLs) and the agent (which signs download URLs for providers).
package files

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"strings"
	"time"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/memblob"
	_ "gocloud.dev/blob/s3blob"
)

// Scheme returns the driver part of a filesystem DSN (s3, file, mem).
func Scheme(dsn string) string {
	s, _, _ := strings.Cut(dsn, "://")
	return s
}

// Direct reports whether clients and providers talk to the storage
// directly with presigned URLs (S3). Other drivers go through hfy.
func Direct(scheme string) bool { return scheme == "s3" }

// Open opens the bucket named by the DSN (an s3://, file:// or mem:// URL).
func Open(ctx context.Context, dsn string) (*blob.Bucket, error) {
	switch Scheme(dsn) {
	case "s3", "file", "mem":
	default:
		return nil, fmt.Errorf("filesystem.dsn: unsupported scheme %q (want s3, file or mem)", Scheme(dsn))
	}
	return blob.OpenBucket(ctx, dsn)
}

// sign returns the HMAC that authorizes msg until exp. The signing key is
// derived from the server secret, so the credential itself is never used raw
// and no state is kept.
func sign(secret, msg string, exp time.Time) string {
	d := hmac.New(sha256.New, []byte(secret))
	d.Write([]byte("hfy/files/v1"))
	m := hmac.New(sha256.New, d.Sum(nil))
	fmt.Fprintf(m, "%s\n%d", msg, exp.Unix())
	return hex.EncodeToString(m.Sum(nil))
}

func verify(secret, msg, sig string, exp time.Time) bool {
	return time.Now().Before(exp) && hmac.Equal([]byte(sign(secret, msg, exp)), []byte(sig))
}

// SignGet authorizes downloading key until exp; SignPut authorizes uploading
// key with the given mime. The "get"/"put" prefixes keep one kind of
// signature from ever working as the other.
func SignGet(secret, key string, exp time.Time) string { return sign(secret, "get\n"+key, exp) }

func VerifyGet(secret, key, sig string, exp time.Time) bool {
	return verify(secret, "get\n"+key, sig, exp)
}

func SignPut(secret, key, mime string, exp time.Time) string {
	return sign(secret, "put\n"+key+"\n"+mime, exp)
}

func VerifyPut(secret, key, mime, sig string, exp time.Time) bool {
	return verify(secret, "put\n"+key+"\n"+mime, sig, exp)
}

// StorageOrigins returns the HTTPS origins a browser must be allowed to
// reach for direct (S3) uploads/downloads, for the embed page's CSP.
func StorageOrigins(dsn string) []string {
	if Scheme(dsn) != "s3" {
		return nil
	}
	_, rest, _ := strings.Cut(dsn, "://")
	bucket, rawQuery, _ := strings.Cut(rest, "?")
	region := "us-east-1"
	var endpoint string
	for kv := range strings.SplitSeq(rawQuery, "&") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "region":
			if v != "" {
				region = v
			}
		case "endpoint":
			endpoint = v
		}
	}
	origins := []string{
		"https://" + bucket + ".s3." + region + ".amazonaws.com",
		"https://s3." + region + ".amazonaws.com",
	}
	if endpoint != "" {
		scheme := "https"
		if strings.HasPrefix(endpoint, "http://") {
			scheme = "http"
		}
		host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
		host, _, _ = strings.Cut(host, "/")
		origins = append(origins, scheme+"://"+host)
	}
	return origins
}

// ExtForMIME returns a file extension (with dot) for a MIME type.
func ExtForMIME(m string) string {
	exts, _ := mime.ExtensionsByType(m)
	if len(exts) == 0 {
		return ""
	}
	return exts[0]
}
