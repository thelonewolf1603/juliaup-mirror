package mirror

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const target = "x86_64-unknown-linux-gnu"

const initScript = `#!/bin/sh
# If JULIAUP_SERVER is unset or empty, default it.
JULIAUP_SERVER="${JULIAUP_SERVER:-https://julialang-s3.julialang.org}"
JULIAUP_VERSION="THISISREPLACEDWITHREALVERSIONINGITHUBWORKFLOW"
run THISISREPLACEDWITHCHANNELCONFIGINGITHUBWORKFLOW "$@"
`

type fakeUpstream struct {
	files map[string]string
	hits  atomic.Int64
}

func (u *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.hits.Add(1)
	body, ok := u.files[strings.TrimPrefix(r.URL.Path, "/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write([]byte(body))
}

func newUpstream(dbVersion string) *fakeUpstream {
	return &fakeUpstream{files: map[string]string{
		"juliaup/RELEASECHANNELDBVERSION":                                   dbVersion + "\n",
		"juliaup/RELEASECHANNELVERSION":                                     "1.24.0\n",
		"juliaup/versiondb/versiondb-" + dbVersion + "-" + target + ".json": sampleDB,
		"bin/linux/x86_64/1.13/julia-1.13.4-linux-x86_64.tar.gz":            "julia 1.13.4",
		"bin/linux/x86_64/1.10/julia-1.10.14-linux-x86_64.tar.gz":           "julia 1.10.14",
		"bin/linux/x86_64/1.13/julia-1.13.3-linux-x86_64.tar.gz":            "julia 1.13.3",
		"juliaup/bin/juliaup-1.24.0-" + target + ".tar.gz":                  "juliaup",
		"juliaup/bin/juliainstaller-1.24.0-" + target:                       "installer",
		"install/v1.24.0/juliaup-init.sh":                                   initScript,
	}}
}

func testConfig(t *testing.T, srv *httptest.Server, out string, channels ...string) Config {
	t.Helper()
	return Config{
		Upstream:         srv.URL,
		Out:              out,
		Targets:          []string{target},
		Selection:        Selection{Patterns: channels},
		ServerURL:        "https://mirror.example.com/jl",
		InstallScriptURL: srv.URL + "/install/v{VERSION}/juliaup-init.sh",
		Logf:             t.Logf,
	}
}

func testFetcher() *Fetcher {
	return &Fetcher{Client: http.DefaultClient, Retries: 0, Backoff: time.Millisecond}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

func TestSyncLifecycle(t *testing.T) {
	up := newUpstream("1.0.104")
	srv := httptest.NewServer(up)
	defer srv.Close()
	out := t.TempDir()
	ctx := context.Background()

	res, err := Sync(ctx, testFetcher(), testConfig(t, srv, out, "release", "lts"))
	if err != nil {
		t.Fatal(err)
	}
	if res.DBVersion != "1.0.104000" || res.Downloaded != 4 {
		t.Fatalf("first sync: %+v", res)
	}
	for _, f := range []string{"DBVERSION", "RELEASECHANNELDBVERSION", "DEVCHANNELDBVERSION"} {
		if got := read(t, out, "juliaup/"+f); got != "1.0.104000\n" {
			t.Errorf("%s = %q", f, got)
		}
	}
	if got := read(t, out, "juliaup/RELEASECHANNELVERSION"); got != "1.24.0\n" {
		t.Errorf("RELEASECHANNELVERSION = %q", got)
	}
	db, err := ParseVersionDB([]byte(read(t, out, "juliaup/versiondb/versiondb-1.0.104000-"+target+".json")))
	if err != nil {
		t.Fatal(err)
	}
	if names := channelNames(db); !equal(names, []string{"lts", "release", "release~x64"}) {
		t.Errorf("channels = %v", names)
	}
	if exists(out, "bin/linux/x86_64/1.13/julia-1.13.3-linux-x86_64.tar.gz") {
		t.Error("unselected version was downloaded")
	}
	script := read(t, out, "install.sh")
	if !strings.Contains(script, `export JULIAUP_SERVER="${JULIAUP_SERVER:-https://mirror.example.com/jl}"`) ||
		!strings.Contains(script, `JULIAUP_VERSION="1.24.0"`) || strings.Contains(script, "THISISREPLACED") {
		t.Errorf("install.sh not rendered:\n%s", script)
	}

	// Unchanged upstream and selection: nothing downloaded, same version.
	res, err = Sync(ctx, testFetcher(), testConfig(t, srv, out, "release", "lts"))
	if err != nil {
		t.Fatal(err)
	}
	if res.DBVersion != "1.0.104000" || res.Downloaded != 0 {
		t.Fatalf("no-op sync: %+v", res)
	}

	// Selection changed: new revision so clients refetch; old archive pruned.
	cfg := testConfig(t, srv, out, "release")
	cfg.Prune = true
	res, err = Sync(ctx, testFetcher(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.DBVersion != "1.0.104001" {
		t.Fatalf("selection change: %+v", res)
	}
	if exists(out, "bin/linux/x86_64/1.10/julia-1.10.14-linux-x86_64.tar.gz") {
		t.Error("lts archive not pruned")
	}
	if !exists(out, "juliaup/versiondb/versiondb-1.0.104000-"+target+".json") {
		t.Error("previous versiondb removed too early")
	}

	// Upstream moved on: revision resets, versiondb two versions back goes.
	up2 := newUpstream("1.0.105")
	srv.Config.Handler = up2
	res, err = Sync(ctx, testFetcher(), testConfig(t, srv, out, "release"))
	if err != nil {
		t.Fatal(err)
	}
	if res.DBVersion != "1.0.105000" {
		t.Fatalf("upstream bump: %+v", res)
	}
	if exists(out, "juliaup/versiondb/versiondb-1.0.104000-"+target+".json") {
		t.Error("stale versiondb kept")
	}
	if !exists(out, "juliaup/versiondb/versiondb-1.0.104001-"+target+".json") {
		t.Error("previous versiondb removed")
	}
}

func TestSyncRejectsUnknownChannel(t *testing.T) {
	srv := httptest.NewServer(newUpstream("1.0.104"))
	defer srv.Close()
	_, err := Sync(context.Background(), testFetcher(), testConfig(t, srv, t.TempDir(), "relaese"))
	if err == nil || !strings.Contains(err.Error(), "relaese") {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncDoesNotPublishOnFailedDownload(t *testing.T) {
	up := newUpstream("1.0.104")
	delete(up.files, "bin/linux/x86_64/1.10/julia-1.10.14-linux-x86_64.tar.gz")
	srv := httptest.NewServer(up)
	defer srv.Close()
	out := t.TempDir()
	if _, err := Sync(context.Background(), testFetcher(), testConfig(t, srv, out, "lts")); err == nil {
		t.Fatal("want error")
	}
	if exists(out, "juliaup/RELEASECHANNELDBVERSION") {
		t.Error("metadata published although an archive is missing")
	}
}

func TestSyncDryRun(t *testing.T) {
	srv := httptest.NewServer(newUpstream("1.0.104"))
	defer srv.Close()
	out := t.TempDir()
	cfg := testConfig(t, srv, out, "release")
	cfg.DryRun = true
	res, err := Sync(context.Background(), testFetcher(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Downloaded != 3 || res.MissingBytes != int64(len("julia 1.13.4")+len("juliaup")+len("installer")) {
		t.Fatalf("dry run: %+v", res)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Errorf("dry run wrote %d entries", len(entries))
	}
}
