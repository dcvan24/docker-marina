package xfer

import (
	"context"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"

	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/ioutils"
)

func createLayerArchive(ctx context.Context, downloadReader io.ReadCloser, prevErr error) (io.ReadCloser, string, error) {
	if prevErr != nil {
		return nil, "", prevErr
	}
	f, err := ioutil.TempFile("", "LayerArchive")
	if err != nil {
		return nil, "", err
	}
	path := f.Name()
	tr := io.TeeReader(downloadReader, f)
	ts := ioutils.NewReadCloserWrapper(tr, func() error {
		downloadReader.Close()
		f.Close()

		select {
		case <-ctx.Done():
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		default:
		}

		return nil
	})
	return ioutils.NewCancelReadCloser(ctx, ts), path, nil
}

func getArchiveReader(diffID layer.DiffID) (io.ReadCloser, error) {
	if diffID == "" {
		return nil, nil
	}
	path := filepath.Join(os.TempDir(), digest.Digest(diffID).Hex())
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return ioutils.NewReadCloserWrapper(f, func() error {
		f.Close()
		return nil
	}), nil
}

func renameArchive(path string, diffID layer.DiffID) error {
	newPath := filepath.Join(os.TempDir(), digest.Digest(diffID).Hex())
	logrus.Debugf("Old path: %s, new path: %s", path, newPath)
	if path == "" || path == newPath {
		return nil
	}
	if fi, _ := os.Stat(newPath); fi != nil {
		if err := os.RemoveAll(newPath); err != nil {
			return err
		}
	}
	return os.Rename(path, newPath)
}
