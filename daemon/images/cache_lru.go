package images // import "github.com/docker/docker/daemon/images"

import (
	"container/list"
	"fmt"
	"runtime"
	"sync"

	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"

	log "github.com/sirupsen/logrus"
)

type lruLayerCacheDriver struct {
	imageCacheDriver
	items     map[layer.ChainID]*list.Element
	evictList *list.List
}

type layerFunc func(l layer.Layer, imgID image.ID, childID layer.ChainID, rootID layer.ChainID) error

func newLRULayerCacheDriver(capacity int64, is *ImageService) ImageCacheDriver {
	return &lruLayerCacheDriver{
		imageCacheDriver: imageCacheDriver{
			capacity:     capacity,
			level:        0,
			imageService: is,
			cacheL:       &sync.RWMutex{},
		},
		items:     make(map[layer.ChainID]*list.Element),
		evictList: list.New(),
	}
}

func (lru *lruLayerCacheDriver) Put(img *image.Image) (err error) {
	if img == nil {
		log.Warn("Null image")
		return nil
	}
	if img.RootFS == nil {
		return fmt.Errorf("RootFS of image %s is not set", img.ImageID)
	}
	if err = lru.checkImageSize(img); err != nil {
		return err
	}
	if err = lru.applyFuncToLayers(img, lru.putLayer); err != nil {
		return err
	}
	return nil
}

func (lru *lruLayerCacheDriver) putLayer(l layer.Layer, imgID image.ID, childID layer.ChainID, rootID layer.ChainID) (err error) {
	items, evictList, cacheL := lru.items, lru.evictList, lru.cacheL
	chainID := l.ChainID()
	size, err := l.DiffSize()
	if err != nil {
		return err
	}
	cacheL.Lock()
	defer cacheL.Unlock()
	if e, ok := items[chainID]; ok {
		i := e.Value.(*imageCacheItem)
		i.images[imgID] = nil
		if len(childID) > 0 {
			i.children[childID] = nil
		}
		evictList.MoveToFront(e)
		return nil
	}
	for lru.level+size > lru.capacity {
		if err = lru.evict(imgID, rootID); err != nil {
			return err
		}
	}
	i := &imageCacheItem{
		layer:    l,
		images:   make(map[image.ID]interface{}),
		children: make(map[layer.ChainID]interface{}),
	}
	i.images[imgID] = nil
	if len(childID) > 0 {
		i.children[childID] = nil
	}
	items[chainID] = evictList.PushFront(i)
	lru.level += size
	level, capacity := lru.level, lru.capacity
	pct := float64(level) / float64(capacity)
	log.Infof("Put layer %s into cache, size: %d, level: %d/%d (%.3f)", chainID, size, level, capacity, pct)
	return nil
}

func (lru *lruLayerCacheDriver) Get(id image.ID) (err error) {
	img, err := lru.imageService.imageStore.Get(id)
	if err != nil {
		return err
	}
	if err = lru.applyFuncToLayers(img, lru.getLayer); err != nil {
		return err
	}
	return nil
}

func (lru *lruLayerCacheDriver) getLayer(l layer.Layer, imgID image.ID, childID layer.ChainID, rootID layer.ChainID) (err error) {
	items, evictList, cacheL := lru.items, lru.evictList, lru.cacheL
	chainID := l.ChainID()
	cacheL.RLock()
	if e, ok := items[chainID]; ok {
		cacheL.RUnlock()
		cacheL.Lock()
		defer cacheL.Unlock()
		i := e.Value.(*imageCacheItem)
		i.images[imgID] = nil
		evictList.MoveToFront(e)
		level, capacity := lru.level, lru.capacity
		pct := float64(level) / float64(capacity)
		log.Infof("Prioritized layer %s, level: %d/%d (%.3f)", chainID, level, capacity, pct)
		return nil
	}
	cacheL.RUnlock()
	return nil
}

func (lru *lruLayerCacheDriver) Remove(img *image.Image) (err error) {
	if img == nil {
		return nil
	}
	if err = lru.applyFuncToLayers(img, lru.removeLayer); err != nil {
		return err
	}
	return nil
}

func (lru *lruLayerCacheDriver) removeLayer(l layer.Layer, imgID image.ID, childID layer.ChainID, rootID layer.ChainID) (err error) {
	items, evictList, cacheL := lru.items, lru.evictList, lru.cacheL
	chainID := l.ChainID()
	cacheL.RLock()
	if e, ok := items[chainID]; ok {
		cacheL.RUnlock()
		cacheL.Lock()
		defer cacheL.Unlock()
		i := e.Value.(*imageCacheItem)
		size, err := l.DiffSize()
		if err != nil {
			return err
		}
		delete(i.images, imgID)
		if len(i.images) == 0 && len(i.children) == 0 {
			evictList.Remove(e)
			delete(items, chainID)
			lru.level -= size
			if l.Parent() != nil {
				lru.removeChild(l.Parent(), chainID)
			}
			level, capacity := lru.level, lru.capacity
			pct := float64(level) / float64(capacity)
			log.Infof("Removed layer %s from cache, size: %d, level: %d/%d (%.3f)", chainID, size, level, capacity, pct)
		} else {
			re := items[rootID]
			if re == nil {
				return fmt.Errorf("Root of image %s (%s) has been deleted in prior to its children", imgID, chainID)
			}
			log.Infof("Moved %s in front of its root %s", chainID, rootID)
			evictList.MoveBefore(e, re)
		}
		return nil
	}
	cacheL.RUnlock()
	return nil
}

func (lru *lruLayerCacheDriver) removeChild(l layer.Layer, childID layer.ChainID) {
	items := lru.items
	chainID := l.ChainID()
	if e, ok := items[chainID]; ok {
		i := e.Value.(*imageCacheItem)
		delete(i.children, childID)
	}
}

func (lru *lruLayerCacheDriver) evict(imgID image.ID, rootID layer.ChainID) (err error) {
	evictList, cacheL := lru.evictList, lru.cacheL
	ls, is := lru.imageService.layerStores[runtime.GOOS], lru.imageService.imageStore
	e := evictList.Back()
	if e == nil {
		log.Warnf("Cache is empty, no more items to be evicted")
		return nil
	}
	i := e.Value.(*imageCacheItem)
	l := i.layer
	chainID := l.ChainID()
	size, err := l.DiffSize()
	if err != nil {
		return err
	}

	log.Infof("Evicting layer %s, size: %d", chainID, size)
	for id := range i.images {
		if id == imgID || lru.imageService.checkImageDeleteConflict(id, conflictHard) != nil {
			evictList.MoveToFront(e)
			return nil
		}
	}

	switch removed, err := ls.ReleaseSingle(l); {
	case err != nil:
		log.Error(err)
		return err
	case removed == nil:
		return nil
	}

	for id := range i.images {
		log.Infof("Removing image %s", id)
		if _, ok := is.Map()[id]; ok {
			if err = is.DeleteReference(id); err != nil {
				log.Error(err)
			}
		}
		cacheL.Unlock()
		lru.removeLayer(l, id, "", rootID)
		cacheL.Lock()
	}

	return nil
}

func (lru *lruLayerCacheDriver) applyFuncToLayers(img *image.Image, layerFunc layerFunc) (err error) {
	ls := lru.imageService.layerStores[runtime.GOOS]
	if ls == nil {
		return fmt.Errorf("Layer store is not set")
	}
	var (
		diffIDs  []layer.DiffID
		chainIDs []layer.ChainID
	)
	for _, diffID := range img.RootFS.DiffIDs {
		diffIDs = append(diffIDs, diffID)
		chainIDs = append(chainIDs, layer.CreateChainID(diffIDs))
	}
	for i := len(chainIDs) - 1; i >= 0; i-- {
		l, err := ls.Peek(chainIDs[i])
		if err != nil {
			return err
		}
		if i < len(chainIDs)-1 {
			err = layerFunc(l, img.ID(), chainIDs[i+1], chainIDs[0])
		} else {
			err = layerFunc(l, img.ID(), "", chainIDs[0])
		}
		if err != nil {
			return err
		}
	}
	return nil
}
