package http

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/locker"
	"github.com/boltdb/bolt"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/source"
	"github.com/moby/buildkit/util/tracing"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type Opt struct {
	CacheAccessor cache.Accessor
	MetadataStore *metadata.Store
}

type httpSource struct {
	md     *metadata.Store
	cache  cache.Accessor
	locker *locker.Locker
}

func NewSource(opt Opt) (source.Source, error) {
	hs := &httpSource{
		md:     opt.MetadataStore,
		cache:  opt.CacheAccessor,
		locker: locker.NewLocker(),
	}
	return hs, nil
}

func (hs *httpSource) ID() string {
	return source.HttpsScheme
}

type httpSourceHandler struct {
	*httpSource
	src      source.HttpIdentifier
	refID    string
	cacheKey digest.Digest
}

func (hs *httpSource) Resolve(ctx context.Context, id source.Identifier) (source.SourceInstance, error) {
	httpIdentifier, ok := id.(*source.HttpIdentifier)
	if !ok {
		return nil, errors.Errorf("invalid http identifier %v", id)
	}

	return &httpSourceHandler{
		src:        *httpIdentifier,
		httpSource: hs,
	}, nil
}

// urlHash is internal hash the etag is stored by that doesn't leak outside
// this package.
func (hs *httpSourceHandler) urlHash() (digest.Digest, error) {
	dt, err := json.Marshal(struct {
		Filename       string
		Perm, UID, GID int
	}{
		Filename: getFileName(hs.src.URL, hs.src.Filename, nil),
		Perm:     hs.src.Perm,
		UID:      hs.src.UID,
		GID:      hs.src.GID,
	})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(dt), nil
}

func (hs *httpSourceHandler) formatCacheKey(filename string, dgst digest.Digest) digest.Digest {
	dt, err := json.Marshal(struct {
		Filename       string
		Perm, UID, GID int
		Checksum       digest.Digest
	}{
		Filename: filename,
		Perm:     hs.src.Perm,
		UID:      hs.src.UID,
		GID:      hs.src.GID,
		Checksum: dgst,
	})
	if err != nil {
		return dgst
	}
	return digest.FromBytes(dt)
}

func (hs *httpSourceHandler) CacheKey(ctx context.Context) (string, error) {
	if hs.src.Checksum != "" {
		hs.cacheKey = hs.src.Checksum
		return hs.formatCacheKey(getFileName(hs.src.URL, hs.src.Filename, nil), hs.src.Checksum).String(), nil
	}

	uh, err := hs.urlHash()
	if err != nil {
		return "", nil
	}

	// look up metadata(previously stored headers) for that URL
	sis, err := hs.md.Search(uh.String())
	if err != nil {
		return "", errors.Wrapf(err, "failed to search metadata for %s", uh)
	}

	req, err := http.NewRequest("GET", hs.src.URL, nil)
	if err != nil {
		return "", err
	}
	req = req.WithContext(ctx)
	m := map[string]*metadata.StorageItem{}

	if len(sis) > 0 {
		for _, si := range sis {
			// if metaDigest := getMetaDigest(si); metaDigest == hs.formatCacheKey("") {
			if etag := getETag(si); etag != "" {
				if dgst := getChecksum(si); dgst != "" {
					m[etag] = si
					req.Header.Add("If-None-Match", etag)
				}
			}
			// }
		}
	}

	resp, err := tracing.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return "", errors.Errorf("invalid response status %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotModified {
		respETag := resp.Header.Get("ETag")
		si, ok := m[respETag]
		if !ok {
			return "", errors.Errorf("invalid not-modified ETag: %v", respETag)
		}
		hs.refID = si.ID()
		dgst := getChecksum(si)
		if dgst == "" {
			return "", errors.Errorf("invalid metadata change")
		}
		resp.Body.Close()
		return hs.formatCacheKey(getFileName(hs.src.URL, hs.src.Filename, resp), dgst).String(), nil
	}

	ref, dgst, err := hs.save(ctx, resp)
	if err != nil {
		return "", err
	}
	ref.Release(context.TODO())

	hs.cacheKey = dgst

	return hs.formatCacheKey(getFileName(hs.src.URL, hs.src.Filename, resp), dgst).String(), nil
}

func (hs *httpSourceHandler) save(ctx context.Context, resp *http.Response) (ref cache.ImmutableRef, dgst digest.Digest, retErr error) {
	newRef, err := hs.cache.New(ctx, nil, cache.CachePolicyRetain, cache.WithDescription(fmt.Sprintf("http url %s", hs.src.URL)))
	if err != nil {
		return nil, "", err
	}

	releaseRef := func() {
		newRef.Release(context.TODO())
	}

	defer func() {
		if retErr != nil && newRef != nil {
			releaseRef()
		}
	}()

	mount, err := newRef.Mount(ctx, false)
	if err != nil {
		return nil, "", err
	}

	lm := snapshot.LocalMounter(mount)
	dir, err := lm.Mount()
	if err != nil {
		return nil, "", err
	}

	defer func() {
		if retErr != nil && lm != nil {
			lm.Unmount()
		}
	}()
	perm := 0600
	if hs.src.Perm != 0 {
		perm = hs.src.Perm
	}
	fp := filepath.Join(dir, getFileName(hs.src.URL, hs.src.Filename, resp))
	logrus.Debugf("write to  %v", fp)

	f, err := os.OpenFile(fp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(perm))
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	h := sha256.New()

	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return nil, "", err
	}

	if err := f.Close(); err != nil {
		return nil, "", err
	}
	f = nil

	if hs.src.UID != 0 || hs.src.GID != 0 {
		if err := os.Chown(fp, hs.src.UID, hs.src.GID); err != nil {
			return nil, "", err
		}
	}

	mTime := time.Unix(0, 0)
	lastMod := resp.Header.Get("Last-Modified")
	if lastMod != "" {
		if parsedMTime, err := http.ParseTime(lastMod); err == nil {
			mTime = parsedMTime
		}
	}

	if err := os.Chtimes(fp, mTime, mTime); err != nil {
		return nil, "", err
	}

	lm.Unmount()
	lm = nil

	ref, err = newRef.Commit(ctx)
	if err != nil {
		return nil, "", err
	}
	newRef = nil

	hs.refID = ref.ID()
	dgst = digest.NewDigest(digest.SHA256, h)

	if respETag := resp.Header.Get("ETag"); respETag != "" {
		setETag(ref.Metadata(), respETag)
		uh, err := hs.urlHash()
		if err != nil {
			return nil, "", err
		}
		setChecksum(ref.Metadata(), uh.String(), dgst)
		if err := ref.Metadata().Commit(); err != nil {
			return nil, "", err
		}
	}

	return ref, dgst, nil
}

func (hs *httpSourceHandler) Snapshot(ctx context.Context) (cache.ImmutableRef, error) {
	if hs.refID != "" {
		ref, err := hs.cache.Get(ctx, hs.refID)
		if err == nil {
			return ref, nil
		}
	}

	req, err := http.NewRequest("GET", hs.src.URL, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)

	resp, err := tracing.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	ref, dgst, err := hs.save(ctx, resp)
	if err != nil {
		return nil, err
	}
	if dgst != hs.cacheKey {
		ref.Release(context.TODO())
		return nil, errors.Errorf("digest mismatch %s: %s", dgst, hs.cacheKey)
	}

	return ref, nil
}

const keyETag = "etag"
const keyChecksum = "http.checksum"

func setETag(si *metadata.StorageItem, s string) error {
	v, err := metadata.NewValue(s)
	if err != nil {
		return errors.Wrap(err, "failed to create etag value")
	}
	si.Queue(func(b *bolt.Bucket) error {
		return si.SetValue(b, keyETag, v)
	})
	return nil
}

func getETag(si *metadata.StorageItem) string {
	v := si.Get(keyETag)
	if v == nil {
		return ""
	}
	var etag string
	if err := v.Unmarshal(&etag); err != nil {
		return ""
	}
	return etag
}

func setChecksum(si *metadata.StorageItem, url string, d digest.Digest) error {
	v, err := metadata.NewValue(d)
	if err != nil {
		return errors.Wrap(err, "failed to create checksum value")
	}
	v.Index = url
	si.Queue(func(b *bolt.Bucket) error {
		return si.SetValue(b, keyChecksum, v)
	})
	return nil
}

func getChecksum(si *metadata.StorageItem) digest.Digest {
	v := si.Get(keyChecksum)
	if v == nil {
		return ""
	}
	var dgstStr string
	if err := v.Unmarshal(&dgstStr); err != nil {
		return ""
	}
	dgst, err := digest.Parse(dgstStr)
	if err != nil {
		return ""
	}
	return dgst
}

func getFileName(urlStr, manualFilename string, resp *http.Response) string {
	if manualFilename != "" {
		return manualFilename
	}
	if resp != nil {
		if contentDisposition := resp.Header.Get("Content-Disposition"); contentDisposition != "" {
			if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
				if params["filename"] != "" && !strings.HasSuffix(params["filename"], "/") {
					if filename := filepath.Base(filepath.FromSlash(params["filename"])); filename != "" {
						return filename
					}
				}
			}
		}
	}
	u, err := url.Parse(urlStr)
	if err == nil {
		if base := path.Base(u.Path); base != "." && base != "/" {
			return base
		}
	}
	return "download"
}
