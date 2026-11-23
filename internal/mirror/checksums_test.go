package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// withVersionsJSON adds a bin/versions.json listing every bin/ file of up,
// with sums overriding the correct hash where given.
func withVersionsJSON(t *testing.T, up *fakeUpstream, sums map[string]string) *fakeUpstream {
	t.Helper()
	type file struct {
		URL    string `json:"url"`
		SHA256 string `json:"sha256"`
	}
	var files []file
	for rel, body := range up.files {
		if filepath.Dir(rel) == "." || rel[:4] != "bin/" {
			continue
		}
		sum, ok := sums[rel]
		if !ok {
			sum = sha(body)
		}
		files = append(files, file{URL: "https://julialang-s3.julialang.org/" + rel, SHA256: sum})
	}
	data, err := json.Marshal(map[string]any{"1.13.4": map[string]any{"files": files}})
	if err != nil {
		t.Fatal(err)
	}
	up.files[versionsJSON] = string(data)
	return up
}

func TestParseChecksums(t *testing.T) {
	sums, err := ParseChecksums([]byte(`{"1.10.14":{"files":[{"url":"https://julialang-s3.julialang.org/bin/linux/x64/1.10/a.tar.gz","sha256":"ABC"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if sums["bin/linux/x64/1.10/a.tar.gz"] != "abc" {
		t.Errorf("sums = %v", sums)
	}
}

func TestSyncVerifiesArchives(t *testing.T) {
	up := withVersionsJSON(t, newUpstream("1.0.104"), nil)
	srv := httptest.NewServer(up)
	defer srv.Close()
	out := t.TempDir()
	res, err := Sync(context.Background(), testFetcher(), testConfig(t, srv, out, "release", "lts"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Unverified != 0 {
		t.Errorf("unverified = %d", res.Unverified)
	}
	if !exists(out, versionsJSON) {
		t.Fatal("versions.json not mirrored")
	}
	v, err := Verify(out, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if v != (VerifyResult{OK: 2}) {
		t.Errorf("verify = %+v", v)
	}

	// A file changed on disk afterwards is reported.
	if err := os.WriteFile(filepath.Join(out, "bin/linux/x86_64/1.13/julia-1.13.4-linux-x86_64.tar.gz"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, _ := Verify(out, t.Logf); v.Bad != 1 {
		t.Errorf("verify after tampering = %+v", v)
	}

	// Pruning keeps versions.json.
	cfg := testConfig(t, srv, out, "release")
	cfg.Prune = true
	if _, err := Sync(context.Background(), testFetcher(), cfg); err != nil {
		t.Fatal(err)
	}
	if !exists(out, versionsJSON) {
		t.Error("versions.json pruned")
	}
}

func TestSyncRejectsBadChecksum(t *testing.T) {
	bad := "bin/linux/x86_64/1.10/julia-1.10.14-linux-x86_64.tar.gz"
	up := withVersionsJSON(t, newUpstream("1.0.104"), map[string]string{bad: sha("something else")})
	srv := httptest.NewServer(up)
	defer srv.Close()
	out := t.TempDir()
	_, err := Sync(context.Background(), testFetcher(), testConfig(t, srv, out, "lts"))
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	if exists(out, bad) || exists(out, "juliaup/RELEASECHANNELDBVERSION") {
		t.Error("bad archive or metadata published")
	}
}

func TestSyncWithoutVersionsJSONStillWorks(t *testing.T) {
	srv := httptest.NewServer(newUpstream("1.0.104"))
	defer srv.Close()
	res, err := Sync(context.Background(), testFetcher(), testConfig(t, srv, t.TempDir(), "release"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Unverified != 1 {
		t.Errorf("unverified = %d, want 1", res.Unverified)
	}
}

func TestDownloadRetriesChecksumMismatch(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte("corrupt"))
			return
		}
		_, _ = w.Write([]byte("good"))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "f")
	f := &Fetcher{Client: http.DefaultClient, Retries: 2, Backoff: time.Millisecond}
	if err := f.Download(context.Background(), srv.URL, dest, sha("good")); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}
