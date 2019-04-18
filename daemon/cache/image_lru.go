package cache

import (
	"container/list"
	"strings"
	"sync"

	"github.com/docker/docker/daemon/images"
	"github.com/docker/docker/image"
	"github.com/sirupsen/logrus"
)

type imageLRUCache struct {
	*cacheBase
	images    map[image.ID]*list.Element
	evictList *list.List
}

func newImageLRUCache(capacity int64, is *images.ImageService) ImageCache {
	return &imageLRUCache{
		cacheBase: &cacheBase{
			imageService: is,
			capacity:     capacity,
			mu:           &sync.RWMutex{},
		},
		images:    make(map[image.ID]*list.Element),
		evictList: list.New(),
	}
}

// PutImage implements the ImageCache interface
func (c *imageLRUCache) PutImage(img *image.Image) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if img == nil {
		return
	}

	if err := c.checkImageSize(img); err != nil {
		logrus.Errorf("error putting image in cache: %v", err)
		return
	}

	if e, ok := c.images[img.ID()]; ok {
		c.evictList.MoveToFront(e)
		return
	}

	newSize, err := c.getImageSize(img)
	if err != nil {
		return
	}

	c.images[img.ID()] = c.evictList.PushFront(img)
	c.level += newSize
	logrus.Infof("Put image %s, %d/%d (%.3f)", img.ID(), c.level, c.capacity, c.percent())
	c.evict()
}

// UpdateImage implements the ImageCache interface
func (c *imageLRUCache) UpdateImage(refOrID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	img, err := c.imageService.GetImage(refOrID)
	if err != nil {
		logrus.Warnf("error getting image: %v", err)
		return
	}

	if e, ok := c.images[img.ID()]; ok {
		c.evictList.MoveToFront(e)
		logrus.Infof("Updated image %s, %d/%d (%.3f)", img.ID(), c.level, c.capacity, c.percent())
		return
	}
	logrus.Infof("Image %s is not in cache", img.ID())
}

// RemoveImage implements the ImageCache interface
func (c *imageLRUCache) RemoveImage(imgID image.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.images[imgID]; ok {
		img := e.Value.(*image.Image)
		size, err := c.getImageSize(img)
		if err != nil {
			return
		}
		delete(c.images, imgID)
		c.evictList.Remove(e)
		c.level -= size
		logrus.Infof("Removed image %s, %d/%d (%.3f)", imgID, c.level, c.capacity, c.percent())
		return
	}
	logrus.Warnf("Image %s is not in cache", imgID)
}

func (c *imageLRUCache) evict() {
	if c.evictList.Len() == 0 {
		logrus.Debug("Empty cache, nothing to evict")
		return
	}

	for c.capacity < c.level {
		e := c.evictList.Back()
		img := e.Value.(*image.Image)
		size, err := c.getImageSize(img)
		if err != nil {
			continue
		}

		logrus.Infof("Evicting image %s ...", img.ID())

		if _, err := c.imageService.ImageDelete(img.ImageID(), true, false); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "conflict") {
				logrus.Debugf("Image deletion conflict detected, skip")
				continue
			}
			if strings.Contains(strings.ToLower(err.Error()), "no such image") {
				logrus.Warnf("Image %s no longer exists", img.ID())
				continue
			}
			logrus.Errorf("error deleting image: %v", err)
			return
		}

		delete(c.images, img.ID())
		c.evictList.Remove(e)
		c.level -= size

		logrus.Infof("Evicted image %s, %d/%d (%.3f)", img.ID(), c.level, c.capacity, c.percent())

	}
}
