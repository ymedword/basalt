package historyretention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	LogFileBytes  int64 = 32 << 20
	LogTotalBytes int64 = 256 << 20
)

// Files operates only beneath the instance workspace. Archives are flat,
// explicitly named files; symlinks and unknown names are always retained.
type Files struct {
	mu      sync.Mutex
	root    *os.Root
	logs    *os.Root
	current *os.File
	name    string
	size    int64
	day     string
	now     func() time.Time
	days    int
}

var (
	logName        = regexp.MustCompile(`^service-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z\.log$`)
	uploadTempName = regexp.MustCompile(`^\.upload-[0-9a-f]{32}$`)
)

func OpenFiles(workspace string, days int) (*Files, error) {
	if days < 1 || days > 365 {
		return nil, errors.New("invalid file retention days")
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	f := &Files{root: root, days: days, now: time.Now}
	for _, dir := range []string{"retention", "retention/logs", "retention/backups"} {
		if err = root.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, errors.Join(err, root.Close())
		}
		st, e := root.Lstat(dir)
		if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(errors.New("retention directory is not a regular directory"), e, root.Close())
		}
	}
	f.logs, err = root.OpenRoot("retention/logs")
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	if err = f.rotate(); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func (f *Files) rotate() error {
	if f.current != nil {
		if err := f.current.Close(); err != nil {
			return err
		}
		f.current = nil
	}
	now := f.now().UTC()
	name := "service-" + now.Format("20060102T150405.000000000Z") + ".log"
	file, err := f.logs.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.current, f.name, f.size, f.day = file, name, 0, now.Format("20060102")
	return nil
}

func (f *Files) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current == nil {
		return 0, fs.ErrClosed
	}
	// Large writes are split across bounded files; a quiet service rotates by day.
	total := 0
	for len(p) > 0 {
		if f.size >= LogFileBytes || f.day != f.now().UTC().Format("20060102") {
			if err := f.rotate(); err != nil {
				return total, err
			}
			if _, err := f.pruneLogsLocked(context.Background()); err != nil {
				return total, err
			}
		}
		length := min(int64(len(p)), LogFileBytes-f.size)
		n, err := f.current.Write(p[:length])
		total += n
		f.size += int64(n)
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (f *Files) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var err error
	if f.current != nil {
		err = f.current.Close()
		f.current = nil
	}
	if f.logs != nil {
		err = errors.Join(err, f.logs.Close())
	}
	if f.root != nil {
		err = errors.Join(err, f.root.Close())
	}
	return err
}

// Cleanup never removes the current log, the newest two verified backups or
// unowned files. Backup proof is checked again before any file is removed.
func (f *Files) Cleanup(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	deleted, err := f.pruneLogsLocked(ctx)
	if err != nil {
		return deleted, err
	}
	cutoff := f.now().Add(-time.Duration(f.days) * 24 * time.Hour)
	// An upload is linked into its final name only after closing its temporary file.
	// At 30 days an orphan .upload-<nonce> is abandoned, never a knowledge source.
	if st, e := f.root.Lstat("uploads"); e == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0 {
		uploads, e := f.root.OpenRoot("uploads")
		if e != nil {
			return deleted, e
		}
		list, e := fs.ReadDir(uploads.FS(), ".")
		if e == nil {
			for _, entry := range list {
				if ctx.Err() != nil {
					e = ctx.Err()
					break
				}
				if !uploadTempName.MatchString(entry.Name()) {
					continue
				}
				st, se := uploads.Lstat(entry.Name())
				if se != nil {
					e = se
					break
				}
				if st.Mode().IsRegular() && st.ModTime().Before(cutoff) {
					if e = uploads.Remove(entry.Name()); e != nil {
						break
					}
					deleted++
				}
			}
		}
		e = errors.Join(e, uploads.Close())
		if e != nil {
			return deleted, e
		}
	}
	n, e := f.cleanupBackups(ctx, cutoff)
	return deleted + n, e
}

func (f *Files) pruneLogsLocked(ctx context.Context) (int64, error) {
	cutoff := f.now().Add(-time.Duration(f.days) * 24 * time.Hour)
	entries, err := fs.ReadDir(f.logs.FS(), ".")
	if err != nil {
		return 0, err
	}
	type archived struct {
		name string
		at   time.Time
		size int64
	}
	var archives []archived
	total := max(f.size, LogFileBytes)
	for _, entry := range entries {
		if !logName.MatchString(entry.Name()) || entry.Name() == f.name {
			continue
		}
		st, e := f.logs.Lstat(entry.Name())
		if e != nil {
			return 0, e
		}
		if !st.Mode().IsRegular() {
			continue
		}
		archives = append(archives, archived{entry.Name(), st.ModTime(), st.Size()})
		total += st.Size()
	}
	slices.SortFunc(archives, func(a, b archived) int { return a.at.Compare(b.at) })
	var deleted int64
	for _, file := range archives {
		if err = ctx.Err(); err != nil {
			return deleted, err
		}
		if !file.at.Before(cutoff) && total <= LogTotalBytes {
			continue
		}
		if err = f.logs.Remove(file.name); err != nil {
			return deleted, err
		}
		total -= file.size
		deleted++
	}

	return deleted, nil
}

// BackupManifest is produced only after an isolated database restore and archive
// verification. Legacy backups lacking this proof are deliberately unmanaged.
type BackupManifest struct {
	Format     string            `json:"format"`
	VerifiedAt time.Time         `json:"verified_at"`
	Files      map[string]string `json:"files"`
}
type verifiedBackup struct {
	name  string
	proof BackupManifest
}

func (f *Files) cleanupBackups(ctx context.Context, cutoff time.Time) (int64, error) {
	root, err := f.root.OpenRoot("retention/backups")
	if err != nil {
		return 0, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, err
	}
	var backups []verifiedBackup
	for _, e := range entries {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if !strings.HasPrefix(e.Name(), "verified-") {
			continue
		}
		proof, ok := verifyBackup(ctx, root, e.Name())
		if ok {
			backups = append(backups, verifiedBackup{e.Name(), proof})
		}
	}
	slices.SortFunc(backups, func(a, b verifiedBackup) int { return b.proof.VerifiedAt.Compare(a.proof.VerifiedAt) })
	var deleted int64
	for i, b := range backups {
		if i < 2 || !b.proof.VerifiedAt.Before(cutoff) {
			continue
		}
		proof, ok := verifyBackup(ctx, root, b.name)
		if !ok {
			continue
		}
		dir, e := root.OpenRoot(b.name)
		if e != nil {
			return deleted, e
		}
		// Remove only manifest-enumerated regular files. No recursive deletion.
		for name := range proof.Files {
			if ctx.Err() != nil {
				return deleted, errors.Join(ctx.Err(), dir.Close())
			}
			if e = dir.Remove(name); e != nil {
				return deleted, errors.Join(e, dir.Close())
			}
			deleted++
		}
		e = dir.Remove("verified.json")
		e = errors.Join(e, dir.Close())
		if e != nil {
			return deleted, e
		}
		if e = root.Remove(b.name); e != nil {
			return deleted, e
		}
		deleted++
	}
	return deleted, nil
}

func verifyBackup(ctx context.Context, root *os.Root, name string) (BackupManifest, bool) {
	var proof BackupManifest
	st, err := root.Lstat(name)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return proof, false
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return proof, false
	}
	defer dir.Close()
	st, err = dir.Lstat("verified.json")
	if err != nil || !st.Mode().IsRegular() || st.Size() > 16384 {
		return proof, false
	}
	data, err := dir.ReadFile("verified.json")
	if err != nil {
		return proof, false
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if dec.Decode(&proof) != nil || proof.Format != "basalt-restored-backup-v1" || proof.VerifiedAt.IsZero() || proof.VerifiedAt.After(time.Now()) || len(proof.Files) < 3 {
		return proof, false
	}
	if proof.Files["database.dump"] == "" || proof.Files["workspace.zip"] == "" || proof.Files["basalt.exe"] == "" {
		return proof, false
	}
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil || len(entries) != len(proof.Files)+1 {
		return proof, false
	}
	for filename, want := range proof.Files {
		if filepath.Base(filename) != filename || strings.ContainsAny(filename, "/\\:") || filename == "verified.json" || len(want) != 64 {
			return proof, false
		}
		if ctx.Err() != nil {
			return proof, false
		}
		st, e := dir.Lstat(filename)
		if e != nil || !st.Mode().IsRegular() {
			return proof, false
		}
		file, e := dir.Open(filename)
		if e != nil {
			return proof, false
		}
		h := sha256.New()
		_, e = io.Copy(h, &contextReader{ctx: ctx, r: file})
		e = errors.Join(e, file.Close())
		if e != nil || hex.EncodeToString(h.Sum(nil)) != want {
			return proof, false
		}
	}
	return proof, true
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (f *Files) Description() string {
	return fmt.Sprintf("%d days, %d MiB per log, %d MiB total logs", f.days, LogFileBytes>>20, LogTotalBytes>>20)
}
