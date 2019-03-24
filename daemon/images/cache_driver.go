package images // import "github.com/docker/docker/daemon/images"

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/docker/go-units"
	log "github.com/sirupsen/logrus"
)

// ImageCacheDriver --
type ImageCacheDriver interface {
	Capacity() int64
	Level() int64
	Put(img *image.Image) error
	Get(id image.ID) error
	Remove(img *image.Image) error
	String() string
}

type imageCacheDriver struct {
	capacity     int64
	level        int64
	imageService *ImageService
	cacheL       *sync.RWMutex
}

// DefaultCacheCapacity --
const DefaultCacheCapacity = "1GB"

// NewCacheDriver --
func newCacheDriver(driver, capacity string, is *ImageService) (d ImageCacheDriver) {
	cap, err := units.RAMInBytes(capacity)
	if err != nil {
		log.Errorf(`Failed to parse the cache capacity: "%s", fall back to the default (%s)`, capacity, DefaultCacheCapacity)
		cap, _ = units.RAMInBytes(DefaultCacheCapacity)
	}
	switch driver {
	case "layer-lru":
		d = newLRULayerCacheDriver(cap, is)
	default:
		d = newLRULayerCacheDriver(cap, is)
	}
	log.Infof("Cache Driver: %s, capacity: %d bytes", d.String(), d.Capacity())
	loadExistingImages(d, is.imageStore)
	return d
}

func (d *imageCacheDriver) Capacity() int64 {
	return d.capacity
}

func (d *imageCacheDriver) Level() int64 {
	d.cacheL.RLock()
	defer d.cacheL.RUnlock()
	return d.level
}

func (d *imageCacheDriver) String() string {
	return "LRUCache"
}

func loadExistingImages(d ImageCacheDriver, is image.Store) {
	log.Infof("Loading %d images into cache", is.Len())
	for id, img := range is.Map() {
		if err := d.Put(img); err != nil {
			log.Errorf("Failed to put image %s", id)
		}
	}
}

func (d *imageCacheDriver) checkImageSize(img *image.Image) (err error) {
	ls := d.imageService.layerStores[runtime.GOOS]
	topLayer, err := ls.Peek(img.RootFS.ChainID())
	if err != nil {
		return err
	}
	topSize, err := topLayer.Size()
	if err != nil {
		return err
	}
	if topSize > d.capacity {
		return fmt.Errorf("Image %s (%d bytes) is larger than the cache capacity (%d bytes)", img.ImageID(), topSize, d.capacity)
	}
	return nil
}

type imageCacheItem struct {
	layer    layer.Layer
	images   map[image.ID]interface{}
	children map[layer.ChainID]interface{}
}
