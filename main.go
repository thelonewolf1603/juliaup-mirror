// Command juliaup-mirror builds and serves a mirror of the juliaup server, so
// Julia can be installed with juliaup behind a firewall or without internet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/thelonewolf1603/juliaup-mirror/internal/mirror"
)

const usage = `juliaup-mirror builds a static mirror of the juliaup server.

Usage:
  juliaup-mirror sync  [flags]   download or update the mirror
  juliaup-mirror verify [flags]  check a mirror's Julia archives against their SHA256
  juliaup-mirror serve [flags]   serve a mirror directory over HTTP(S)

Clients use the mirror with:
  export JULIAUP_SERVER=https://mirror.example.com/juliaup-mirror

Run "juliaup-mirror <command> -h" for flags.
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "sync":
		err = runSync(ctx, os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("error: %v", err)
	}
}

func runSync(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("sync", flag.ExitOnError)
	out := fl.String("out", "", "mirror directory (required)")
	upstream := fl.String("upstream", mirror.DefaultUpstream, "juliaup server to mirror")
	channels := fl.String("channels", "release,lts", `comma-separated channel names or globs ("1.11", "1.10.*"); "*" mirrors every channel`)
	targets := fl.String("targets", strings.Join(mirror.DefaultTargets, ","), "comma-separated juliaup targets")
	archVariants := fl.Bool("arch-variants", false, `also mirror the "~x86"/"~x64" variants of the selected channels`)
	juliaupVersion := fl.String("juliaup-version", "", "juliaup release to mirror (default: upstream's current release)")
	serverURL := fl.String("server-url", "", "public URL of the mirror; when set, writes an install.sh that uses it")
	installScript := fl.String("install-script-url", mirror.DefaultInstallScriptURL, "source of juliaup's install script ({VERSION} is replaced)")
	dmg := fl.Bool("macos-dmg", false, "also mirror the macOS .dmg archives juliaup tries first")
	metadataOnly := fl.Bool("metadata-only", false, "skip Julia archives (when a caching proxy serves bin/)")
	skipChecksums := fl.Bool("skip-checksums", false, "do not check Julia archives against the upstream's bin/versions.json")
	prune := fl.Bool("prune", false, "delete archives and juliaup binaries the mirror no longer lists")
	dryRun := fl.Bool("dry-run", false, "show what would be downloaded and how big it is")
	jobs := fl.Int("jobs", 4, "parallel downloads")
	quiet := fl.Bool("quiet", false, "only print the summary")
	_ = fl.Parse(args)

	if *out == "" {
		return errors.New("-out is required")
	}
	if *serverURL != "" && !strings.HasPrefix(*serverURL, "https://") {
		log.Printf("warning: juliaup only accepts http:// for localhost; clients need https://")
	}
	logf := log.Printf
	if *quiet {
		logf = func(string, ...any) {}
	}
	cfg := mirror.Config{
		Upstream:         *upstream,
		Out:              *out,
		Targets:          splitList(*targets),
		Selection:        mirror.Selection{Patterns: splitList(*channels), ArchVariants: *archVariants},
		JuliaupVersion:   *juliaupVersion,
		ServerURL:        *serverURL,
		InstallScriptURL: *installScript,
		MacOSDmg:         *dmg,
		MetadataOnly:     *metadataOnly,
		SkipChecksums:    *skipChecksums,
		Prune:            *prune,
		DryRun:           *dryRun,
		Jobs:             *jobs,
		Logf:             logf,
	}
	res, err := mirror.Sync(ctx, mirror.NewFetcher(), cfg)
	if res != nil {
		printSummary(res, *dryRun)
	}
	return err
}

func printSummary(r *mirror.Result, dryRun bool) {
	fmt.Printf("upstream versiondb %s -> mirror versiondb %s, juliaup %s\n", r.UpstreamDBVersion, r.DBVersion, r.JuliaupVersion)
	if dryRun {
		size := "unknown size"
		if r.MissingBytes >= 0 {
			size = humanBytes(r.MissingBytes)
		}
		fmt.Printf("%d files listed, %d to download (%s)\n", r.Files, r.Downloaded, size)
		return
	}
	fmt.Printf("%d files listed, %d downloaded (%s), %d pruned\n", r.Files, r.Downloaded, humanBytes(r.DownloadedBytes), r.Pruned)
	if r.Unverified > 0 {
		fmt.Printf("%d archives have no published SHA256 and were not verified\n", r.Unverified)
	}
}

func runVerify(args []string) error {
	fl := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fl.String("dir", "", "mirror directory (required)")
	_ = fl.Parse(args)
	if *dir == "" {
		return errors.New("-dir is required")
	}
	res, err := mirror.Verify(*dir, log.Printf)
	if err != nil {
		return err
	}
	fmt.Printf("%d archives OK, %d mismatched, %d without a published hash\n", res.OK, res.Bad, res.Unknown)
	if res.Bad > 0 {
		return fmt.Errorf("%d archives do not match their SHA256", res.Bad)
	}
	return nil
}

func runServe(args []string) error {
	fl := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fl.String("dir", "", "mirror directory (required)")
	addr := fl.String("addr", "127.0.0.1:8080", "listen address")
	cert := fl.String("tls-cert", "", "TLS certificate file (PEM); serves https")
	key := fl.String("tls-key", "", "TLS key file (PEM)")
	_ = fl.Parse(args)
	if *dir == "" {
		return errors.New("-dir is required")
	}
	if (*cert == "") != (*key == "") {
		return errors.New("-tls-cert and -tls-key go together")
	}
	h := logRequests(http.FileServer(http.Dir(*dir)))
	if *cert != "" {
		log.Printf("serving %s on https://%s", *dir, *addr)
		return http.ListenAndServeTLS(*addr, *cert, *key, h)
	}
	log.Printf("serving %s on http://%s (juliaup accepts plain http only for localhost, and install.sh needs https)", *dir, *addr)
	return http.ListenAndServe(*addr, h)
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		h.ServeHTTP(w, r)
	})
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
