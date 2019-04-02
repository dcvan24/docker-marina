package daemon

import (
	"context"
	"io"

	"github.com/docker/docker/daemon/cache"
	"github.com/docker/docker/daemon/images"

	"github.com/sirupsen/logrus"

	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/image"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Wrapper is
type Wrapper struct {
	*Daemon
	*images.ImageService
	cache.ImageCache
}

// NewWrapper creates the cache proxy
func NewWrapper(d *Daemon) *Wrapper {
	return &Wrapper{
		Daemon:       d,
		ImageService: d.ImageService(),
		ImageCache:   d.ImageCache(),
	}
}

// PullImage puts the image in cache
func (c *Wrapper) PullImage(ctx context.Context, image, tag string, platform *specs.Platform, metaHeaders map[string][]string, authConfig *types.AuthConfig, outStream io.Writer) error {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return errdefs.InvalidParameter(err)
	}

	if tag != "" {
		// The "tag" could actually be a digest.
		var dgst digest.Digest
		dgst, err = digest.Parse(tag)
		if err == nil {
			ref, err = reference.WithDigest(reference.TrimNamed(ref), dgst)
		} else {
			ref, err = reference.WithTag(ref, tag)
		}
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
	}

	err = c.ImageService.PullImage(ctx, image, tag, platform, metaHeaders, authConfig, outStream)
	if err != nil {
		return err
	}

	img, err := c.GetImage(ref.String())
	if err != nil {
		logrus.Errorf("error getting image: %v", err)
		return err
	}

	c.ImageCache.PutImage(img)
	return nil
}

// ImageDelete removes the image from the cache
func (c *Wrapper) ImageDelete(imageRef string, force, prune bool) ([]types.ImageDeleteResponseItem, error) {
	resps, err := c.ImageService.ImageDelete(imageRef, force, prune)
	if err != nil {
		return resps, err
	}
	for _, r := range resps {
		if r.Deleted == "" {
			continue
		}
		c.ImageCache.RemoveImage(image.ID(r.Deleted))
	}

	return resps, err
}

// ContainerCreate updates image in cache
func (c *Wrapper) ContainerCreate(config types.ContainerCreateConfig) (container.ContainerCreateCreatedBody, error) {
	body, err := c.Daemon.ContainerCreate(config)
	if err != nil {
		return body, err
	}
	c.ImageCache.UpdateImage(config.Config.Image)
	return body, err
}
