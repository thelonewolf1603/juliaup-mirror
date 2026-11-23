package mirror

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetcherRetries(t *testing.T) {
	tests := []struct {
		name      string
		statuses  []int
		wantCalls int64
		wantErr   bool
		notFound  bool
	}{
		{"ok", []int{200}, 1, false, false},
		{"404 is final", []int{404}, 1, true, true},
		{"403 is final", []int{403}, 1, true, false},
		{"503 then ok", []int{503, 503, 200}, 3, false, false},
		{"429 then ok", []int{429, 200}, 2, false, false},
		{"5xx gives up", []int{500, 500, 500, 500}, 3, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				w.WriteHeader(tc.statuses[min(int(n)-1, len(tc.statuses)-1)])
				_, _ = w.Write([]byte("body"))
			}))
			defer srv.Close()
			f := &Fetcher{Client: http.DefaultClient, Retries: 2, Backoff: time.Millisecond}
			_, err := f.Get(context.Background(), srv.URL)
			if (err != nil) != tc.wantErr || errors.Is(err, ErrNotFound) != tc.notFound {
				t.Errorf("err = %v", err)
			}
			if calls.Load() != tc.wantCalls {
				t.Errorf("calls = %d, want %d", calls.Load(), tc.wantCalls)
			}
		})
	}
}

func TestDownloadLeavesNothingOnTruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "a", "file")
	f := &Fetcher{Client: http.DefaultClient, Retries: 0, Backoff: time.Millisecond}
	if err := f.Download(context.Background(), srv.URL, dest, ""); err == nil {
		t.Fatal("want error")
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 0 {
		t.Errorf("left %d files behind", len(entries))
	}
}
