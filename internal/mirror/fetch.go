package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotFound is returned when the upstream answers 404.
var ErrNotFound = errors.New("not found")

// Fetcher downloads from an upstream with retries.
type Fetcher struct {
	Client  *http.Client
	Retries int
	// Backoff is the wait before the first retry; it doubles each time.
	Backoff time.Duration
}

// NewFetcher returns a Fetcher with sensible defaults.
func NewFetcher() *Fetcher {
	return &Fetcher{Client: &http.Client{Timeout: 30 * time.Minute}, Retries: 4, Backoff: 2 * time.Second}
}

// Get returns the body of url.
func (f *Fetcher) Get(ctx context.Context, url string) ([]byte, error) {
	var body []byte
	err := f.do(ctx, http.MethodGet, url, func(r io.Reader) error {
		var err error
		body, err = io.ReadAll(r)
		return err
	})
	return body, err
}

// GetString returns the trimmed body of url.
func (f *Fetcher) GetString(ctx context.Context, url string) (string, error) {
	b, err := f.Get(ctx, url)
	return strings.TrimSpace(string(b)), err
}

// Size returns the Content-Length of url, or -1 if the server does not say.
func (f *Fetcher) Size(ctx context.Context, url string) (int64, error) {
	var size int64 = -1
	err := f.retry(ctx, func() error {
		resp, err := f.send(ctx, http.MethodHead, url)
		if err != nil {
			return err
		}
		resp.Body.Close()
		size = resp.ContentLength
		return nil
	})
	return size, err
}

// ErrChecksum is returned when a download does not match its expected hash.
var ErrChecksum = errors.New("checksum mismatch")

// Download writes url to dest through a temporary file, so dest is either
// absent or complete. If sha256Hex is not empty, dest is only written when
// the body hashes to it; a mismatch is retried like a network error.
func (f *Fetcher) Download(ctx context.Context, url, dest, sha256Hex string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return f.do(ctx, http.MethodGet, url, func(r io.Reader) error {
		return writeAtomic(dest, func(w io.Writer) error {
			h := sha256.New()
			if _, err := io.Copy(io.MultiWriter(w, h), r); err != nil {
				return err
			}
			if got := hex.EncodeToString(h.Sum(nil)); sha256Hex != "" && !strings.EqualFold(got, sha256Hex) {
				return fmt.Errorf("%s: %w: got sha256 %s, want %s", url, ErrChecksum, got, sha256Hex)
			}
			return nil
		})
	})
}

func (f *Fetcher) do(ctx context.Context, method, url string, consume func(io.Reader) error) error {
	return f.retry(ctx, func() error {
		resp, err := f.send(ctx, method, url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if err := consume(resp.Body); err != nil {
			return fmt.Errorf("read %s: %w", url, err)
		}
		return nil
	})
}

func (f *Fetcher) send(ctx context.Context, method, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, permanent{err}
	}
	req.Header.Set("User-Agent", "juliaup-mirror")
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp, nil
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, permanent{fmt.Errorf("%s %s: %w", method, url, ErrNotFound)}
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: HTTP %d", method, url, resp.StatusCode)
	default:
		resp.Body.Close()
		return nil, permanent{fmt.Errorf("%s %s: HTTP %d", method, url, resp.StatusCode)}
	}
}

type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

func (f *Fetcher) retry(ctx context.Context, op func() error) error {
	wait := f.Backoff
	for attempt := 0; ; attempt++ {
		err := op()
		if perm := (permanent{}); errors.As(err, &perm) {
			return perm.error
		}
		if err == nil || attempt >= f.Retries || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// writeAtomic writes dest via a temporary file in the same directory.
func writeAtomic(dest string, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".tmp-"+filepath.Base(dest)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}
