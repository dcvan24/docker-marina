package cache

import (
	"sync"

	"github.com/docker/docker/daemon/images"
	"github.com/docker/docker/image"
	"github.com/sirupsen/logrus"
)

type naiveCache struct {
	*cacheBase
	images map[string]int64
}

func newNaiveCache(capacity int64, is *images.ImageService) ImageCache {
	return &naiveCache{
		cacheBase: &cacheBase{
			imageService: is,
			capacity:     capacity,
			mu:           &sync.RWMutex{},
		},
		images: make(map[string]int64),
	}
}

func (c *naiveCache) PutImage(img *image.Image) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if img == nil {
		return
	}

	if err := c.checkImageSize(img); err != nil {
		logrus.Errorf("error putting image in cache: %v", err)
		return
	}

	if _, ok := c.images[img.ImageID()]; ok {
		return
	}

	size, err := c.getImageSize(img)
	if err != nil {
		return
	}

	c.images[img.ImageID()] = size
	c.level += size
	logrus.Infof("Put image %s, %d/%d (%.3f)", img.ID(), c.level, c.capacity, c.percent())
	c.evict(img.ImageID())
}

func (c *naiveCache) UpdateImage(refOrID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.evict("")
}

func (c *naiveCache) RemoveImage(imgID image.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()

	size, ok := c.images[imgID.String()]
	if !ok {
		return
	}
	delete(c.images, imgID.String())
	c.level -= size
	logrus.Infof("Removed image %s, %d/%d (%.3f)", imgID, c.level, c.capacity, c.percent())
}

func (c *naiveCache) evict(current string) {
	if c.level > c.capacity {
		for imgID, size := range c.images {
			if imgID == current {
				continue
			}
			if _, err := c.imageService.ImageDelete(imgID, true, true); err != nil {
				logrus.Errorf("error deleting image: %v", err)
			}
			delete(c.images, imgID)
			c.level -= size
		}
		logrus.Infof("Evicted images, %d/%d (%.3f)", c.level, c.capacity, c.percent())
	}
}
