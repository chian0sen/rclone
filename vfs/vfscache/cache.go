// Package vfscache deals with caching of files locally for the VFS layer
package vfscache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/rclone/rclone/fs"
	fscache "github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/vfs/vfscommon"
)

// FIXME size in cache needs to be size on disk if we have sparse files...

// Cache opened files
type Cache struct {
	fremote    fs.Fs              // fs for the remote we are caching
	fcache     fs.Fs              // fs for the cache directory
	fcacheMeta fs.Fs              // fs for the cache metadata directory
	opt        *vfscommon.Options // vfs Options
	root       string             // root of the cache directory
	metaRoot   string             // root of the cache metadata directory
	itemMu     sync.Mutex         // protects the following variables
	item       map[string]*Item   // files/directories in the cache
	used       int64              // total size of files in the cache
	hashType   hash.Type          // hash to use locally and remotely
	hashOption *fs.HashesOption   // corresponding OpenOption
}

// New creates a new cache heirachy for fremote
//
// This starts background goroutines which can be cancelled with the
// context passed in.
func New(ctx context.Context, fremote fs.Fs, opt *vfscommon.Options) (*Cache, error) {
	fRoot := filepath.FromSlash(fremote.Root())
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(fRoot, `\\?`) {
			fRoot = fRoot[3:]
		}
		fRoot = strings.Replace(fRoot, ":", "", -1)
	}
	root := filepath.Join(config.CacheDir, "vfs", fremote.Name(), fRoot)
	fs.Debugf(nil, "vfs cache root is %q", root)
	metaRoot := filepath.Join(config.CacheDir, "vfsMeta", fremote.Name(), fRoot)
	fs.Debugf(nil, "vfs metadata cache root is %q", root)

	fcache, err := fscache.Get(root)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create cache remote")
	}
	fcacheMeta, err := fscache.Get(root)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create cache meta remote")
	}

	hashType, hashOption := operations.CommonHash(fcache, fremote)

	c := &Cache{
		fremote:    fremote,
		fcache:     fcache,
		fcacheMeta: fcacheMeta,
		opt:        opt,
		root:       root,
		metaRoot:   metaRoot,
		item:       make(map[string]*Item),
		hashType:   hashType,
		hashOption: hashOption,
	}

	// Make sure cache directories exist
	_, err = c.mkdir("")
	if err != nil {
		return nil, errors.Wrap(err, "failed to make cache directory")
	}

	// load in the cache and metadata off disk
	err = c.reload()
	if err != nil {
		return nil, errors.Wrap(err, "failed to load cache")
	}

	// Remove any empty directories
	c.purgeEmptyDirs()

	go c.cleaner(ctx)

	return c, nil
}

// objectFingerprint produces a unique-ish string for an object so we
// can tell if it has changed from when it was stored in the cache
func (c *Cache) objectFingerprint(o fs.Object) string {
	ctx := context.Background()
	var out strings.Builder
	fmt.Fprintf(&out, "%d", o.Size())
	// FIXME might not want do do this for S3 where modtimes are expensive?
	if c.fremote.Precision() != fs.ModTimeNotSupported {
		fmt.Fprintf(&out, ",%v", o.ModTime(ctx).UTC())
	}
	// FIXME might not want do do this for SFTP/local where hashes
	// are expensive?
	if c.hashType != hash.None {
		hash, err := o.Hash(ctx, c.hashType)
		if err == nil {
			fmt.Fprintf(&out, ",%v", hash)
		}
	}
	return out.String()
}

// clean returns the cleaned version of name for use in the index map
func clean(name string) string {
	name = strings.Trim(name, "/")
	name = filepath.Clean(name)
	if name == "." || name == "/" {
		name = ""
	}
	return name
}

// toOSPath turns a remote relative name into an OS path in the cache
func (c *Cache) toOSPath(name string) string {
	return filepath.Join(c.root, filepath.FromSlash(name))
}

// toOSPathMeta turns a remote relative name into an OS path in the
// cache for the metadata
func (c *Cache) toOSPathMeta(name string) string {
	return filepath.Join(c.metaRoot, filepath.FromSlash(name))
}

// mkdir makes the directory for name in the cache and returns an os
// path for the file
func (c *Cache) mkdir(name string) (string, error) {
	parent := vfscommon.FindParent(name)
	leaf := filepath.Base(name)
	parentPath := c.toOSPath(parent)
	err := os.MkdirAll(parentPath, 0700)
	if err != nil {
		return "", errors.Wrap(err, "make cache directory failed")
	}
	parentPathMeta := c.toOSPathMeta(parent)
	err = os.MkdirAll(parentPathMeta, 0700)
	if err != nil {
		return "", errors.Wrap(err, "make cache meta directory failed")
	}
	return filepath.Join(parentPath, leaf), nil
}

// _get gets name from the cache or creates a new one
//
// It returns the item and found as to whether this item was found in
// the cache (or just created).
//
// name should be a remote path not an osPath
//
// must be called with itemMu held
func (c *Cache) _get(name string) (item *Item, found bool) {
	item = c.item[name]
	found = item != nil
	if !found {
		item = newItem(c, name)
		c.item[name] = item
	}
	return item, found
}

// put puts item under name in the cache
//
// It returns an old item if there was one or nil if not.
//
// name should be a remote path not an osPath
//
// must be called with itemMu held
func (c *Cache) put(name string, item *Item) (oldItem *Item) {
	name = clean(name)
	c.itemMu.Lock()
	oldItem = c.item[name]
	if oldItem != item {
		c.item[name] = item
	} else {
		oldItem = nil
	}
	c.itemMu.Unlock()
	return oldItem
}

// Opens returns the number of opens that are on the file
//
// name should be a remote path not an osPath
func (c *Cache) Opens(name string) int {
	name = clean(name)
	c.itemMu.Lock()
	defer c.itemMu.Unlock()
	item := c.item[name]
	if item == nil {
		return 0
	}
	item.mu.Lock()
	defer item.mu.Unlock()
	return item.opens
}

// get gets a file name from the cache or creates a new one
//
// It returns the item and found as to whether this item was found in
// the cache (or just created).
//
// name should be a remote path not an osPath
func (c *Cache) get(name string) (item *Item, found bool) {
	name = clean(name)
	c.itemMu.Lock()
	item, found = c._get(name)
	c.itemMu.Unlock()
	return item, found
}

// Item gets a cache item for name
//
// To use it item.Open will need to be called
//
// name should be a remote path not an osPath
func (c *Cache) Item(name string) (item *Item) {
	item, _ = c.get(name)
	return item
}

// Exists checks to see if the file exists in the cache or not
//
// FIXME check the metadata exists here too?
func (c *Cache) Exists(name string) bool {
	osPath := c.toOSPath(name)
	fi, err := os.Stat(osPath)
	if err != nil {
		return false
	}
	// checks for non-regular files (e.g. directories, symlinks, devices, etc.)
	if !fi.Mode().IsRegular() {
		return false
	}
	return true
}

// rename with os.Rename and more checking
func rename(osOldPath, osNewPath string) error {
	sfi, err := os.Stat(osOldPath)
	if err != nil {
		// Just do nothing if the source does not exist
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrapf(err, "Failed to stat source: %s", osOldPath)
	}
	if !sfi.Mode().IsRegular() {
		// cannot copy non-regular files (e.g., directories, symlinks, devices, etc.)
		return errors.Errorf("Non-regular source file: %s (%q)", sfi.Name(), sfi.Mode().String())
	}
	dfi, err := os.Stat(osNewPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return errors.Wrapf(err, "Failed to stat destination: %s", osNewPath)
		}
		parent := vfscommon.FindParent(osNewPath)
		err = os.MkdirAll(parent, 0700)
		if err != nil {
			return errors.Wrapf(err, "Failed to create parent dir: %s", parent)
		}
	} else {
		if !(dfi.Mode().IsRegular()) {
			return errors.Errorf("Non-regular destination file: %s (%q)", dfi.Name(), dfi.Mode().String())
		}
		if os.SameFile(sfi, dfi) {
			return nil
		}
	}
	if err = os.Rename(osOldPath, osNewPath); err != nil {
		return errors.Wrapf(err, "Failed to rename in cache: %s to %s", osOldPath, osNewPath)
	}
	return nil
}

// Rename the item in cache
func (c *Cache) Rename(name string, newName string, newObj fs.Object) (err error) {
	item, _ := c.get(name)
	err = item.rename(name, newName, newObj)
	if err != nil {
		return err
	}

	// Move the item in the cache
	c.itemMu.Lock()
	if item, ok := c.item[name]; ok {
		c.item[newName] = item
		delete(c.item, name)
	}
	c.itemMu.Unlock()

	fs.Infof(name, "Renamed in cache to %q", newName)
	return nil
}

// Remove should be called if name is deleted
func (c *Cache) Remove(name string) {
	item, _ := c.get(name)
	item.remove("file deleted")
}

// SetModTime should be called to set the modification time of the cache file
func (c *Cache) SetModTime(name string, modTime time.Time) {
	item, _ := c.get(name)
	item.setModTime(modTime)
}

// CleanUp empties the cache of everything
func (c *Cache) CleanUp() error {
	err1 := os.RemoveAll(c.root)
	err2 := os.RemoveAll(c.metaRoot)
	if err1 != nil {
		return err1
	}
	return err2
}

// walk walks the cache calling the function
func (c *Cache) walk(dir string, fn func(osPath string, fi os.FileInfo, name string) error) error {
	return filepath.Walk(dir, func(osPath string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Find path relative to the cache root
		name, err := filepath.Rel(dir, osPath)
		if err != nil {
			return errors.Wrap(err, "filepath.Rel failed in walk")
		}
		if name == "." {
			name = ""
		}
		// And convert into slashes
		name = filepath.ToSlash(name)

		return fn(osPath, fi, name)
	})
}

// reload walks the cache loading metadata files
func (c *Cache) reload() error {
	err := c.walk(c.root, func(osPath string, fi os.FileInfo, name string) error {
		if !fi.IsDir() {
			_, _ = c.get(name)
		}
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "failed to walk cache")
	}
	err = c.walk(c.root, func(osPathMeta string, fi os.FileInfo, name string) error {
		if !fi.IsDir() {
			_, _ = c.get(name)
		}
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "failed to walk meta cache")
	}
	return err
}

// purgeOld gets rid of any files that are over age
func (c *Cache) purgeOld(maxAge time.Duration) {
	c._purgeOld(maxAge, func(item *Item) {
		// Note item.mu is held here
		item._remove("too old")
	})
}

func (c *Cache) _purgeOld(maxAge time.Duration, remove func(item *Item)) {
	c.itemMu.Lock()
	defer c.itemMu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	for name, item := range c.item {
		item.mu.Lock()
		if item.opens == 0 {
			// If not locked and access time too long ago - delete the file
			dt := item.ATime.Sub(cutoff)
			// fs.Debugf(name, "atime=%v cutoff=%v, dt=%v", item.ATime, cutoff, dt)
			if dt < 0 {
				remove(item)
				// Remove the entry
				delete(c.item, name)
			}
		}
		item.mu.Unlock()
	}
}

// Purge any empty directories
func (c *Cache) purgeEmptyDirs() {
	ctx := context.Background()
	err := operations.Rmdirs(ctx, c.fcache, "", true)
	if err != nil {
		fs.Errorf(c.fcache, "Failed to remove empty directories from cache: %v", err)
	}
	err = operations.Rmdirs(ctx, c.fcacheMeta, "", true)
	if err != nil {
		fs.Errorf(c.fcache, "Failed to remove empty directories from metadata cache: %v", err)
	}
}

type cacheItems []*Item

func (v cacheItems) Len() int      { return len(v) }
func (v cacheItems) Swap(i, j int) { v[i], v[j] = v[j], v[i] }
func (v cacheItems) Less(i, j int) bool {
	if i == j {
		return false
	}
	iItem := v[i]
	jItem := v[j]
	iItem.mu.Lock()
	defer iItem.mu.Unlock()
	jItem.mu.Lock()
	defer jItem.mu.Unlock()

	return iItem.ATime.Before(jItem.ATime)
}

// Remove any files that are over quota starting from the
// oldest first
func (c *Cache) purgeOverQuota(quota int64) {
	c._purgeOverQuota(quota, func(item *Item) {
		// Note item.mu is held here
		item._remove("over quota")
	})
}

// updateUsed updates c.used so it is accurate
func (c *Cache) updateUsed() {
	c.itemMu.Lock()
	defer c.itemMu.Unlock()

	newUsed := int64(0)
	for _, item := range c.item {
		item.mu.Lock()
		newUsed += item.Size // FIXME make this size on disk
		item.mu.Unlock()

	}
	c.used = newUsed
}

func (c *Cache) _purgeOverQuota(quota int64, remove func(item *Item)) {
	c.updateUsed()

	c.itemMu.Lock()
	defer c.itemMu.Unlock()

	if quota <= 0 || c.used < quota {
		return
	}

	var items cacheItems

	// Make a slice of unused files
	for _, item := range c.item {
		item.mu.Lock()
		if item.opens == 0 {
			items = append(items, item)
		}
		item.mu.Unlock()
	}

	sort.Sort(items)

	// Remove items until the quota is OK
	for _, item := range items {
		if c.used < quota {
			break
		}
		item.mu.Lock()
		c.used -= item.Size // FIXME size on disk
		remove(item)
		// Remove the entry
		delete(c.item, item.name)
		item.mu.Unlock()
	}
}

// clean empties the cache of stuff if it can
func (c *Cache) clean() {
	// Cache may be empty so end
	_, err := os.Stat(c.root)
	if os.IsNotExist(err) {
		return
	}

	c.itemMu.Lock()
	oldItems, oldUsed := len(c.item), fs.SizeSuffix(c.used)
	c.itemMu.Unlock()

	// Remove any files that are over age
	c.purgeOld(c.opt.CacheMaxAge)

	// Now remove any files that are over quota starting from the
	// oldest first
	c.purgeOverQuota(int64(c.opt.CacheMaxSize))

	// Stats
	c.itemMu.Lock()
	newItems, newUsed := len(c.item), fs.SizeSuffix(c.used)
	c.itemMu.Unlock()

	fs.Infof(nil, "Cleaned the cache: objects %d (was %d), total size %v (was %v)", newItems, oldItems, newUsed, oldUsed)
}

// cleaner calls clean at regular intervals
//
// doesn't return until context is cancelled
func (c *Cache) cleaner(ctx context.Context) {
	if c.opt.CachePollInterval <= 0 {
		fs.Debugf(nil, "Cache cleaning thread disabled because poll interval <= 0")
		return
	}
	// Start cleaning the cache immediately
	c.clean()
	// Then every interval specified
	timer := time.NewTicker(c.opt.CachePollInterval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			c.clean()
		case <-ctx.Done():
			fs.Debugf(nil, "cache cleaner exiting")
			return
		}
	}
}

// copy an object to or from the remote while accounting for it
func copyObj(f fs.Fs, dst fs.Object, remote string, src fs.Object) (newDst fs.Object, err error) {
	if operations.NeedTransfer(context.TODO(), dst, src) {
		newDst, err = operations.Copy(context.TODO(), f, dst, remote, src)
	} else {
		newDst = dst
	}
	return newDst, err
}

// Check the local file is up to date in the cache
func (c *Cache) Check(ctx context.Context, o fs.Object, remote string) (err error) {
	defer log.Trace(o, "remote=%q", remote)("err=%v", &err)
	item, _ := c.get(remote)
	item.checkObject(o)
	err = item.truncateToCurrentSize()
	if err != nil {
		return errors.Wrap(err, "Check truncate failed")
	}
	return nil
}

/*
// Fetch fetches the object to the cache file starting with offset
func (c *Cache) Fetch(ctx context.Context, o fs.Object, remote string) (err error) {
	defer log.Trace(o, "remote=%q", remote)("err=%v", &err)
	item, _ := c.get(remote)

	// Get the whole object
	// if false {
	// 	o, err := copyObj(c.fcache, nil, remote, o)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	c.itemMu.Lock()
	// 	item.Size = o.Size()
	// 	item.Rs.Insert(ranges.Range{Pos: 0, Size: item.Size}) // FIXME
	// 	c.itemMu.Unlock()
	// }

	// if cached item is present and up to date then carry on
	if item.Present() {
		fs.Debugf(o, "already have present")
		return nil
		// cacheObj, err := c.fcache.NewObject(ctx, remote)
		// if err == nil && !operations.NeedTransfer(context.TODO(), cacheObj, o) {
		// 	fs.Debugf(o, "already have present")
		// 	return nil
		// }
	}

	// start the downloader if not started
	return item.newDownloader()
}
*/
