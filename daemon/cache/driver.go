package cache

import (
	"fmt"
	"strings"
	"sync"

	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/daemon/images"
	"github.com/docker/go-units"
	"github.com/sirupsen/logrus"

	"github.com/docker/docker/image"
)

const (
	policyLayerLRU   = "layer-lru"
	policyImageLRU   = "image-lru"
	policyArchiveLRU = "archive-lru"
)

// ImageCache is the interface of the image cache
type ImageCache interface {
	Capacity() int64
	Level() int64
	PutImage(*image.Image)
	UpdateImage(string)
	RemoveImage(image.ID)
}

// NewImageCache creates a new image cache
// defaults to image-level LRU
func NewImageCache(cfg *config.Config, is *images.ImageService) (ImageCache, error) {
	capacity, err := units.RAMInBytes(cfg.CacheCapacity)
	if err != nil {
		return nil, err
	}
	switch policy := strings.ToLower(cfg.CachePolicy); policy {
	case policyImageLRU:
		return newImageLRUCache(capacity, is), nil
	case policyLayerLRU:
		return newLayerLRUCache(capacity, is), nil
	case policyArchiveLRU:
		if !cfg.CacheArchive {
			return nil, fmt.Errorf(`"--cache-archive" is required for "archive-lru" cache policy`)
		}
		return newArchiveLRUCache(capacity, is), nil
	}
	return newImageLRUCache(capacity, is), nil
}

type cacheBase struct {
	imageService *images.ImageService
	capacity     int64
	level        int64
	mu           *sync.RWMutex
}

// Capacity returns the cache capacity
func (c *cacheBase) Capacity() int64 {
	return c.capacity
}

// Level returns the cache level
func (c *cacheBase) Level() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.level
}

func (c *cacheBase) percent() float64 {
	return float64(c.level) / float64(c.capacity)
}

func (c *cacheBase) checkImageSize(img *image.Image) error {
	size, err := c.getImageSize(img)
	if err != nil {
		return err
	}
	if size > c.capacity {
		return fmt.Errorf("The size of image %s (%d) is larger than the cache capacity (%d)", img.ImageID(), size, c.capacity)
	}
	return nil
}

func (c *cacheBase) getImageSize(img *image.Image) (int64, error) {
	topLayer, err := c.imageService.GetReadOnlyLayer(img.RootFS.ChainID(), img.OperatingSystem())
	defer c.imageService.ReleaseReadOnlyLayer(topLayer, img.OperatingSystem())
	if err != nil {
		logrus.Errorf("error getting the top layer of image: %v", err)
		return 0, err
	}
	size, err := topLayer.Size()
	if err != nil {
		logrus.Errorf("error getting the layer size: %v", err)
		return 0, err
	}
	return size, nil
}
