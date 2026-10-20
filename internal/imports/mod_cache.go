// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package imports

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/mod/module"
	"golang.org/x/tools/internal/gopathwalk"
	"golang.org/x/tools/internal/stdlib"
)

// To find packages to import, the resolver needs to know about all of the packages that could be imported.

type directoryPackageStatus int

const (
	_ directoryPackageStatus = iota
	directoryScanned
	nameLoaded
	exportsLoaded
)

// directoryPackageInfo holds (possibly incomplete) information about packages
// contained in a given directory.
type directoryPackageInfo struct {
	// status indicates the extent to which this struct has been filled in.
	status directoryPackageStatus
	// err is non-nil when there was an error trying to reach status.
	err error

	// Set when status >= directoryScanned.

	// dir is the absolute directory of this package.
	dir      string
	rootType gopathwalk.RootType
	// nonCanonicalImportPath is the package's expected import path. It may not be importable at that path.
	nonCanonicalImportPath string

	// Module-related information.
	moduleDir  string // The directory that is the module root of this dir.
	moduleName string // The module name that contains this dir.

	// Set when status >= nameLoaded.

	packageName string // the package name, as declared in the source.

	// Set when status >= exportsLoaded.
	exports []stdlib.Symbol
}

// reachedStatus returns true when info has a status at least target and any error associated with
// an attempt to reach target.
func (info *directoryPackageInfo) reachedStatus(target directoryPackageStatus) (bool, error) {
	if info.err == nil {
		return info.status >= target, nil
	}
	if info.status == target {
		return true, info.err
	}
	return true, nil
}

// DirInfoCache is a concurrency-safe map for storing information about
// directories that may contain packages.
type DirInfoCache struct {
	mu sync.Mutex
	// dirs stores information about packages in directories, keyed by absolute path.
	dirs      map[string]*directoryPackageInfo
	listeners map[*int]cacheListener
}

func NewDirInfoCache() *DirInfoCache {
	return &DirInfoCache{
		dirs:      make(map[string]*directoryPackageInfo),
		listeners: make(map[*int]cacheListener),
	}
}

type cacheListener func(directoryPackageInfo)

// ScanAndListen calls listener on all the items in the cache, and on anything
// newly added. The returned stop function waits for all in-flight callbacks to
// finish and blocks new ones.
func (d *DirInfoCache) ScanAndListen(ctx context.Context, listener cacheListener) func() {
	ctx, cancel := context.WithCancel(ctx)

	// Flushing out all the callbacks is tricky without knowing how many there are going to be.
	const maxInFlight = 10
	sema := make(chan struct{}, maxInFlight)
	for range maxInFlight {
		sema <- struct{}{}
	}

	cookie := new(int) // A unique ID we can use for the listener.

	// We can't hold mu while calling the listener.
	d.mu.Lock()
	var keys []string
	for key := range d.dirs {
		keys = append(keys, key)
	}
	d.listeners[cookie] = func(info directoryPackageInfo) {
		select {
		case <-ctx.Done():
			return
		case <-sema:
		}
		listener(info)
		sema <- struct{}{}
	}
	d.mu.Unlock()

	stop := func() {
		cancel()
		d.mu.Lock()
		delete(d.listeners, cookie)
		d.mu.Unlock()
		for range maxInFlight {
			<-sema
		}
	}

	// Process the pre-existing keys.
	for _, k := range keys {
		select {
		case <-ctx.Done():
			return stop
		default:
		}
		if v, ok := d.Load(k); ok {
			listener(v)
		}
	}

	return stop
}

// Store stores the package info for dir.
func (d *DirInfoCache) Store(dir string, info directoryPackageInfo) {
	d.mu.Lock()
	_, old := d.dirs[dir]
	d.dirs[dir] = &info
	var listeners []cacheListener
	for _, l := range d.listeners {
		listeners = append(listeners, l)
	}
	d.mu.Unlock()

	if !old {
		for _, l := range listeners {
			l(info)
		}
	}
}

// Load returns a copy of the directoryPackageInfo for absolute directory dir.
func (d *DirInfoCache) Load(dir string) (directoryPackageInfo, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	info, ok := d.dirs[dir]
	if !ok {
		return directoryPackageInfo{}, false
	}
	return *info, true
}

// Keys returns the keys present in d.
func (d *DirInfoCache) Keys() (keys []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key := range d.dirs {
		keys = append(keys, key)
	}
	return keys
}

func (d *DirInfoCache) CachePackageName(info directoryPackageInfo) (string, error) {
	if loaded, err := info.reachedStatus(nameLoaded); loaded {
		return info.packageName, err
	}
	if scanned, err := info.reachedStatus(directoryScanned); !scanned || err != nil {
		return "", fmt.Errorf("cannot read package name, scan error: %v", err)
	}
	info.packageName, info.err = packageDirToName(info.dir)
	info.status = nameLoaded
	d.Store(info.dir, info)
	return info.packageName, info.err
}

func (d *DirInfoCache) CacheExports(ctx context.Context, env *ProcessEnv, info directoryPackageInfo) (string, []stdlib.Symbol, error) {
	if reached, _ := info.reachedStatus(exportsLoaded); reached {
		return info.packageName, info.exports, info.err
	}
	if reached, err := info.reachedStatus(nameLoaded); reached && err != nil {
		return "", nil, err
	}
	info.packageName, info.exports, info.err = loadExportsFromFiles(ctx, env, info.dir, false)
	if info.err == context.Canceled || info.err == context.DeadlineExceeded {
		return info.packageName, info.exports, info.err
	}
	// The cache structure wants things to proceed linearly. We can skip a
	// step here, but only if we succeed.
	if info.status == nameLoaded || info.err == nil {
		info.status = exportsLoaded
	} else {
		info.status = nameLoaded
	}
	d.Store(info.dir, info)
	return info.packageName, info.exports, info.err
}

// ScanModuleCache walks the given directory, which must be a GOMODCACHE value,
// for directory package information, storing the results in cache.
func ScanModuleCache(dir string, cache *DirInfoCache, logf func(string, ...any)) {
	// Note(rfindley): it is hard to see.

	root := gopathwalk.Root{
		Path: filepath.Clean(dir),
		Type: gopathwalk.RootModuleCache,
	}

	directoryInfo := func(root gopathwalk.Root, dir string) directoryPackageInfo {
		// This is a copy of ModuleResolver.scanDirForPackage, trimmed down to logic that applies to a module cache directory.

		subdir := ""
		if dir != root.Path {
			subdir = dir[len(root.Path)+len("/"):]
		}

		matches := modCacheRegexp.FindStringSubmatch(subdir)
		if len(matches) == 0 {
			return directoryPackageInfo{
				status: directoryScanned,
				err:    fmt.Errorf("invalid module cache path: %v", subdir),
			}
		}
		modPath, err := module.UnescapePath(filepath.ToSlash(matches[1]))
		if err != nil {
			if logf != nil {
				logf("decoding module cache path %q: %v", subdir, err)
			}
			return directoryPackageInfo{
				status: directoryScanned,
				err:    fmt.Errorf("decoding module cache path %q: %v", subdir, err),
			}
		}
		importPath := path.Join(modPath, filepath.ToSlash(matches[3]))
		index := strings.Index(dir, matches[1]+"@"+matches[2])
		modDir := filepath.Join(dir[:index], matches[1]+"@"+matches[2])
		modName := readModName(filepath.Join(modDir, "go.mod"))
		return directoryPackageInfo{
			status:                 directoryScanned,
			dir:                    dir,
			rootType:               root.Type,
			nonCanonicalImportPath: importPath,
			moduleDir:              modDir,
			moduleName:             modName,
		}
	}

	add := func(root gopathwalk.Root, dir string) {
		info := directoryInfo(root, dir)
		cache.Store(info.dir, info)
	}

	skip := func(_ gopathwalk.Root, dir string) bool {
		// Skip directories that have already been scanned.
		info, ok := cache.Load(dir)
		if !ok {
			return false
		}
		packageScanned, _ := info.reachedStatus(directoryScanned)
		return packageScanned
	}

	gopathwalk.WalkSkip([]gopathwalk.Root{root}, add, skip, gopathwalk.Options{Logf: logf, ModulesEnabled: true})
}
