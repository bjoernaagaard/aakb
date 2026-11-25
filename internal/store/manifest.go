package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"
)

const (
	manifestFilename = "downloads.json"
	manifestVersion  = 1
	maxManifestBytes = 16 << 20
	manifestLockKey  = "__manifest__"
)

// Manifest is the persisted JSON index of verified local downloads.
type Manifest struct {
	Version  int              `json:"version"`
	ByMD5    map[string]Entry `json:"by_md5"`
	BySHA256 map[string]Entry `json:"by_sha256"`
	ByKey    map[string]Entry `json:"by_key"`
}

// Entry records one finalized file inside the store's download directory.
type Entry struct {
	File            string `json:"file"`
	Bytes           int64  `json:"bytes"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
	MD5             string `json:"md5,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	MIMEType        string `json:"mime_type,omitempty"`
}

func newManifest() *Manifest {
	return &Manifest{
		Version:  manifestVersion,
		ByMD5:    make(map[string]Entry),
		BySHA256: make(map[string]Entry),
		ByKey:    make(map[string]Entry),
	}
}

func (m *Manifest) ensureMaps() {
	if m.Version == 0 {
		m.Version = manifestVersion
	}
	if m.ByMD5 == nil {
		m.ByMD5 = make(map[string]Entry)
	}
	if m.BySHA256 == nil {
		m.BySHA256 = make(map[string]Entry)
	}
	if m.ByKey == nil {
		m.ByKey = make(map[string]Entry)
	}
}

func (s *Store) loadManifestUnlocked() (*Manifest, error) {
	root, err := os.OpenRoot(s.cacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newManifest(), nil
		}
		return nil, localIOError(err)
	}
	defer func() { _ = root.Close() }() //nolint:errcheck // closing read-only cache root

	file, err := root.OpenFile(manifestFilename, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newManifest(), nil
		}
		return nil, localIOError(err)
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // closing read-only manifest file

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return newManifest(), nil
	}
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return nil, localIOError(err)
	}
	if int64(len(data)) > maxManifestBytes || len(data) == 0 {
		return newManifest(), nil
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		// Treat a corrupted manifest as empty so future downloads can recover.
		return newManifest(), nil
	}
	m.ensureMaps()
	return &m, nil
}

func (s *Store) saveManifestUnlocked(m *Manifest) error {
	if err := os.MkdirAll(s.cacheDir, privateDirMode); err != nil {
		return localIOError(err)
	}
	m.ensureMaps()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return localIOError(err)
	}
	data = append(data, '\n')

	temp, err := os.CreateTemp(s.cacheDir, ".downloads-*.tmp")
	if err != nil {
		return localIOError(err)
	}
	tempName := temp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tempName) //nolint:errcheck // cleanup failed temporary manifest
		}
	}()

	if err := temp.Chmod(privateFileMode); err != nil {
		_ = temp.Close() //nolint:errcheck // already returning chmod error
		return localIOError(err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close() //nolint:errcheck // already returning write error
		return localIOError(err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close() //nolint:errcheck // already returning sync error
		return localIOError(err)
	}
	if err := temp.Close(); err != nil {
		return localIOError(err)
	}
	if err := os.Rename(tempName, s.manifestPath); err != nil {
		return localIOError(err)
	}
	renamed = true
	return nil
}

// mutateManifest loads, mutates, and atomically persists downloads.json under both
// an in-process mutex and a cross-process lease so concurrent writers do not clobber
// each other's entries.
func (s *Store) mutateManifest(ctx context.Context, mutate func(m *Manifest) bool) error {
	s.manifestMu.Lock()
	defer s.manifestMu.Unlock()

	unlock, _, err := acquireLease(ctx, s.locksDir, s.downloadDir, manifestLockKey)
	if err != nil {
		return err
	}
	defer unlock()

	m, err := s.loadManifestUnlocked()
	if err != nil {
		return err
	}
	if !mutate(m) {
		return nil
	}
	return s.saveManifestUnlocked(m)
}

func (s *Store) readManifestEntry(lookup func(m *Manifest) (Entry, bool)) (Entry, bool) {
	s.manifestMu.Lock()
	defer s.manifestMu.Unlock()

	m, err := s.loadManifestUnlocked()
	if err != nil {
		return Entry{}, false
	}
	return lookup(m)
}
