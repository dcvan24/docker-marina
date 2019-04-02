package cache

import (
	"os"
	"path/filepath"

	"github.com/docker/docker/layer"
	"github.com/opencontainers/go-digest"
)

func createLayerArchivePath(diffID layer.DiffID) string {
	return filepath.Join(os.TempDir(), digest.Digest(diffID).Hex())
}

func getLayerArchiveInfo(diffID layer.DiffID) (os.FileInfo, error) {
	fi, err := os.Stat(createLayerArchivePath(diffID))
	if err != nil && os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return fi, nil
}

func deleteArchive(diffID layer.DiffID) error {
	err := os.RemoveAll(createLayerArchivePath(diffID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
