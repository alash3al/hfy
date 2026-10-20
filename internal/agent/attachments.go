package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/svc"
)

// attachmentFetcher downloads attachments for providers that cannot fetch
// URLs themselves. Only allowed hosts are ever contacted (SSRF), redirects to
// other hosts are refused, and the size cap is enforced while downloading.
type attachmentFetcher struct {
	cfg    *config.Config
	client *http.Client
}

func newAttachmentFetcher(cfg *config.Config) *attachmentFetcher {
	f := &attachmentFetcher{cfg: cfg}
	f.client = &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("too many redirects")
			}
			if !f.allowed(req.URL) {
				return fmt.Errorf("redirect to %q is not allowed", req.URL.Hostname())
			}
			return nil
		},
	}
	return f
}

func (f *attachmentFetcher) allowed(u *url.URL) bool {
	a := f.cfg.Attachments
	return a != nil && (u.Scheme == "http" || u.Scheme == "https") && svc.HostAllowed(a.AllowedHosts, u.Hostname())
}

// dataURI downloads the attachment and returns it as a base64 data URI.
func (f *attachmentFetcher) dataURI(ctx context.Context, raw, mime string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || !f.allowed(u) {
		return "", errors.New("attachment host is not allowed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("attachment returned %d", resp.StatusCode)
	}
	limit := int64(f.cfg.Attachments.MaxBytes)
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("attachment is larger than %d bytes", limit)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}
