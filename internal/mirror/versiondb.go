package mirror

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// VersionDB is one juliaup versiondb-<version>-<target>.json file. Entries are
// kept as raw JSON so fields this tool does not know about pass through.
type VersionDB struct {
	Versions map[string]json.RawMessage
	Channels map[string]json.RawMessage
	extra    map[string]json.RawMessage
}

type versionEntry struct {
	URLPath string `json:"UrlPath"`
}

type channelEntry struct {
	Version string `json:"Version"`
}

// ParseVersionDB decodes a versiondb file.
func ParseVersionDB(data []byte) (*VersionDB, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse versiondb: %w", err)
	}
	db := &VersionDB{extra: top}
	if err := json.Unmarshal(top["AvailableVersions"], &db.Versions); err != nil {
		return nil, fmt.Errorf("parse versiondb AvailableVersions: %w", err)
	}
	if err := json.Unmarshal(top["AvailableChannels"], &db.Channels); err != nil {
		return nil, fmt.Errorf("parse versiondb AvailableChannels: %w", err)
	}
	delete(top, "AvailableVersions")
	delete(top, "AvailableChannels")
	delete(top, "Version")
	return db, nil
}

// URLPath returns the download path of a version, relative to the server root.
func (db *VersionDB) URLPath(version string) (string, error) {
	var e versionEntry
	if err := json.Unmarshal(db.Versions[version], &e); err != nil {
		return "", fmt.Errorf("version %q: %w", version, err)
	}
	if e.URLPath == "" {
		return "", fmt.Errorf("version %q has no UrlPath", version)
	}
	return e.URLPath, nil
}

// ChannelVersion returns the version a channel points to.
func (db *VersionDB) ChannelVersion(channel string) (string, error) {
	var e channelEntry
	if err := json.Unmarshal(db.Channels[channel], &e); err != nil {
		return "", fmt.Errorf("channel %q: %w", channel, err)
	}
	return e.Version, nil
}

// Encode writes the versiondb with the given Version field. Go sorts map keys,
// so the output is deterministic.
func (db *VersionDB) Encode(version string) ([]byte, error) {
	out := make(map[string]any, len(db.extra)+3)
	for k, v := range db.extra {
		out[k] = v
	}
	out["AvailableVersions"] = db.Versions
	out["AvailableChannels"] = db.Channels
	out["Version"] = version
	return json.MarshalIndent(out, "", "  ")
}

// Selection says which channels to keep.
type Selection struct {
	// Patterns are matched (path.Match) against the channel name without its
	// "~arch" suffix, e.g. "release", "lts", "1.10", "1.11.*".
	Patterns []string
	// ArchVariants keeps every "name~arch" channel of a selected name. Without
	// it a variant is kept only when it points at an already kept version.
	ArchVariants bool
}

// All reports whether the selection keeps every channel.
func (s Selection) All() bool {
	return len(s.Patterns) == 1 && s.Patterns[0] == "*"
}

// Filter returns a copy of db holding only the selected channels and the
// versions they point to, and which patterns matched at least one channel.
func (s Selection) Filter(db *VersionDB) (*VersionDB, map[string]bool, error) {
	matched := map[string]bool{}
	out := &VersionDB{
		Versions: map[string]json.RawMessage{},
		Channels: map[string]json.RawMessage{},
		extra:    db.extra,
	}
	var variants []string
	for name := range db.Channels {
		base, _, isVariant := strings.Cut(name, "~")
		pattern, ok := s.match(base)
		if !ok {
			continue
		}
		matched[pattern] = true
		if isVariant && !s.ArchVariants {
			variants = append(variants, name)
			continue
		}
		if err := out.keepChannel(db, name); err != nil {
			return nil, nil, err
		}
	}
	for _, name := range variants {
		v, err := db.ChannelVersion(name)
		if err != nil {
			return nil, nil, err
		}
		if _, kept := out.Versions[v]; kept {
			out.Channels[name] = db.Channels[name]
		}
	}
	return out, matched, nil
}

func (s Selection) match(base string) (string, bool) {
	for _, p := range s.Patterns {
		if ok, _ := path.Match(p, base); ok {
			return p, true
		}
	}
	return "", false
}

func (db *VersionDB) keepChannel(src *VersionDB, name string) error {
	v, err := src.ChannelVersion(name)
	if err != nil {
		return err
	}
	entry, ok := src.Versions[v]
	if !ok {
		return fmt.Errorf("channel %q points to unknown version %q", name, v)
	}
	db.Channels[name] = src.Channels[name]
	db.Versions[v] = entry
	return nil
}

// URLPaths lists the distinct download paths in db, sorted.
func (db *VersionDB) URLPaths() ([]string, error) {
	seen := map[string]bool{}
	for v := range db.Versions {
		p, err := db.URLPath(v)
		if err != nil {
			return nil, err
		}
		seen[p] = true
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

// contentHash hashes the channel and version tables of several versiondbs.
func contentHash(dbs map[string]*VersionDB) (string, error) {
	targets := make([]string, 0, len(dbs))
	for t := range dbs {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	h := sha256.New()
	for _, t := range targets {
		data, err := dbs[t].Encode("")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\n%s\n", t, data)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// DBVersion is a juliaup versiondb version: plain major.minor.patch.
type DBVersion struct{ Major, Minor, Patch int }

// ParseDBVersion parses the contents of a *DBVERSION file.
func ParseDBVersion(s string) (DBVersion, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 {
		return DBVersion{}, fmt.Errorf("db version %q: want major.minor.patch", s)
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return DBVersion{}, fmt.Errorf("db version %q: bad component %q", s, p)
		}
		n[i] = v
	}
	return DBVersion{n[0], n[1], n[2]}, nil
}

func (v DBVersion) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Less reports whether v sorts before w.
func (v DBVersion) Less(w DBVersion) bool {
	if v.Major != w.Major {
		return v.Major < w.Major
	}
	if v.Minor != w.Minor {
		return v.Minor < w.Minor
	}
	return v.Patch < w.Patch
}

// maxRevisions is how many mirror revisions fit under one upstream version.
const maxRevisions = 1000

// MirrorDBVersion is the version the mirror publishes for an upstream version
// and a mirror revision.
//
// juliaup only downloads a versiondb whose version is higher than the one it
// has (bundled or cached). A filtered versiondb must sort above the upstream
// one it came from, or clients that saw upstream keep its full channel list,
// and every change on the mirror needs a higher number than the last. Scaling
// the patch by maxRevisions does both: upstream 1.0.104 becomes 1.0.104000,
// 1.0.104001, ... and upstream 1.0.105 becomes 1.0.105000.
//
// The cost: mirror versions sort above every realistic upstream version, so
// a client that leaves the mirror keeps the mirror's versiondb until its
// cached copy is deleted.
func MirrorDBVersion(upstream DBVersion, revision int) (DBVersion, error) {
	if revision < 0 || revision >= maxRevisions {
		return DBVersion{}, fmt.Errorf("mirror revision %d out of range [0, %d)", revision, maxRevisions)
	}
	return DBVersion{upstream.Major, upstream.Minor, upstream.Patch*maxRevisions + revision}, nil
}
