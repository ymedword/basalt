//go:build !windows

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKnowledgeUpload_RefusesLinkedUploadDirectory(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "uploads")))
	s := &knowledgeUploadAdmin{root: root}
	require.Error(t, s.writeFile(context.Background(), "test.txt", []byte("data")))
	_, err := os.Stat(filepath.Join(outside, "test.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
