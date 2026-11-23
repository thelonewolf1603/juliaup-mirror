package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// versionsJSON is where Julia's release list, with a SHA256 for every
// archive, lives relative to the server root.
const versionsJSON = "bin/versions.json"

// Checksums maps an archive path relative to the server root
// ("bin/linux/x64/1.10/julia-1.10.14-linux-x86_64.tar.gz") to its SHA256.
type Checksums map[string]string

// ParseChecksums reads the hashes out of a versions.json file. Its URLs name
// the official host; only their paths matter, since mirrors keep the layout.
func ParseChecksums(data []byte) (Checksums, error) {
	var releases map[string]struct {
		Files []struct {
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &releases); err != nil {
		return nil, fmt.Errorf("parse %s: %w", versionsJSON, err)
	}
	sums := Checksums{}
	for _, r := range releases {
		for _, f := range r.Files {
			u, err := url.Parse(f.URL)
			if err != nil || f.SHA256 == "" {
				continue
			}
			sums[strings.TrimPrefix(u.Path, "/")] = strings.ToLower(f.SHA256)
		}
	}
	return sums, nil
}

// VerifyResult summarises a Verify run.
type VerifyResult struct {
	OK, Bad, Unknown int
}

// Verify checks every archive under root/bin against root/bin/versions.json.
// Archives the file has no hash for are counted as unknown.
func Verify(root string, logf func(string, ...any)) (VerifyResult, error) {
	var res VerifyResult
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(versionsJSON)))
	if err != nil {
		return res, fmt.Errorf("%s: %w (sync without -skip-checksums writes it)", versionsJSON, err)
	}
	sums, err := ParseChecksums(data)
	if err != nil {
		return res, err
	}
	err = filepath.WalkDir(filepath.Join(root, "bin"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == versionsJSON {
			return nil
		}
		want, ok := sums[rel]
		if !ok {
			res.Unknown++
			logf("no hash for %s", rel)
			return nil
		}
		got, err := fileSHA256(p)
		if err != nil {
			return err
		}
		if got != want {
			res.Bad++
			logf("MISMATCH %s: got %s, want %s", rel, got, want)
			return nil
		}
		res.OK++
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return res, err
}

func fileSHA256(p string) (string, error) {
	fh, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
