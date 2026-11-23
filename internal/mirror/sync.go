// Package mirror builds a static mirror of the juliaup server layout.
//
// A juliaup client reads, relative to JULIAUP_SERVER:
//
//	juliaup/{RELEASE,RELEASEPREVIEW,DEV}CHANNELDBVERSION   versiondb version
//	juliaup/versiondb/versiondb-<dbversion>-<target>.json  channels -> versions -> UrlPath
//	<UrlPath>                                             Julia archives (bin/...)
//	juliaup/{RELEASE,RELEASEPREVIEW,DEV}CHANNELVERSION     juliaup version, for self update
//	juliaup/bin/juliaup-<version>-<target>.tar.gz          juliaup itself
//	juliaup/bin/juliainstaller-<version>-<target>          used by install.sh
//
// Sync writes the same tree into a directory, so any static web server can
// serve it.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// DefaultUpstream is the official juliaup server.
const DefaultUpstream = "https://julialang-s3.julialang.org"

// DefaultInstallScriptURL is the install script source; {VERSION} is the
// juliaup version.
const DefaultInstallScriptURL = "https://raw.githubusercontent.com/JuliaLang/juliaup/v{VERSION}/deploy/shellscript/juliaup-init.sh"

// DefaultTargets are the platforms most installs need. On Linux, juliaup's
// install script always installs the musl juliaup, so a Linux mirror needs
// the musl target; both libc targets list the same Julia archives.
var DefaultTargets = []string{
	"x86_64-unknown-linux-musl",
	"x86_64-unknown-linux-gnu",
	"aarch64-unknown-linux-musl",
	"aarch64-unknown-linux-gnu",
	"x86_64-apple-darwin",
	"aarch64-apple-darwin",
	"x86_64-pc-windows-msvc",
}

var dbVersionFiles = []string{"DBVERSION", "RELEASECHANNELDBVERSION", "RELEASEPREVIEWCHANNELDBVERSION", "DEVCHANNELDBVERSION"}

var juliaupVersionFiles = []string{"RELEASECHANNELVERSION", "RELEASEPREVIEWCHANNELVERSION", "DEVCHANNELVERSION"}

const stateFile = ".juliaup-mirror-state.json"

// Config controls a sync.
type Config struct {
	Upstream  string
	Out       string
	Targets   []string
	Selection Selection
	// JuliaupVersion is the juliaup release to mirror; empty means upstream's
	// current release.
	JuliaupVersion string
	// ServerURL is the mirror's public URL. When set, an install.sh pointing
	// at it is written.
	ServerURL        string
	InstallScriptURL string
	// MacOSDmg also mirrors the .dmg juliaup tries before the .tar.gz on macOS.
	MacOSDmg bool
	// MetadataOnly skips Julia archives, e.g. when a caching proxy serves bin/.
	MetadataOnly bool
	// Prune deletes archives and juliaup binaries the mirror no longer lists.
	Prune  bool
	DryRun bool
	Jobs   int
	Logf   func(format string, args ...any)
}

// Result summarises a sync.
type Result struct {
	UpstreamDBVersion string
	DBVersion         string
	JuliaupVersion    string
	Files             int
	Downloaded        int
	DownloadedBytes   int64
	// MissingBytes is the size still to download (dry run only); -1 if unknown.
	MissingBytes int64
	Pruned       int
}

type state struct {
	UpstreamDBVersion string `json:"upstream_db_version"`
	Revision          int    `json:"revision"`
	ContentHash       string `json:"content_hash"`
	DBVersion         string `json:"db_version"`
	PreviousDBVersion string `json:"previous_db_version,omitempty"`
}

type file struct {
	rel      string
	url      string
	optional bool
}

// Sync updates the mirror in cfg.Out.
func Sync(ctx context.Context, f *Fetcher, cfg Config) (*Result, error) {
	if cfg.Upstream == "" {
		cfg.Upstream = DefaultUpstream
	}
	cfg.Upstream = strings.TrimSuffix(cfg.Upstream, "/")
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Jobs < 1 {
		cfg.Jobs = 4
	}
	if len(cfg.Targets) == 0 {
		return nil, errors.New("no targets")
	}
	if len(cfg.Selection.Patterns) == 0 {
		return nil, errors.New("no channels selected")
	}
	up := func(rel string) string { return cfg.Upstream + "/" + rel }
	for _, t := range cfg.Targets {
		musl := strings.TrimSuffix(t, "-linux-gnu") + "-linux-musl"
		if strings.HasSuffix(t, "-linux-gnu") && !slices.Contains(cfg.Targets, musl) {
			cfg.Logf("warning: %s without %s: juliaup's install script installs the musl juliaup on Linux", t, musl)
		}
	}

	raw, err := f.GetString(ctx, up("juliaup/RELEASECHANNELDBVERSION"))
	if err != nil {
		return nil, fmt.Errorf("upstream db version: %w", err)
	}
	upstreamDB, err := ParseDBVersion(raw)
	if err != nil {
		return nil, err
	}
	juliaupVersion := cfg.JuliaupVersion
	if juliaupVersion == "" {
		if juliaupVersion, err = f.GetString(ctx, up("juliaup/RELEASECHANNELVERSION")); err != nil {
			return nil, fmt.Errorf("upstream juliaup version: %w", err)
		}
	}
	cfg.Logf("upstream versiondb %s, juliaup %s", upstreamDB, juliaupVersion)

	dbs := map[string]*VersionDB{}
	matched := map[string]bool{}
	for _, t := range cfg.Targets {
		data, err := f.Get(ctx, up(fmt.Sprintf("juliaup/versiondb/versiondb-%s-%s.json", upstreamDB, t)))
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("target %q: upstream has no versiondb for it", t)
		}
		if err != nil {
			return nil, err
		}
		full, err := ParseVersionDB(data)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", t, err)
		}
		kept, m, err := cfg.Selection.Filter(full)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", t, err)
		}
		for p := range m {
			matched[p] = true
		}
		if len(kept.Channels) == 0 {
			cfg.Logf("warning: target %s has none of the selected channels", t)
		}
		dbs[t] = kept
	}
	for _, p := range cfg.Selection.Patterns {
		if !matched[p] {
			return nil, fmt.Errorf("channel %q matches no channel on any target", p)
		}
	}

	files, err := plan(cfg, dbs, juliaupVersion, up)
	if err != nil {
		return nil, err
	}

	st, err := loadState(cfg.Out)
	if err != nil {
		return nil, err
	}
	hash, err := contentHash(dbs)
	if err != nil {
		return nil, err
	}
	next := nextState(st, upstreamDB, hash)
	mirrorDB, err := MirrorDBVersion(upstreamDB, next.Revision)
	if err != nil {
		return nil, err
	}
	next.DBVersion = mirrorDB.String()

	res := &Result{
		UpstreamDBVersion: upstreamDB.String(),
		DBVersion:         next.DBVersion,
		JuliaupVersion:    juliaupVersion,
		Files:             len(files),
	}

	var missing []file
	for _, fl := range files {
		if _, err := os.Stat(filepath.Join(cfg.Out, filepath.FromSlash(fl.rel))); errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, fl)
		}
	}

	if cfg.DryRun {
		res.MissingBytes = sizeOf(ctx, f, missing, cfg)
		for _, fl := range missing {
			cfg.Logf("would download %s", fl.rel)
		}
		res.Downloaded = len(missing)
		return res, nil
	}

	n, bytes, err := download(ctx, f, cfg, missing)
	res.Downloaded, res.DownloadedBytes = n, bytes
	if err != nil {
		return res, err
	}

	// Metadata goes last: clients only see the new versiondb once every file
	// it points to is in place, and the *DBVERSION pointers go after the
	// versiondb files they name.
	for t, db := range dbs {
		data, err := db.Encode(next.DBVersion)
		if err != nil {
			return res, err
		}
		if err := writeFile(cfg.Out, fmt.Sprintf("juliaup/versiondb/versiondb-%s-%s.json", next.DBVersion, t), data); err != nil {
			return res, err
		}
	}
	if cfg.ServerURL != "" {
		if err := writeInstallScript(ctx, f, cfg, juliaupVersion); err != nil {
			return res, err
		}
	}
	for _, name := range juliaupVersionFiles {
		if err := writeFile(cfg.Out, "juliaup/"+name, []byte(juliaupVersion+"\n")); err != nil {
			return res, err
		}
	}
	for _, name := range dbVersionFiles {
		if err := writeFile(cfg.Out, "juliaup/"+name, []byte(next.DBVersion+"\n")); err != nil {
			return res, err
		}
	}
	if err := saveState(cfg.Out, next); err != nil {
		return res, err
	}

	pruned, err := prune(cfg, files, next)
	res.Pruned = pruned
	return res, err
}

func plan(cfg Config, dbs map[string]*VersionDB, juliaupVersion string, up func(string) string) ([]file, error) {
	byRel := map[string]file{}
	add := func(rel string, optional bool) {
		if _, ok := byRel[rel]; !ok {
			byRel[rel] = file{rel: rel, url: up(rel), optional: optional}
		}
	}
	for _, t := range cfg.Targets {
		if !cfg.MetadataOnly {
			paths, err := dbs[t].URLPaths()
			if err != nil {
				return nil, fmt.Errorf("target %q: %w", t, err)
			}
			for _, p := range paths {
				if strings.Contains(p, "..") || strings.HasPrefix(p, "/") || strings.Contains(p, "://") {
					return nil, fmt.Errorf("target %q: refusing UrlPath %q", t, p)
				}
				add(p, false)
				if cfg.MacOSDmg && strings.HasSuffix(t, "-apple-darwin") && strings.HasSuffix(p, ".tar.gz") {
					add(strings.TrimSuffix(p, ".tar.gz")+".dmg", true)
				}
			}
		}
		if arch, ok := windowsMSIArch(t); ok {
			add(fmt.Sprintf("juliaup/winmsi/Julia-v%s-%s.msi", juliaupVersion, arch), true)
			continue
		}
		add(fmt.Sprintf("juliaup/bin/juliaup-%s-%s.tar.gz", juliaupVersion, t), false)
		add(fmt.Sprintf("juliaup/bin/juliainstaller-%s-%s", juliaupVersion, t), false)
	}
	files := make([]file, 0, len(byRel))
	for _, fl := range byRel {
		files = append(files, fl)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

// windowsMSIArch maps a Windows target to the arch in juliaup's MSI name.
// Windows has no juliaup tarball; installs use the MSI.
func windowsMSIArch(target string) (string, bool) {
	if !strings.Contains(target, "-pc-windows-") {
		return "", false
	}
	if strings.HasPrefix(target, "i686-") {
		return "x86", true
	}
	return "x64", true
}

func nextState(prev state, upstream DBVersion, hash string) state {
	next := state{UpstreamDBVersion: upstream.String(), ContentHash: hash, PreviousDBVersion: prev.DBVersion}
	switch {
	case prev.UpstreamDBVersion != upstream.String():
		next.Revision = 0
	case prev.ContentHash == hash:
		next.Revision = prev.Revision
		next.PreviousDBVersion = prev.PreviousDBVersion
	default:
		next.Revision = prev.Revision + 1
	}
	return next
}

func download(ctx context.Context, f *Fetcher, cfg Config, files []file) (int, int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		n        int
		total    int64
		firstErr error
		wg       sync.WaitGroup
	)
	work := make(chan file)
	for range cfg.Jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fl := range work {
				dest := filepath.Join(cfg.Out, filepath.FromSlash(fl.rel))
				err := f.Download(ctx, fl.url, dest)
				mu.Lock()
				switch {
				case err == nil:
					n++
					if fi, statErr := os.Stat(dest); statErr == nil {
						total += fi.Size()
					}
					cfg.Logf("downloaded %s", fl.rel)
				case fl.optional && errors.Is(err, ErrNotFound):
					cfg.Logf("skipped %s (not on upstream)", fl.rel)
				case firstErr == nil:
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	for _, fl := range files {
		select {
		case work <- fl:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	return n, total, firstErr
}

func sizeOf(ctx context.Context, f *Fetcher, files []file, cfg Config) int64 {
	var total int64
	for _, fl := range files {
		size, err := f.Size(ctx, fl.url)
		if err != nil {
			if fl.optional && errors.Is(err, ErrNotFound) {
				continue
			}
			cfg.Logf("warning: size of %s: %v", fl.rel, err)
			return -1
		}
		if size < 0 {
			return -1
		}
		total += size
	}
	return total
}

var serverLine = regexp.MustCompile(`(?m)^JULIAUP_SERVER="\$\{JULIAUP_SERVER:-[^}]*\}"$`)

func writeInstallScript(ctx context.Context, f *Fetcher, cfg Config, juliaupVersion string) error {
	src := cfg.InstallScriptURL
	if src == "" {
		src = DefaultInstallScriptURL
	}
	src = strings.ReplaceAll(src, "{VERSION}", juliaupVersion)
	body, err := f.Get(ctx, src)
	if err != nil {
		return fmt.Errorf("install script: %w", err)
	}
	script, err := RenderInstallScript(string(body), juliaupVersion, strings.TrimSuffix(cfg.ServerURL, "/"))
	if err != nil {
		return err
	}
	return writeFile(cfg.Out, "install.sh", []byte(script))
}

// RenderInstallScript turns juliaup's juliaup-init.sh template into an
// installer for the mirror at serverURL.
func RenderInstallScript(tmpl, juliaupVersion, serverURL string) (string, error) {
	s := strings.ReplaceAll(tmpl, "THISISREPLACEDWITHREALVERSIONINGITHUBWORKFLOW", juliaupVersion)
	s = strings.ReplaceAll(s, "THISISREPLACEDWITHCHANNELCONFIGINGITHUBWORKFLOW", "--juliaup-channel release")
	if strings.Contains(s, "THISISREPLACED") {
		return "", errors.New("install script has placeholders this tool does not know")
	}
	if !serverLine.MatchString(s) {
		return "", errors.New("install script: JULIAUP_SERVER default line not found; the upstream script changed")
	}
	// Exported, so juliainstaller (a child process) also uses the mirror.
	repl := fmt.Sprintf(`export JULIAUP_SERVER="${JULIAUP_SERVER:-%s}"
echo "Using JULIAUP_SERVER=$JULIAUP_SERVER" 1>&2
echo "To keep juliaup on this mirror, add 'export JULIAUP_SERVER=$JULIAUP_SERVER' to your shell profile." 1>&2`, serverURL)
	return serverLine.ReplaceAllLiteralString(s, repl), nil
}

func prune(cfg Config, files []file, st state) (int, error) {
	removed := 0
	keepDB := map[string]bool{st.DBVersion: true, st.PreviousDBVersion: true}
	vdbDir := filepath.Join(cfg.Out, "juliaup", "versiondb")
	entries, err := os.ReadDir(vdbDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	for _, e := range entries {
		v, ok := versiondbFileVersion(e.Name())
		if ok && !keepDB[v] {
			if err := os.Remove(filepath.Join(vdbDir, e.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	}
	if !cfg.Prune {
		return removed, nil
	}
	listed := map[string]bool{}
	for _, fl := range files {
		listed[fl.rel] = true
	}
	roots := []string{"juliaup/bin", "juliaup/winmsi"}
	if !cfg.MetadataOnly {
		roots = append(roots, "bin")
	}
	for _, root := range roots {
		err := filepath.WalkDir(filepath.Join(cfg.Out, root), func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(cfg.Out, p)
			if err != nil {
				return err
			}
			if listed[filepath.ToSlash(rel)] {
				return nil
			}
			cfg.Logf("pruned %s", filepath.ToSlash(rel))
			removed++
			return os.Remove(p)
		})
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

var versiondbName = regexp.MustCompile(`^versiondb-(\d+\.\d+\.\d+)-.+\.json$`)

func versiondbFileVersion(name string) (string, bool) {
	m := versiondbName.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func writeFile(root, rel string, data []byte) error {
	return writeAtomic(filepath.Join(root, filepath.FromSlash(rel)), func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

func loadState(root string) (state, error) {
	var st state
	data, err := os.ReadFile(filepath.Join(root, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("%s: %w", stateFile, err)
	}
	return st, nil
}

func saveState(root string, st state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(root, stateFile, append(data, '\n'))
}
