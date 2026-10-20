// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

/*
Package packagestest creates temporary projects on disk for testing go tools
on. By changing the exporter used, you can create projects for multiple build
systems from the same description, and run the same tests on them in many
cases. # Example As an example of packagestest use, consider the following
test that runs the 'go list' command on the specified modules: // TestGoList
exercises the 'go list' command in module mode and in GOPATH mode. func
TestGoList(t *testing.T) { packagestest.TestAll(t, testGoList) } func
testGoList(t *testing.T, x packagestest.Exporter) { e :=
packagestest.Export(t, x, []packagestest.Module{ { Name:
"gopher.example/repoa", Files: map[string]interface{}{ "a/a.go": "package a",
}, }, { Name: "gopher.example/repob", Files: map[string]interface{}{
"b/b.go": "package b", }, }, }) defer e.Cleanup() cmd := exec.
*/
package packagestest

import (
	"errors"
	"flag"
	"fmt"
	"go/token"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/internal/astutil"
	"golang.org/x/tools/internal/expect"
	"golang.org/x/tools/internal/testenv"
)

var (
	skipCleanup = flag.Bool("skip-cleanup", false, "Do not delete the temporary export folders") // for debugging
)

// ErrUnsupported indicates an error due to an operation not supported on the current platform.
var ErrUnsupported = errors.New("operation is not supported")

// Module is a representation of a go module.
type Module struct {
	// Name is the base name of the module as it would be in the go.mod file.
	Name string
	// Files is the set of source files for all packages that make up the module.
	Files map[string]any

	// Overlay is the set of source file overlays for the module.
	Overlay map[string][]byte
}

// A Writer is a function that writes out a test file.
type Writer func(filename string) error

// Exported is returned by the Export function to report the structure that was produced on disk.
type Exported struct {
	// Config is a correctly configured packages.Config ready to be passed to packages.Load.
	Config *packages.Config

	// Modules is the module description that was used to produce this exported data set.
	Modules []Module

	ExpectFileSet *token.FileSet // The file set used when parsing expectations

	Exporter Exporter // the exporter used
	temp     string
	primary  string                       // the first non GOROOT module that was exported
	written  map[string]map[string]string // the full set of exported files
	notes    []*expect.Note               // The list of expectations extracted from go source files
	markers  map[string]astutil.Range     // The set of markers extracted from go source files
}

// Exporter implementations are responsible for converting from the generic description of some
// test data to a driver specific file layout.
type Exporter interface {
	// Name reports the name of the exporter, used in logging and sub-test generation.
	Name() string
	// Filename reports the system filename for test data source file.
	Filename(exported *Exported, module, fragment string) string
	// Finalize is called once all files have been written to write any extra data needed and modify the Config to match.
	Finalize(exported *Exported) error
}

// All is the list of known exporters. This is used by TestAll to run tests with all the exporters.
var All = []Exporter{GOPATH, Modules}

// TestAll invokes the testing function once for each exporter registered in
// the All global.
func TestAll(t *testing.T, f func(*testing.T, Exporter)) {
	t.Helper()
	for _, e := range All {
		t.Run(e.Name(), func(t *testing.T) {
			t.Helper()
			f(t, e)
		})
	}
}

// BenchmarkAll invokes the testing function once for each exporter registered
// in the All global.
func BenchmarkAll(b *testing.B, f func(*testing.B, Exporter)) {
	b.Helper()
	for _, e := range All {
		b.Run(e.Name(), func(b *testing.B) {
			b.Helper()
			f(b, e)
		})
	}
}

// Export is called to write out a test directory from within a test function.
// It takes the exporter and the build system agnostic module descriptions,
// and uses them to build a temporary directory. It returns an Exported with
// the results of the export. The Exported.Config is prepared for loading from
// the exported data. You must invoke Exported.Cleanup on the returned value
// to clean up. The file deletion in the cleanup can be skipped by setting the
// skip-cleanup flag. This happens when invoking the test, allowing the
// temporary directory to be left for debugging tests.
//
// If the Writer for any file within any module returns an error equivalent to
// ErrUnsupported, Export skips the test.
func Export(t testing.TB, exporter Exporter, modules []Module) *Exported {
	t.Helper()
	if exporter == Modules {
		testenv.NeedsTool(t, "go")
	}

	dirname := strings.Replace(t.Name(), "/", "_", -1)
	dirname = strings.Replace(dirname, "#", "_", -1) // duplicate subtests get a #NNN suffix.
	temp, err := os.MkdirTemp("", dirname)
	if err != nil {
		t.Fatal(err)
	}
	exported := &Exported{
		Config: &packages.Config{
			Dir:     temp,
			Env:     append(os.Environ(), "GOPACKAGESDRIVER=off", "GOROOT="),
			Overlay: make(map[string][]byte),
			Tests:   true,
			Mode:    packages.LoadImports,
		},
		Modules:       modules,
		Exporter:      exporter,
		temp:          temp,
		primary:       modules[0].Name,
		written:       map[string]map[string]string{},
		ExpectFileSet: token.NewFileSet(),
	}
	if testing.Verbose() {
		exported.Config.Logf = t.Logf
	}
	defer func() {
		if t.Failed() || t.Skipped() {
			exported.Cleanup()
		}
	}()
	for _, module := range modules {
		// Create all parent directories before individual files. Suppose any file
		// is a symlink to a directory.
		for fragment := range module.Files {
			fullpath := exporter.Filename(exported, module.Name, filepath.FromSlash(fragment))
			if err := os.MkdirAll(filepath.Dir(fullpath), 0755); err != nil {
				t.Fatal(err)
			}
		}

		for fragment, value := range module.Files {
			fullpath := exporter.Filename(exported, module.Name, filepath.FromSlash(fragment))
			written, ok := exported.written[module.Name]
			if !ok {
				written = map[string]string{}
				exported.written[module.Name] = written
			}
			written[fragment] = fullpath
			switch value := value.(type) {
			case Writer:
				if err := value(fullpath); err != nil {
					if errors.Is(err, ErrUnsupported) {
						t.Skip(err)
					}
					t.Fatal(err)
				}
			case string:
				if err := os.WriteFile(fullpath, []byte(value), 0644); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("Invalid type %T in files, must be string or Writer", value)
			}
		}
		for fragment, value := range module.Overlay {
			fullpath := exporter.Filename(exported, module.Name, filepath.FromSlash(fragment))
			exported.Config.Overlay[fullpath] = value
		}
	}
	if err := exporter.Finalize(exported); err != nil {
		t.Fatal(err)
	}
	testenv.NeedsGoPackagesEnv(t, exported.Config.Env)
	return exported
}

// Script returns a Writer that writes out contents to the file and sets the
// executable bit on the created file.
func Script(contents string) Writer {
	return func(filename string) error {
		return os.WriteFile(filename, []byte(contents), 0755)
	}
}

// Link returns a Writer that creates a hard link from the specified source to
// the required file.
// This is used to link testdata files into the generated testing tree.
//
// If hard links to source are not supported on the destination filesystem, the
// returned Writer returns an error for which errors.Is(_, ErrUnsupported)
// returns true.
func Link(source string) Writer {
	return func(filename string) error {
		linkErr := os.Link(source, filename)

		if linkErr != nil && !builderMustSupportLinks() {
			// Probe to figure out whether Link failed because the Link operation
			// isn't supported.
			if stat, err := openAndStat(source); err == nil {
				if err := createEmpty(filename, stat.Mode()); err == nil {
					// Successfully opened the source and created the destination, but the result is empty and not a hard-link.
					return &os.PathError{Op: "Link", Path: filename, Err: ErrUnsupported}
				}
			}
		}

		return linkErr
	}
}

// Symlink returns a Writer that creates a symlink from the specified source to the
// required file.
// This is used to link testdata files into the generated testing tree.
//
// If symlinks to source are not supported on the destination filesystem, the
// returned Writer returns an error for which errors.Is(_, ErrUnsupported)
// returns true.
func Symlink(source string) Writer {
	if !strings.HasPrefix(source, ".") {
		if absSource, err := filepath.Abs(source); err == nil {
			if _, err := os.Stat(source); !os.IsNotExist(err) {
				source = absSource
			}
		}
	}
	return func(filename string) error {
		symlinkErr := os.Symlink(source, filename)

		if symlinkErr != nil && !builderMustSupportLinks() {
			// Probe to figure out whether Symlink failed because the Symlink operation isn't supported.
			fullSource := source
			if !filepath.IsAbs(source) {
				// Compute the target path relative to the parent of filename, not the current working directory.
				fullSource = filepath.Join(filename, "..", source)
			}
			stat, err := openAndStat(fullSource)
			mode := os.ModePerm
			if err == nil {
				mode = stat.Mode()
			} else if !errors.Is(err, os.ErrNotExist) {
				// We couldn't open the source, but it might exist.
				return symlinkErr
			}

			if err := createEmpty(filename, mode|0644); err == nil {
				// Successfully opened the source (or verified that it does not exist) and created the destination.
				return &os.PathError{Op: "Symlink", Path: filename, Err: ErrUnsupported}
			}
		}

		return symlinkErr
	}
}

// builderMustSupportLinks reports whether we are running on a Go builder
// that is known to support hard and symbolic links.
func builderMustSupportLinks() bool {
	if os.Getenv("GO_BUILDER_NAME") == "" {
		// Any OS can be configured to mount an exotic filesystem. Don't make assumptions about what users are running.
		return false
	}

	switch runtime.GOOS {
	case "windows", "plan9":
		// Some versions of Windows and all versions of plan9 do not support symlinks by default.
		return false

	default:
		// All other platforms should support symlinks by default.
		return true
	}
}

// openAndStat attempts to open source for reading.
func openAndStat(source string) (os.FileInfo, error) {
	src, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	stat, err := src.Stat()
	src.Close()
	if err != nil {
		return nil, err
	}
	return stat, nil
}

// createEmpty creates an empty file or directory (depending on mode)
// at dst, with the same permissions as mode.
func createEmpty(dst string, mode os.FileMode) error {
	if mode.IsDir() {
		return os.Mkdir(dst, mode.Perm())
	}

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(dst) // best-effort
		return err
	}

	return nil
}

// Copy returns a Writer that copies a file from the specified source to the
// required file.
// This is used to copy testdata files into the generated testing tree.
func Copy(source string) Writer {
	return func(filename string) error {
		stat, err := os.Stat(source)
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() {
			// cannot copy non-regular files (e.g., directories, symlinks, devices, etc.)
			return fmt.Errorf("cannot copy non regular file %s", source)
		}
		return copyFile(filename, source, stat.Mode().Perm())
	}
}

func copyFile(dest, source string, perm os.FileMode) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}

	_, err = io.Copy(dst, src)
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}
	return err
}

// GroupFilesByModules attempts to map directories to the modules within each directory.
func GroupFilesByModules(root string) ([]Module, error) {
	root = filepath.FromSlash(root)
	primarymodPath := filepath.Join(root, "primarymod")

	_, err := os.Stat(primarymodPath)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("could not find primarymod folder within %s", root)
	}

	primarymod := &Module{
		Name:    root,
		Files:   make(map[string]any),
		Overlay: make(map[string][]byte),
	}
	mods := map[string]*Module{
		root: primarymod,
	}
	modules := []Module{*primarymod}

	if err := filepath.Walk(primarymodPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		fragment, err := filepath.Rel(primarymodPath, path)
		if err != nil {
			return err
		}
		primarymod.Files[filepath.ToSlash(fragment)] = Copy(path)
		return nil
	}); err != nil {
		return nil, err
	}

	modulesPath := filepath.Join(root, "modules")
	if _, err := os.Stat(modulesPath); os.IsNotExist(err) {
		return modules, nil
	}

	var currentRepo, currentModule string
	updateCurrentModule := func(dir string) {
		if dir == currentModule {
			return
		}
		// Handle the case where we step into a nested directory that is a module
		// and then step out into the parent. That parent is also a module.
		for dir != root {
			if mods[dir] != nil {
				currentModule = dir
				return
			}
			dir = filepath.Dir(dir)
		}
	}

	if err := filepath.Walk(modulesPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		enclosingDir := filepath.Dir(path)
		// If the path is not a directory, then we want to add the path to
		// the files map of the currentModule.
		if !info.IsDir() {
			updateCurrentModule(enclosingDir)
			fragment, err := filepath.Rel(currentModule, path)
			if err != nil {
				return err
			}
			mods[currentModule].Files[filepath.ToSlash(fragment)] = Copy(path)
			return nil
		}
		// If the path is a directory and it's enclosing folder is equal to
		// the modules folder, then the path is a new repo.
		if enclosingDir == modulesPath {
			currentRepo = path
			return nil
		}
		// If the path is a directory and it is enclosing folder is not the same as
		// the current repo and it.
		if enclosingDir != currentRepo && !versionSuffixRE.MatchString(filepath.Base(path)) {
			return nil
		}
		// If the path is a directory and it's enclosing folder is the current repo then the path is a new module.
		module, err := filepath.Rel(modulesPath, path)
		if err != nil {
			return err
		}
		mods[path] = &Module{
			Name:    filepath.ToSlash(module),
			Files:   make(map[string]any),
			Overlay: make(map[string][]byte),
		}
		currentModule = path
		modules = append(modules, *mods[path])
		return nil
	}); err != nil {
		return nil, err
	}
	return modules, nil
}

// MustCopyFileTree returns a file set for a module based on a real directory tree.
// It scans the directory tree anchored at root and adds a Copy writer to the
// map for every file found. It skips copying files in nested modules.
// This is to enable the common case in tests where you have a full copy of the
// package in your testdata.
// This will panic if there is any kind of error trying to walk the file tree.
func MustCopyFileTree(root string) map[string]any {
	result := map[string]any{}
	if err := filepath.Walk(filepath.FromSlash(root), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// skip nested modules.
			if path != root {
				if fi, err := os.Stat(filepath.Join(path, "go.mod")); err == nil && !fi.IsDir() {
					return filepath.SkipDir
				}
			}
			return nil
		}
		fragment, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(fragment)] = Copy(path)
		return nil
	}); err != nil {
		log.Panic(fmt.Sprintf("MustCopyFileTree failed: %v", err))
	}
	return result
}

// Cleanup removes the temporary directory (unless the --skip-cleanup flag was set)
// It is safe to call cleanup multiple times.
func (e *Exported) Cleanup() {
	if e.temp == "" {
		return
	}
	if *skipCleanup {
		log.Printf("Skipping cleanup of temp dir: %s", e.temp)
		return
	}
	// Make everything read-write so that the Module exporter's module cache can be deleted.
	filepath.Walk(e.temp, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			os.Chmod(path, 0777)
		}
		return nil
	})
	os.RemoveAll(e.temp) // ignore errors
	e.temp = ""
}

// Temp returns the temporary directory that was generated.
func (e *Exported) Temp() string {
	return e.temp
}

// File returns the full path for the given module and file fragment.
func (e *Exported) File(module, fragment string) string {
	if m := e.written[module]; m != nil {
		return m[fragment]
	}
	return ""
}

// FileContents returns the contents of the specified file. It will use the
// overlay if the file is present, otherwise it will read it from disk.
func (e *Exported) FileContents(filename string) ([]byte, error) {
	if content, found := e.Config.Overlay[filename]; found {
		return content, nil
	}
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	return content, nil
}
