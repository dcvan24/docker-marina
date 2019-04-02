package cache

import (
	"container/list"
	"strings"
	"sync"

	"github.com/docker/docker/daemon/images"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/sirupsen/logrus"
)

type layerLRUCache struct {
	*cacheBase
	images    map[image.ID]*image.Image
	layers    map[layer.ChainID]*list.Element
	evictList *list.List
}

type cacheLayer struct {
	layer  layer.Layer
	size   int64
	images []string
	os     string
}

func newLayerLRUCache(capacity int64, is *images.ImageService) ImageCache {
	return &layerLRUCache{
		cacheBase: &cacheBase{
			imageService: is,
			capacity:     capacity,
			mu:           &sync.RWMutex{},
		},
		images:    make(map[image.ID]*image.Image),
		layers:    make(map[layer.ChainID]*list.Element),
		evictList: list.New(),
	}
}

// PutImage implements the ImageCache interface
func (c *layerLRUCache) PutImage(img *image.Image) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if img == nil {
		return
	}

	if err := c.checkImageSize(img); err != nil {
		logrus.Errorf("error putting image in cache: %v", err)
		return
	}

	var (
		diffIDs  []layer.DiffID
		chainIDs []layer.ChainID
	)
	c.images[img.ID()] = img
	for _, diffID := range img.RootFS.DiffIDs {
		diffIDs = append(diffIDs, diffID)
		chainID := layer.CreateChainID(diffIDs)
		chainIDs = append([]layer.ChainID{chainID}, chainIDs...)
	}

	for _, chainID := range chainIDs {
		c.putLayer(chainID, img)
	}

}

func (c *layerLRUCache) putLayer(chainID layer.ChainID, img *image.Image) {
	defer c.evict(img.ID())

	if e, ok := c.layers[chainID]; ok {
		oldLayer := e.Value.(*cacheLayer)
		c.evictList.Remove(e)
		c.level -= oldLayer.size
	}

	l, err := c.imageService.GetReadOnlyLayer(chainID, img.OperatingSystem())
	if err != nil {
		logrus.Errorf("error getting layer: %v", err)
		return
	}

	size, err := l.DiffSize()
	if err != nil {
		logrus.Errorf("error getting layer size: %v", err)
		return
	}
	cl := &cacheLayer{
		layer:  l,
		size:   size,
		images: []string{img.ImageID()},
		os:     img.OperatingSystem(),
	}

	c.layers[chainID] = c.evictList.PushFront(cl)
	c.level += size
	logrus.Infof("Put layer %s, %d/%d (%.3f)", chainID, c.level, c.capacity, c.percent())
}

// UpdateImage implements the ImageCache interface
func (c *layerLRUCache) UpdateImage(refOrID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	img, err := c.imageService.GetImage(refOrID)
	if err != nil {
		logrus.Warnf("error getting image: %v", err)
		return
	}

	var (
		diffIDs  []layer.DiffID
		chainIDs []layer.ChainID
	)
	for _, diffID := range img.RootFS.DiffIDs {
		diffIDs = append(diffIDs, diffID)
		chainID := layer.CreateChainID(diffIDs)
		chainIDs = append([]layer.ChainID{chainID}, chainIDs...)
	}

	for _, chainID := range chainIDs {
		c.updateLayer(chainID, img)
	}

}

func (c *layerLRUCache) updateLayer(chainID layer.ChainID, img *image.Image) {
	defer c.evict("")
	e, ok := c.layers[chainID]
	if !ok {
		logrus.Debugf("Layer %s is not in cache", chainID)
		return
	}
	cl := e.Value.(*cacheLayer)
	cl.images = append(cl.images, img.ImageID())
	c.evictList.MoveToFront(e)

	logrus.Infof("Updated layer %s, %d/%d (%.3f)", chainID, c.level, c.capacity, c.percent())
}

// RemoveImage implements the ImageCache interface
func (c *layerLRUCache) RemoveImage(imgID image.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()

	img, ok := c.images[imgID]
	if !ok {
		return
	}
	delete(c.images, imgID)
	var diffIDs []layer.DiffID
	for _, diffID := range img.RootFS.DiffIDs {
		diffIDs = append(diffIDs, diffID)
		c.removeLayer(layer.CreateChainID(diffIDs))
	}
}

func (c *layerLRUCache) removeLayer(chainID layer.ChainID) {
	e, ok := c.layers[chainID]
	if !ok {
		logrus.Debugf("Layer %s is not in cache", chainID)
		return
	}
	cl := e.Value.(*cacheLayer)
	released, err := c.imageService.ReleaseReadOnlyLayer(cl.layer, cl.os)
	if err != nil {
		logrus.Errorf("error releasing layer: %v", err)
		return
	}
	for _, l := range released {
		e, ok := c.layers[l.ChainID]
		if !ok {
			logrus.Warnf("Layer %s is not in cache", l.ChainID)
			continue
		}
		c.level -= l.DiffSize
		delete(c.layers, l.ChainID)
		if err := deleteArchive(l.DiffID); err != nil {
			logrus.Warnf("error deleting layer archive: %v", err)
		}
		c.evictList.Remove(e)
		logrus.Infof("Removed layer %s, %d/%d (%.3f)", l.ChainID, c.level, c.capacity, c.percent())
	}

}

func (c *layerLRUCache) evict(current image.ID) {
	if c.evictList.Len() == 0 {
		logrus.Debug("Empty cache, nothing to evict")
		return
	}

	checkboard := make(map[layer.ChainID]int)

	for c.capacity < c.level {
		e := c.evictList.Back()
		cl := e.Value.(*cacheLayer)
		chainID := cl.layer.ChainID()

		logrus.Infof("Eviciting %s, %d/%d (%.3f)", chainID, c.level, c.capacity, c.percent())

		var conflict bool
		for _, imgID := range cl.images {
			if imgID == current.String() {
				conflict = true
				break
			}
			if _, err := c.imageService.ImageDelete(imgID, false, false); err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "conflict") {
					conflict = true
					break
				}
				if !strings.Contains(strings.ToLower(err.Error()), "no such image") {
					logrus.Errorf("error deleting image: %v", err)
					return
				}
			}
		}

		if conflict {
			logrus.Debugf("Image deletion conflict detected, skip")
			checkboard[chainID]++
			if checkboard[chainID] > 3 {
				logrus.Warnf("Exceeding the max eviction retries, abort")
				return
			}
			continue
		}

		var (
			released []layer.Metadata
			err      error
		)
		for released, err = c.imageService.ReleaseReadOnlyLayer(cl.layer, cl.os); err != nil; {
			if strings.Contains(strings.ToLower(err.Error()), "layer not retained") {
				// Bump up the number of references to the layer to evict in order to
				// clean up the layer data
				if cl.layer, err = c.imageService.GetReadOnlyLayer(chainID, cl.os); err != nil {
					logrus.Errorf("error getting layer: %v", err)
					return
				}
			} else {
				logrus.Errorf("error releasing layer: %v", err)
			}
		}

		if len(released) == 0 {
			logrus.Infof("Layer %s seems being used, skip", chainID)
			c.evictList.MoveToFront(e)
			checkboard[chainID]++
			if checkboard[chainID] > 3 {
				logrus.Warnf("Exceeding the max eviction retries, abort")
				return
			}
			continue
		}

		for _, l := range released {
			e, ok := c.layers[l.ChainID]
			if !ok {
				logrus.Warnf("Layer %s is not in cache", l.ChainID)
				continue
			}
			c.level -= l.DiffSize
			delete(c.layers, l.ChainID)
			c.evictList.Remove(e)
			logrus.Infof("Evicted layer %s, %d/%d (%.3f)", l.ChainID, c.level, c.capacity, c.percent())
		}

	}

}
