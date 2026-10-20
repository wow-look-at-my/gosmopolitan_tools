// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package modindex

import (
	"bufio"
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

/* The on-disk index ("payload") is a text file. */

// CurrentVersion tells readers about the format of the index.
const CurrentVersion int = 0

// Index is returned by [Read].
type Index struct {
	Version    int
	GOMODCACHE string    // absolute path of Go module cache dir
	ValidAt    time.Time // moment at which the index was up to date
	Entries    []Entry
}

func (ix *Index) String() string {
	return fmt.Sprintf("Index(%s v%d has %d entries at %v)",
		ix.GOMODCACHE, ix.Version, len(ix.Entries), ix.ValidAt)
}

// An Entry contains information for an import path.
type Entry struct {
	Dir        string // package directory relative to GOMODCACHE; uses OS path separator
	ImportPath string
	PkgName    string
	Version    string
	Names      []string // exported names and information
}

// IndexDir is where the module index is stored.
var IndexDir string = func() string {
	var dir string
	if testing.Testing() {
		dir = os.TempDir()
	} else {
		var err error
		dir, err = os.UserCacheDir()
		// shouldn't happen, but TempDir is better than creating ./goimports
		if err != nil {
			dir = os.TempDir()
		}
	}
	dir = filepath.Join(dir, "goimports")
	if err := os.MkdirAll(dir, 0777); err != nil {
		dir = "" // #people complain about the error message
	}
	return dir
}()

// cache memoizes the most recently read index for each Go module cache directory; see [Read].
var cache sync.Map // from gomodcache (string) to *cacheEntry

type cacheEntry struct {
	payloadFile string
	index       *Index
}

// parseCount counts the number of times [Read] has parsed a payload file, as opposed to returning a memoized index.
var parseCount atomic.Int64

// Read reads the latest version of the on-disk index for the specified Go
// module cache directory. If there is no index, it returns a nil Index and an
// fs.ErrNotExist error.
func Read(gomodcache string) (*Index, error) {
	gomodcache, err := filepath.Abs(gomodcache)
	if err != nil {
		return nil, err
	}
	if IndexDir == "" {
		return nil, os.ErrNotExist
	}

	// Read the "link" file for the specified gomodcache directory. It names the payload file.
	content, err := os.ReadFile(filepath.Join(IndexDir, linkFileBasename(gomodcache)))
	if err != nil {
		return nil, err
	}
	payloadFile := filepath.Join(IndexDir, string(content))

	// Is the index from that payload file already in memory?
	if v, ok := cache.Load(gomodcache); ok {
		if e := v.(*cacheEntry); e.payloadFile == payloadFile {
			return e.index, nil
		}
	}

	// Read the index out of the payload file.
	f, err := os.Open(payloadFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	parseCount.Add(1)
	ix, err := readIndexFrom(gomodcache, bufio.NewReader(f))
	if err != nil {
		return nil, err
	}
	cache.Store(gomodcache, &cacheEntry{payloadFile, ix})
	return ix, nil
}

func readIndexFrom(gomodcache string, r io.Reader) (*Index, error) {
	scan := bufio.NewScanner(r)

	// version
	if !scan.Scan() {
		return nil, fmt.Errorf("unexpected scan error: %v", scan.Err())
	}
	version, err := strconv.Atoi(scan.Text())
	if err != nil {
		return nil, err
	}
	if version != CurrentVersion {
		return nil, fmt.Errorf("got version %d, expected %d", version, CurrentVersion)
	}

	// gomodcache
	if !scan.Scan() {
		return nil, fmt.Errorf("scanner error reading module cache dir: %v", scan.Err())
	}
	// TODO(pjw): need to check that this is the expected cache dir
	// so the tag should be passed in to this function
	if dir := string(scan.Text()); dir != gomodcache {
		return nil, fmt.Errorf("index file GOMODCACHE mismatch: got %q, want %q", dir, gomodcache)
	}

	// changed
	if !scan.Scan() {
		return nil, fmt.Errorf("scanner error reading index creation time: %v", scan.Err())
	}
	changed, err := time.ParseInLocation(time.DateTime, scan.Text(), time.Local)
	if err != nil {
		return nil, err
	}

	// entries
	var (
		curEntry *Entry
		entries  []Entry
	)
	for scan.Scan() {
		v := scan.Text()
		if len(v) < 2 {
			return nil, fmt.Errorf("malformed line: %q, %d entries", v, len(entries))
		}
		if v[0] == ':' {
			if curEntry != nil {
				entries = append(entries, *curEntry)
			}
			// as directories may contain commas and quotes, they need to be read as csv.
			rdr := strings.NewReader(v[1:])
			cs := csv.NewReader(rdr)
			flds, err := cs.Read()
			if err != nil {
				return nil, err
			}
			if len(flds) != 4 {
				return nil, fmt.Errorf("header contains %d fields, not 4: %q", len(v), v)
			}
			curEntry = &Entry{
				PkgName:    flds[0],
				ImportPath: flds[1],
				Dir:        relative(gomodcache, flds[2]),
				Version:    flds[3],
			}
			continue
		}
		curEntry.Names = append(curEntry.Names, v)
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("scanner failed while reading modindex entry: %v", err)
	}
	if curEntry != nil {
		entries = append(entries, *curEntry)
	}

	return &Index{
		Version:    version,
		GOMODCACHE: gomodcache,
		ValidAt:    changed,
		Entries:    entries,
	}, nil
}

// write writes the index file and updates the index directory to refer to it.
func write(gomodcache string, ix *Index) error {
	if IndexDir == "" {
		return os.ErrNotExist
	}
	// Write the index into a payload file with a fresh name.
	f, err := os.CreateTemp(IndexDir, fmt.Sprintf("index-%d-*", CurrentVersion))
	if err != nil {
		return err // e.g. disk full, or index dir deleted
	}
	if err := writeIndexToFile(ix, bufio.NewWriter(f)); err != nil {
		_ = f.Close() // ignore error
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// Write the name of the payload file into a link file.
	indexDirFile := filepath.Join(IndexDir, linkFileBasename(gomodcache))
	content := []byte(filepath.Base(f.Name()))
	return os.WriteFile(indexDirFile, content, 0666)
}

func writeIndexToFile(x *Index, w *bufio.Writer) error {
	fmt.Fprintf(w, "%d\n", x.Version)
	fmt.Fprintf(w, "%s\n", x.GOMODCACHE)
	tm := x.ValidAt.Truncate(time.Second) // round the time down
	fmt.Fprintf(w, "%s\n", tm.Format(time.DateTime))
	for _, e := range x.Entries {
		if e.ImportPath == "" {
			continue // shouldn't happen
		}
		// PJW: maybe always write these headers as csv?
		if strings.ContainsAny(string(e.Dir), ",\"") {
			cw := csv.NewWriter(w)
			cw.Write([]string{":" + e.PkgName, e.ImportPath, string(e.Dir), e.Version})
			cw.Flush()
		} else {
			fmt.Fprintf(w, ":%s,%s,%s,%s\n", e.PkgName, e.ImportPath, e.Dir, e.Version)
		}
		for _, x := range e.Names {
			fmt.Fprintf(w, "%s\n", x)
		}
	}
	return w.Flush()
}

// linkFileBasename returns the base name of the link file in the index
// directory.
func linkFileBasename(gomodcache string) string {
	// Note: coupled to logic in ./gomodindex/cmd.go. TODO: factor.
	h := sha256.Sum256([]byte(gomodcache)) // collision-resistant hash
	return fmt.Sprintf("index-name-%d-%032x", CurrentVersion, h)
}

func relative(base, file string) string {
	if rel, err := filepath.Rel(base, file); err == nil {
		return rel
	}
	return file
}
