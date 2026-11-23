package mirror

import (
	"sort"
	"testing"
)

const sampleDB = `{
  "AvailableVersions": {
    "1.10.14+0.x64.linux.gnu": {"UrlPath": "bin/linux/x86_64/1.10/julia-1.10.14-linux-x86_64.tar.gz"},
    "1.10.14+0.x86.linux.gnu": {"UrlPath": "bin/linux/x86/1.10/julia-1.10.14-linux-i686.tar.gz"},
    "1.13.4+0.x64.linux.gnu": {"UrlPath": "bin/linux/x86_64/1.13/julia-1.13.4-linux-x86_64.tar.gz"},
    "1.13.3+0.x64.linux.gnu": {"UrlPath": "bin/linux/x86_64/1.13/julia-1.13.3-linux-x86_64.tar.gz"}
  },
  "AvailableChannels": {
    "release": {"Version": "1.13.4+0.x64.linux.gnu"},
    "release~x64": {"Version": "1.13.4+0.x64.linux.gnu"},
    "lts": {"Version": "1.10.14+0.x64.linux.gnu"},
    "lts~x86": {"Version": "1.10.14+0.x86.linux.gnu"},
    "1.13.3": {"Version": "1.13.3+0.x64.linux.gnu"},
    "1.13.4": {"Version": "1.13.4+0.x64.linux.gnu"}
  },
  "Version": "1.0.104",
  "FutureField": 7
}`

func channelNames(db *VersionDB) []string {
	var out []string
	for c := range db.Channels {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func TestFilter(t *testing.T) {
	db, err := ParseVersionDB([]byte(sampleDB))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		sel       Selection
		wantChans []string
		wantPaths int
	}{
		{"release keeps same-version variant", Selection{Patterns: []string{"release"}}, []string{"release", "release~x64"}, 1},
		{"lts drops x86 variant by default", Selection{Patterns: []string{"lts"}}, []string{"lts"}, 1},
		{"arch variants on", Selection{Patterns: []string{"lts"}, ArchVariants: true}, []string{"lts", "lts~x86"}, 2},
		{"glob", Selection{Patterns: []string{"1.13.*"}}, []string{"1.13.3", "1.13.4"}, 2},
		{"all", Selection{Patterns: []string{"*"}, ArchVariants: true}, []string{"1.13.3", "1.13.4", "lts", "lts~x86", "release", "release~x64"}, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, matched, err := tc.sel.Filter(db)
			if err != nil {
				t.Fatal(err)
			}
			if names := channelNames(got); !equal(names, tc.wantChans) {
				t.Errorf("channels = %v, want %v", names, tc.wantChans)
			}
			paths, err := got.URLPaths()
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) != tc.wantPaths {
				t.Errorf("paths = %v, want %d", paths, tc.wantPaths)
			}
			for _, p := range tc.sel.Patterns {
				if !matched[p] {
					t.Errorf("pattern %q not reported as matched", p)
				}
			}
		})
	}
}

func TestEncodeKeepsUnknownFields(t *testing.T) {
	db, err := ParseVersionDB([]byte(sampleDB))
	if err != nil {
		t.Fatal(err)
	}
	out, err := db.Encode("1.0.104000")
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseVersionDB(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.extra["FutureField"]) != "7" {
		t.Errorf("FutureField lost: %s", out)
	}
}

func TestMirrorDBVersionOrdering(t *testing.T) {
	up, err := ParseDBVersion("1.0.104\n")
	if err != nil {
		t.Fatal(err)
	}
	m0, _ := MirrorDBVersion(up, 0)
	m5, _ := MirrorDBVersion(up, 5)
	nextUp, _ := MirrorDBVersion(DBVersion{1, 0, 105}, 0)
	if m0.String() != "1.0.104000" || m5.String() != "1.0.104005" {
		t.Fatalf("got %s, %s", m0, m5)
	}
	if !(less(up, m0) && less(m0, m5) && less(m5, DBVersion{1, 0, 105000}) && m5.Patch < nextUp.Patch) {
		t.Error("mirror versions must sort above their upstream and below the next upstream's mirror versions")
	}
	if _, err := MirrorDBVersion(up, maxRevisions); err == nil {
		t.Error("want error for revision overflow")
	}
	if _, err := ParseDBVersion("1.0"); err == nil {
		t.Error("want error for two components")
	}
}

func less(a, b DBVersion) bool {
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	return a.Patch < b.Patch
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
