package bundler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// OutputMode selects artifact synthesis.
type OutputMode int

const (
	// OutputSingle emits one artifact containing every entry closure.
	OutputSingle OutputMode = iota
	// OutputSplit emits one deterministic-named artifact per entry.
	OutputSplit
)

// Artifact is one published deliverable.
type Artifact struct {
	Name    string
	Entries []string
	Content []byte
}

type manifestFile struct {
	Artifacts []manifestEntry `json:"artifacts"`
}

type manifestEntry struct {
	Name    string   `json:"name"`
	Entries []string `json:"entries"`
	Size    int      `json:"size"`
	SHA256  string   `json:"sha256"`
}

// ArtifactStore publishes bundles atomically under a releases/ layout with a
// current/ symlink. A release becomes visible only after every file is staged
// and synced and the symlink is atomically swapped, so partially written
// output is never externally observable. Release directory names are derived
// purely from artifact content, so identical inputs always publish the same
// release id and identical bytes.
type ArtifactStore struct {
	dir string
}

func NewArtifactStore(dir string) *ArtifactStore { return &ArtifactStore{dir: dir} }

// releaseID is a deterministic digest of the complete artifact set.
func releaseID(artifacts []Artifact) string {
	ordered := append([]Artifact(nil), artifacts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	h := sha256.New()
	h.Write([]byte("release1\n"))
	for _, a := range ordered {
		h.Write([]byte(a.Name))
		h.Write([]byte{0})
		entries := append([]string(nil), a.Entries...)
		sort.Strings(entries)
		for _, e := range entries {
			h.Write([]byte(e))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
		sum := sha256.Sum256(a.Content)
		h.Write(sum[:])
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}

// Publish stages every artifact, then atomically makes the new release
// current. On failure or cancellation staging is discarded and the previously
// current release stays in place.
func (s *ArtifactStore) Publish(ctx context.Context, artifacts []Artifact) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", ctx.Err()
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "releases"), 0o755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}
	id := releaseID(artifacts)
	relDir := filepath.Join(s.dir, "releases", id)

	if _, err := os.Stat(filepath.Join(relDir, "MANIFEST.json")); err == nil {
		// An identical release already exists; just repoint current.
		if err := s.swapCurrent(id); err != nil {
			return "", err
		}
		return relDir, nil
	}

	staging, err := os.MkdirTemp(s.dir, ".staging-*")
	if err != nil {
		return "", err
	}
	aborted := true
	defer func() {
		if aborted {
			_ = os.RemoveAll(staging)
		}
	}()

	ordered := append([]Artifact(nil), artifacts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	mf := manifestFile{Artifacts: make([]manifestEntry, 0, len(ordered))}
	for _, a := range ordered {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !safeName(a.Name) {
			return "", fmt.Errorf("unsafe artifact name %q", a.Name)
		}
		dst := filepath.Join(staging, filepath.FromSlash(a.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := writeFileAtomicInto(dst, a.Content); err != nil {
			return "", err
		}
		sum := sha256.Sum256(a.Content)
		entries := append([]string(nil), a.Entries...)
		sort.Strings(entries)
		mf.Artifacts = append(mf.Artifacts, manifestEntry{
			Name: a.Name, Entries: entries, Size: len(a.Content),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}

	var mbuf bytes.Buffer
	enc := json.NewEncoder(&mbuf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(mf); err != nil {
		return "", err
	}
	if err := writeFileAtomicInto(filepath.Join(staging, "MANIFEST.json"), mbuf.Bytes()); err != nil {
		return "", err
	}
	if err := syncDir(staging); err != nil {
		return "", err
	}

	// Atomically move the fully formed staging tree into releases/. If the
	// target appeared meanwhile (concurrent identical build), content is the
	// same by construction; discard our staging copy.
	if err := os.Rename(staging, relDir); err != nil {
		if _, serr := os.Stat(filepath.Join(relDir, "MANIFEST.json")); serr == nil {
			_ = os.RemoveAll(staging)
		} else {
			return "", fmt.Errorf("publish release: %w", err)
		}
	} else {
		if err := syncDir(filepath.Join(s.dir, "releases")); err != nil {
			return "", err
		}
	}
	aborted = false

	if err := s.swapCurrent(id); err != nil {
		return "", err
	}
	s.pruneOldReleases(id)
	return relDir, nil
}

// swapCurrent points current/ at releases/id via a temporary symlink and
// atomic rename. Existing current from older layouts (e.g. a directory) is
// replaced defensively.
func (s *ArtifactStore) swapCurrent(id string) error {
	link := filepath.Join(s.dir, "current")
	tmp := filepath.Join(s.dir, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Join("releases", id), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		// Rename over a non-symlink directory fails on Linux; clear it once.
		_ = os.RemoveAll(link)
		if err2 := os.Rename(tmp, link); err2 != nil {
			return fmt.Errorf("activate release: %w", err2)
		}
	}
	return syncDir(s.dir)
}

func (s *ArtifactStore) pruneOldReleases(keep string) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "releases"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() != keep {
			_ = os.RemoveAll(filepath.Join(s.dir, "releases", e.Name()))
		}
	}
}

// CurrentDir returns the path behind the current symlink, or "" if none.
func (s *ArtifactStore) CurrentDir() string {
	target, err := os.Readlink(filepath.Join(s.dir, "current"))
	if err != nil {
		return ""
	}
	if filepath.IsAbs(target) {
		return target
	}
	return filepath.Join(s.dir, target)
}

func safeName(name string) bool {
	if name == "" || filepath.IsAbs(name) {
		return false
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || clean != filepath.ToSlash(name) {
		return false
	}
	for _, p := range filepath.SplitList(filepath.ToSlash(name)) {
		if p == ".." {
			return false
		}
	}
	for i := 0; i < len(name); i++ {
		if name[i] == 0 {
			return false
		}
	}
	return true
}

func writeFileAtomicInto(dst string, content []byte) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
