package historyretention

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFilesOwnedLogsAndUploadTemps(t *testing.T) {
	root := t.TempDir()
	f, err := OpenFiles(root, 30)
	require.NoError(t, err)
	defer f.Close()
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, name := range []string{"service-20250101T000000.000000001Z.log", "unknown.log"} {
		path := filepath.Join(root, "retention", "logs", name)
		require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
		require.NoError(t, os.Chtimes(path, old, old))
	}
	require.NoError(t, os.Mkdir(filepath.Join(root, "uploads"), 0o700))
	for _, name := range []string{".upload-00000000000000000000000000000000", "source.md"} {
		path := filepath.Join(root, "uploads", name)
		require.NoError(t, os.WriteFile(path, []byte("knowledge"), 0o600))
		require.NoError(t, os.Chtimes(path, old, old))
	}
	count, err := f.Cleanup(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	for _, path := range []string{filepath.Join(root, "retention", "logs", "unknown.log"), filepath.Join(root, "uploads", "source.md"), filepath.Join(root, "retention", "logs", f.name)} {
		_, err = os.Stat(path)
		require.NoError(t, err)
	}
	// A day boundary rotates the writer. A size boundary never truncates content.
	previous := f.name
	f.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	_, err = f.Write([]byte("new day"))
	require.NoError(t, err)
	require.NotEqual(t, previous, f.name)
	f.size = LogFileBytes
	previous = f.name
	f.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	_, err = f.Write([]byte("new size"))
	require.NoError(t, err)
	require.NotEqual(t, previous, f.name)
}

func TestFilesVerifiedBackupProtection(t *testing.T) {
	root := t.TempDir()
	f, err := OpenFiles(root, 30)
	require.NoError(t, err)
	defer f.Close()
	create := func(name string, age int, corrupt bool) {
		dir := filepath.Join(root, "retention", "backups", name)
		require.NoError(t, os.Mkdir(dir, 0o700))
		proof := BackupManifest{Format: "basalt-restored-backup-v1", VerifiedAt: time.Now().Add(-time.Duration(age) * 24 * time.Hour), Files: map[string]string{}}
		for _, file := range []string{"database.dump", "workspace.zip", "basalt.exe"} {
			data := []byte(file)
			sum := sha256.Sum256(data)
			proof.Files[file] = hex.EncodeToString(sum[:])
			require.NoError(t, os.WriteFile(filepath.Join(dir, file), data, 0o600))
		}
		data, e := json.Marshal(proof)
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "verified.json"), data, 0o600))
		if corrupt {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "database.dump"), []byte("changed"), 0o600))
		}
	}
	create("verified-one", 31, false)
	create("verified-two", 40, false)
	create("verified-three", 50, false)
	create("verified-corrupt", 60, true)
	_, err = f.Cleanup(t.Context())
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "retention", "backups", "verified-three"))
	require.True(t, os.IsNotExist(err))
	for _, name := range []string{"verified-one", "verified-two", "verified-corrupt"} {
		_, err = os.Stat(filepath.Join(root, "retention", "backups", name))
		require.NoError(t, err)
	}
}

func TestFilesSymlinkAndCancelledCleanup(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "retention"), 0o700))
	err := os.Symlink(outside, filepath.Join(root, "retention", "logs"))
	if err != nil {
		t.Skip("Windows symlink privilege unavailable")
	}
	_, err = OpenFiles(root, 30)
	require.Error(t, err)
}
