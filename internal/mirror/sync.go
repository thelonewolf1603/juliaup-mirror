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

// DefaultTargets are the platforms most installs need.
//
// On Linux, juliaup's install script always installs the musl juliaup; both
// libc targets list the same Julia archives. On Windows, the MSI installs the
// -gnu juliaup and the Store/App Installer builds are -msvc.
var DefaultTargets = []string{
	"x86_64-unknown-linux-musl",
	"x86_64-unknown-linux-gnu",
	"aarch64-unknown-linux-musl",
	"aarch64-unknown-linux-gnu",
	"x86_64-apple-darwin",
	"aarch64-apple-darwin",
	"x86_64-pc-windows-gnu",
	"x86_64-pc-windows-msvc",
}

var dbVersionFiles = []string{"DBVERSION", "RELEASECHANNELDBVERSION", "RELEASEPREVIEWCHANNELDBVERSION", "DEVCHANNELDBVERSION"}

var juliaupVersionFiles = []string{"RELEASECHANNELVERSION", "RELEASEPREVIEWCHANNELVERSION", "DEVCHANNELVERSION"}

const (
	stateFile = ".juliaup-mirror-state.json"
	lockFile  = ".juliaup-mirror.lock"
	tmpPrefix = ".tmp-"
)

var juliaupVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Config controls a sync.
type Config struct {
	Upstream  string
	Out       string
	Targets   []string
	Selection Selection
	// JuliaupVersion is the juliaup release to mirror; empty means upstream's
	// current release.
	JuliaupVersion string
	// ServerURL is the mirror's public URL, used for install.sh. Empty means
	// the URL of the previous sync, if any.
	ServerURL        string
	InstallScriptURL string
	// MacOSDmg also mirrors the .dmg juliaup tries before the .tar.gz on macOS.
	MacOSDmg bool
	// MetadataOnly skips Julia archives, e.g. when a caching proxy serves bin/.
	MetadataOnly bool
	// Prune deletes archives and juliaup binaries that neither the current
	// nor the previous versiondb lists.
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
	UpstreamDBVersion      string `json:"upstream_db_version"`
	ContentHash            string `json:"content_hash"`
	DBVersion              string `json:"db_version"`
	PreviousDBVersion      string `json:"previous_db_version,omitempty"`
	JuliaupVersion         string `json:"juliaup_version,omitempty"`
	PreviousJuliaupVersion string `json:"previous_juliaup_version,omitempty"`
	ServerURL              string `json:"server_url,omitempty"`
}

type file struct {
	rel      string
	url      string
	optional bool
}

type upstreamData struct {
	dbVersion      DBVersion
	juliaupVersion string
	dbs            map[string]*VersionDB
}

// Sync updates the mirror in cfg.Out.
func Sync(ctx context.Context, f *Fetcher, cfg Config) (*Result, error) {
	if err := cfg.setDefaults(); err != nil {
		return nil, err
	}
	if !cfg.DryRun {
		if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
			return nil, err
		}
		unlock, err := lock(filepath.Join(cfg.Out, lockFile))
		if err != nil {
			return nil, err
		}
		defer unlock()
		if err := removeTempFiles(cfg.Out); err != nil {
			return nil, err
		}
	}

	upd, err := fetchUpstream(ctx, f, cfg)
	if err != nil {
		return nil, err
	}
	files, err := plan(cfg, upd.dbs, upd.juliaupVersion)
	if err != nil {
		return nil, err
	}

	prev, err := loadState(cfg.Out)
	if err != nil {
		return nil, err
	}
	hash, err := contentHash(upd.dbs)
	if err != nil {
		return nil, err
	}
	published, err := publishedVersion(cfg.Out)
	if err != nil {
		return nil, err
	}
	next, err := nextState(prev, published, upd, hash)
	if err != nil {
		return nil, err
	}
	if cfg.ServerURL == "" {
		cfg.ServerURL = prev.ServerURL
	}
	next.ServerURL = cfg.ServerURL

	res := &Result{
		UpstreamDBVersion: upd.dbVersion.String(),
		DBVersion:         next.DBVersion,
		JuliaupVersion:    upd.juliaupVersion,
		Files:             len(files),
	}
	missing := missingFiles(cfg.Out, files)

	if cfg.DryRun {
		res.MissingBytes = sizeOf(ctx, f, missing, cfg)
		for _, fl := range missing {
			cfg.Logf("would download %s", fl.rel)
		}
		res.Downloaded = len(missing)
		return res, nil
	}

	res.Downloaded, res.DownloadedBytes, err = download(ctx, f, cfg, missing)
	if err != nil {
		return res, err
	}
	if err := publish(ctx, f, cfg, upd, next); err != nil {
		return res, err
	}
	res.Pruned, err = prune(cfg, next)
	return res, err
}

func (cfg *Config) setDefaults() error {
	if cfg.Upstream == "" {
		cfg.Upstream = DefaultUpstream
	}
	cfg.Upstream = strings.TrimSuffix(cfg.Upstream, "/")
	cfg.ServerURL = strings.TrimSuffix(cfg.ServerURL, "/")
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Jobs < 1 {
		cfg.Jobs = 4
	}
	if cfg.Out == "" {
		return errors.New("no output directory")
	}
	if len(cfg.Targets) == 0 {
		return errors.New("no targets")
	}
	if len(cfg.Selection.Patterns) == 0 {
		return errors.New("no channels selected")
	}
	for _, t := range cfg.Targets {
		musl := strings.TrimSuffix(t, "-linux-gnu") + "-linux-musl"
		if strings.HasSuffix(t, "-linux-gnu") && !slices.Contains(cfg.Targets, musl) {
			cfg.Logf("warning: %s without %s: juliaup's install script installs the musl juliaup on Linux", t, musl)
		}
	}
	return nil
}

func (cfg *Config) upstreamURL(rel string) string { return cfg.Upstream + "/" + rel }

func fetchUpstream(ctx context.Context, f *Fetcher, cfg Config) (*upstreamData, error) {
	raw, err := f.GetString(ctx, cfg.upstreamURL("juliaup/RELEASECHANNELDBVERSION"))
	if err != nil {
		return nil, fmt.Errorf("upstream db version: %w", err)
	}
	upd := &upstreamData{dbs: map[string]*VersionDB{}, juliaupVersion: cfg.JuliaupVersion}
	if upd.dbVersion, err = ParseDBVersion(raw); err != nil {
		return nil, fmt.Errorf("upstream db version: %w", err)
	}
	if upd.juliaupVersion == "" {
		if upd.juliaupVersion, err = f.GetString(ctx, cfg.upstreamURL("juliaup/RELEASECHANNELVERSION")); err != nil {
			return nil, fmt.Errorf("upstream juliaup version: %w", err)
		}
	}
	if !juliaupVersionRE.MatchString(upd.juliaupVersion) {
		return nil, fmt.Errorf("juliaup version %.40q is not major.minor.patch", upd.juliaupVersion)
	}
	cfg.Logf("upstream versiondb %s, juliaup %s", upd.dbVersion, upd.juliaupVersion)

	matched := map[string]bool{}
	for _, t := range cfg.Targets {
		data, err := f.Get(ctx, cfg.upstreamURL(fmt.Sprintf("juliaup/versiondb/versiondb-%s-%s.json", upd.dbVersion, t)))
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
		upd.dbs[t] = kept
	}
	for _, p := range cfg.Selection.Patterns {
		if !matched[p] {
			return nil, fmt.Errorf("channel %q matches no channel on any target", p)
		}
	}
	return upd, nil
}

func plan(cfg Config, dbs map[string]*VersionDB, juliaupVersion string) ([]file, error) {
	byRel := map[string]file{}
	add := func(rel string, optional bool) {
		if _, ok := byRel[rel]; !ok {
			byRel[rel] = file{rel: rel, url: cfg.upstreamURL(rel), optional: optional}
		}
	}
	for _, t := range cfg.Targets {
		if !cfg.MetadataOnly {
			paths, err := archivePaths(dbs[t], t, cfg.MacOSDmg)
			if err != nil {
				return nil, err
			}
			for _, p := range paths {
				add(p, strings.HasSuffix(p, ".dmg"))
			}
		}
		for _, p := range juliaupPaths(juliaupVersion, t) {
			_, isWindows := windowsMSIArch(t)
			add(p, isWindows)
		}
	}
	files := make([]file, 0, len(byRel))
	for _, fl := range byRel {
		files = append(files, fl)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

// archivePaths lists the Julia archives db points to, checked to stay inside
// the mirror.
func archivePaths(db *VersionDB, target string, dmg bool) ([]string, error) {
	paths, err := db.URLPaths()
	if err != nil {
		return nil, fmt.Errorf("target %q: %w", target, err)
	}
	var out []string
	for _, p := range paths {
		if !filepath.IsLocal(p) || strings.Contains(p, "..") || strings.Contains(p, "://") || strings.Contains(p, `\`) {
			return nil, fmt.Errorf("target %q: refusing UrlPath %q", target, p)
		}
		out = append(out, p)
		if dmg && strings.HasSuffix(target, "-apple-darwin") && strings.HasSuffix(p, ".tar.gz") {
			out = append(out, strings.TrimSuffix(p, ".tar.gz")+".dmg")
		}
	}
	return out, nil
}

// juliaupPaths lists the juliaup files a target needs. Windows has no juliaup
// tarball; installs use the MSI.
func juliaupPaths(version, target string) []string {
	if arch, ok := windowsMSIArch(target); ok {
		return []string{fmt.Sprintf("juliaup/winmsi/Julia-v%s-%s.msi", version, arch)}
	}
	return []string{
		fmt.Sprintf("juliaup/bin/juliaup-%s-%s.tar.gz", version, target),
		fmt.Sprintf("juliaup/bin/juliainstaller-%s-%s", version, target),
	}
}

func windowsMSIArch(target string) (string, bool) {
	if !strings.Contains(target, "-pc-windows-") {
		return "", false
	}
	if strings.HasPrefix(target, "i686-") {
		return "x86", true
	}
	return "x64", true
}

// publishedVersion is the highest db version the mirror has ever exposed: the
// pointer file or any versiondb file, including ones a killed sync left
// behind. Zero if there is none.
func publishedVersion(root string) (DBVersion, error) {
	var max DBVersion
	if raw, err := os.ReadFile(filepath.Join(root, "juliaup", "RELEASECHANNELDBVERSION")); err == nil {
		if v, err := ParseDBVersion(string(raw)); err == nil {
			max = v
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return max, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "juliaup", "versiondb"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return max, err
	}
	for _, e := range entries {
		if s, ok := versiondbFileVersion(e.Name()); ok {
			if v, err := ParseDBVersion(s); err == nil && max.Less(v) {
				max = v
			}
		}
	}
	return max, nil
}

// nextState picks the db version to publish.
//
// The same version must never be published with different content: juliaup
// only downloads a versiondb whose version is strictly higher than its cached
// one, so a client that saw the first content would keep it. The next version
// is therefore derived from what is on disk, not only from the state file,
// which a killed sync may not have updated.
func nextState(prev state, published DBVersion, upd *upstreamData, hash string) (state, error) {
	next := state{
		UpstreamDBVersion:      upd.dbVersion.String(),
		ContentHash:            hash,
		PreviousDBVersion:      prev.DBVersion,
		JuliaupVersion:         upd.juliaupVersion,
		PreviousJuliaupVersion: prev.JuliaupVersion,
	}
	if prev.JuliaupVersion == upd.juliaupVersion {
		next.PreviousJuliaupVersion = prev.PreviousJuliaupVersion
	}
	if prev.DBVersion == published.String() && prev.ContentHash == hash && prev.UpstreamDBVersion == next.UpstreamDBVersion {
		next.DBVersion = prev.DBVersion
		next.PreviousDBVersion = prev.PreviousDBVersion
		return next, nil
	}
	base, err := MirrorDBVersion(upd.dbVersion, 0)
	if err != nil {
		return next, err
	}
	v := base
	if !published.Less(base) {
		v = published
		v.Patch++
		if published.Major != base.Major || published.Minor != base.Minor || published.Patch/maxRevisions != upd.dbVersion.Patch {
			return next, fmt.Errorf("mirror already published db version %s, which is newer than upstream %s allows; did the upstream go backwards?", published, upd.dbVersion)
		}
		if v.Patch-base.Patch >= maxRevisions {
			return next, fmt.Errorf("more than %d mirror revisions for upstream %s", maxRevisions, upd.dbVersion)
		}
	}
	next.DBVersion = v.String()
	return next, nil
}

func missingFiles(root string, files []file) []file {
	var missing []file
	for _, fl := range files {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(fl.rel))); errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, fl)
		}
	}
	return missing
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
				if ctx.Err() != nil {
					continue
				}
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
		if ctx.Err() != nil {
			break
		}
		select {
		case work <- fl:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}
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

// publish writes the metadata. It runs after every archive is in place, and
// the *DBVERSION pointers go last, after the versiondb files they name, so a
// client never sees a versiondb whose files are missing.
func publish(ctx context.Context, f *Fetcher, cfg Config, upd *upstreamData, next state) error {
	for t, db := range upd.dbs {
		data, err := db.Encode(next.DBVersion)
		if err != nil {
			return err
		}
		if err := writeFile(cfg.Out, fmt.Sprintf("juliaup/versiondb/versiondb-%s-%s.json", next.DBVersion, t), data); err != nil {
			return err
		}
	}
	if cfg.ServerURL != "" {
		if err := writeInstallScript(ctx, f, cfg, upd.juliaupVersion); err != nil {
			return err
		}
	} else {
		cfg.Logf("no -server-url given yet: install.sh not written")
	}
	for _, name := range juliaupVersionFiles {
		if err := writeFile(cfg.Out, "juliaup/"+name, []byte(upd.juliaupVersion+"\n")); err != nil {
			return err
		}
	}
	for _, name := range dbVersionFiles {
		if err := writeFile(cfg.Out, "juliaup/"+name, []byte(next.DBVersion+"\n")); err != nil {
			return err
		}
	}
	return saveState(cfg.Out, next)
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
	script, err := RenderInstallScript(string(body), juliaupVersion, cfg.ServerURL)
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

// prune removes versiondb files other than the current and previous ones and,
// with cfg.Prune, archives and juliaup files that neither of those lists.
// Clients that fetched the previous versiondb a moment ago can still install
// from it.
func prune(cfg Config, st state) (int, error) {
	removed := 0
	keepDB := map[string]bool{st.DBVersion: true, st.PreviousDBVersion: true}
	vdbDir := filepath.Join(cfg.Out, "juliaup", "versiondb")
	entries, err := os.ReadDir(vdbDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	keep := map[string]bool{}
	for _, e := range entries {
		v, ok := versiondbFileVersion(e.Name())
		if !ok {
			continue
		}
		p := filepath.Join(vdbDir, e.Name())
		if !keepDB[v] {
			if err := os.Remove(p); err != nil {
				return removed, err
			}
			removed++
			continue
		}
		if !cfg.Prune {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return removed, err
		}
		db, err := ParseVersionDB(data)
		if err != nil {
			return removed, fmt.Errorf("%s: %w", e.Name(), err)
		}
		target := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "versiondb-"+v+"-"), ".json")
		paths, err := archivePaths(db, target, cfg.MacOSDmg)
		if err != nil {
			return removed, err
		}
		for _, rel := range paths {
			keep[rel] = true
		}
	}
	if !cfg.Prune {
		return removed, nil
	}
	for _, t := range cfg.Targets {
		for _, v := range []string{st.JuliaupVersion, st.PreviousJuliaupVersion} {
			for _, rel := range juliaupPaths(v, t) {
				keep[rel] = true
			}
		}
	}
	roots := []string{"juliaup/bin", "juliaup/winmsi"}
	if !cfg.MetadataOnly {
		roots = append(roots, "bin")
	}
	for _, root := range roots {
		var dirs []string
		err := filepath.WalkDir(filepath.Join(cfg.Out, filepath.FromSlash(root)), func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if d.IsDir() {
				dirs = append(dirs, p)
				return nil
			}
			rel, err := filepath.Rel(cfg.Out, p)
			if err != nil {
				return err
			}
			if keep[filepath.ToSlash(rel)] {
				return nil
			}
			cfg.Logf("pruned %s", filepath.ToSlash(rel))
			removed++
			return os.Remove(p)
		})
		if err != nil {
			return removed, err
		}
		// Deepest first; os.Remove fails on, and so keeps, non-empty ones.
		for i := len(dirs) - 1; i > 0; i-- {
			_ = os.Remove(dirs[i])
		}
	}
	return removed, nil
}

// removeTempFiles deletes partial downloads a killed sync left behind. The
// caller holds the lock, so no other sync is writing them.
func removeTempFiles(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), tmpPrefix) {
			return os.Remove(p)
		}
		return nil
	})
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
