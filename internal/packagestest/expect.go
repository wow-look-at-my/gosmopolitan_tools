// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package packagestest

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/internal/astutil"
	"golang.org/x/tools/internal/expect"
)

const (
	markMethod    = "mark"
	eofIdentifier = "EOF"
)

// Expect invokes the supplied methods for all expectation notes found in the
// exported source files.
//
// All exported go source files are parsed to collect the expectation notes.
// See the documentation for expect.Parse for how the notes are collected and
// parsed.
//
// The methods are supplied as a map of name to function, and those functions
// will be matched against the expectations by name. Notes with no matching
// function will be skipped, and functions with no matching notes will not be
// invoked. If there are no registered markers yet, a special pass will be run
// first which adds any markers declared with @mark(Name, pattern) or @name.
// These call the Mark method to add the marker to the global set. You can
// register the "mark" method to override these in your own call to Expect.
func (e *Exported) Expect(methods map[string]any) error {
	if err := e.getNotes(); err != nil {
		return err
	}
	if err := e.getMarkers(); err != nil {
		return err
	}
	var err error
	ms := make(map[string]method, len(methods))
	for name, f := range methods {
		mi := method{f: reflect.ValueOf(f)}
		mi.converters = make([]converter, mi.f.Type().NumIn())
		for i := 0; i < len(mi.converters); i++ {
			mi.converters[i], err = e.buildConverter(mi.f.Type().In(i))
			if err != nil {
				return fmt.Errorf("invalid method %v: %v", name, err)
			}
		}
		ms[name] = mi
	}
	for _, n := range e.notes {
		if n.Args == nil {
			// simple identifier form, convert to a call to mark
			n = &expect.Note{
				Pos:  n.Pos,
				Name: markMethod,
				Args: []any{n.Name, n.Name},
			}
		}
		mi, ok := ms[n.Name]
		if !ok {
			continue
		}
		params := make([]reflect.Value, len(mi.converters))
		args := n.Args
		for i, convert := range mi.converters {
			params[i], args, err = convert(n, args)
			if err != nil {
				return fmt.Errorf("%v: %v", e.ExpectFileSet.Position(n.Pos), err)
			}
		}
		if len(args) > 0 {
			return fmt.Errorf("%v: unwanted args got %+v extra", e.ExpectFileSet.Position(n.Pos), args)
		}
		//TODO: catch the error returned from the method
		mi.f.Call(params)
	}
	return nil
}

// Mark adds a new marker to the known set.
func (e *Exported) Mark(name string, r astutil.Range) {
	if e.markers == nil {
		e.markers = make(map[string]astutil.Range)
	}
	e.markers[name] = r
}

func (e *Exported) getNotes() error {
	if e.notes != nil {
		return nil
	}
	notes := []*expect.Note{}
	var dirs []string
	for _, module := range e.written {
		for _, filename := range module {
			dirs = append(dirs, filepath.Dir(filename))
		}
	}
	for filename := range e.Config.Overlay {
		dirs = append(dirs, filepath.Dir(filename))
	}
	pkgs, err := packages.Load(e.Config, dirs...)
	if err != nil {
		return fmt.Errorf("unable to load packages for directories %s: %v", dirs, err)
	}
	seen := make(map[token.Position]struct{})
	for _, pkg := range pkgs {
		for _, filename := range pkg.GoFiles {
			content, err := e.FileContents(filename)
			if err != nil {
				return err
			}
			l, err := expect.Parse(e.ExpectFileSet, filename, content)
			if err != nil {
				return fmt.Errorf("failed to extract expectations: %v", err)
			}
			for _, note := range l {
				pos := e.ExpectFileSet.Position(note.Pos)
				if _, ok := seen[pos]; ok {
					continue
				}
				notes = append(notes, note)
				seen[pos] = struct{}{}
			}
		}
	}
	if _, ok := e.written[e.primary]; !ok {
		e.notes = notes
		return nil
	}
	// Check go.mod markers regardless of mode. We need to do this so that our marker count
	// matches the counts in the summary.txt.golden file for the test directory.
	if gomod, found := e.written[e.primary]["go.mod"]; found {
		// If we are in Modules mode, then we need to check the contents of the go.mod.temp.
		if e.Exporter == Modules {
			gomod += ".temp"
		}
		l, err := goModMarkers(e, gomod)
		if err != nil {
			return fmt.Errorf("failed to extract expectations for go.mod: %v", err)
		}
		notes = append(notes, l...)
	}
	e.notes = notes
	return nil
}

func goModMarkers(e *Exported, gomod string) ([]*expect.Note, error) {
	if _, err := os.Stat(gomod); os.IsNotExist(err) {
		// If there is no go.mod file, we want to be able to continue.
		return nil, nil
	}
	content, err := e.FileContents(gomod)
	if err != nil {
		return nil, err
	}
	if e.Exporter == GOPATH {
		return expect.Parse(e.ExpectFileSet, gomod, content)
	}
	gomod = strings.TrimSuffix(gomod, ".temp")
	// If we are in Modules mode, copy the original contents file back into go.mod
	if err := os.WriteFile(gomod, content, 0644); err != nil {
		return nil, nil
	}
	return expect.Parse(e.ExpectFileSet, gomod, content)
}

func (e *Exported) getMarkers() error {
	if e.markers != nil {
		return nil
	}
	// set markers early so that we don't call getMarkers again from Expect
	e.markers = make(map[string]astutil.Range)
	return e.Expect(map[string]any{
		markMethod: e.Mark,
	})
}

var (
	noteType       = reflect.TypeFor[*expect.Note]()
	identifierType = reflect.TypeFor[expect.Identifier]()
	posType        = reflect.TypeFor[token.Pos]()
	positionType   = reflect.TypeFor[token.Position]()
	rangeType      = reflect.TypeFor[astutil.Range]()
	fsetType       = reflect.TypeFor[*token.FileSet]()
	regexType      = reflect.TypeFor[*regexp.Regexp]()
	exportedType   = reflect.TypeFor[*Exported]()
)

// converter converts from a marker's argument parsed from the comment to reflect values passed to the method during Invoke.
type converter func(*expect.Note, []any) (reflect.Value, []any, error)

// method is used to track information about Invoke methods.
type method struct {
	f          reflect.Value // the reflect value of the passed in method
	converters []converter   // the parameter converters for the method
}

// buildConverter works out what function should be used to go from an ast expressions to a reflect
// value of the type expected by a method.
// It is called when only the target type is know, it returns converters that are flexible across
// all supported expression types for that target type.
func (e *Exported) buildConverter(pt reflect.Type) (converter, error) {
	switch {
	case pt == noteType:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			return reflect.ValueOf(n), args, nil
		}, nil
	case pt == fsetType:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			return reflect.ValueOf(e.ExpectFileSet), args, nil
		}, nil
	case pt == exportedType:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			return reflect.ValueOf(e), args, nil
		}, nil
	case pt == posType:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			r, remains, err := e.rangeConverter(n, args)
			if err != nil {
				return reflect.Value{}, nil, err
			}
			return reflect.ValueOf(r.Start), remains, nil
		}, nil
	case pt == positionType:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			r, remains, err := e.rangeConverter(n, args)
			if err != nil {
				return reflect.Value{}, nil, err
			}
			return reflect.ValueOf(e.ExpectFileSet.Position(r.Start)), remains, nil
		}, nil
	case pt == rangeType:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			r, remains, err := e.rangeConverter(n, args)
			if err != nil {
				return reflect.Value{}, nil, err
			}
			return reflect.ValueOf(r), remains, nil
		}, nil
	case pt == identifierType:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			if len(args) < 1 {
				return reflect.Value{}, nil, fmt.Errorf("missing argument")
			}
			arg := args[0]
			args = args[1:]
			switch arg := arg.(type) {
			case expect.Identifier:
				return reflect.ValueOf(arg), args, nil
			default:
				return reflect.Value{}, nil, fmt.Errorf("cannot convert %v to string", arg)
			}
		}, nil

	case pt == regexType:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			if len(args) < 1 {
				return reflect.Value{}, nil, fmt.Errorf("missing argument")
			}
			arg := args[0]
			args = args[1:]
			if _, ok := arg.(*regexp.Regexp); !ok {
				return reflect.Value{}, nil, fmt.Errorf("cannot convert %v to *regexp.Regexp", arg)
			}
			return reflect.ValueOf(arg), args, nil
		}, nil

	case pt.Kind() == reflect.String:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			if len(args) < 1 {
				return reflect.Value{}, nil, fmt.Errorf("missing argument")
			}
			arg := args[0]
			args = args[1:]
			switch arg := arg.(type) {
			case expect.Identifier:
				return reflect.ValueOf(string(arg)), args, nil
			case string:
				return reflect.ValueOf(arg), args, nil
			default:
				return reflect.Value{}, nil, fmt.Errorf("cannot convert %v to string", arg)
			}
		}, nil
	case pt.Kind() == reflect.Int64:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			if len(args) < 1 {
				return reflect.Value{}, nil, fmt.Errorf("missing argument")
			}
			arg := args[0]
			args = args[1:]
			switch arg := arg.(type) {
			case int64:
				return reflect.ValueOf(arg), args, nil
			default:
				return reflect.Value{}, nil, fmt.Errorf("cannot convert %v to int", arg)
			}
		}, nil
	case pt.Kind() == reflect.Bool:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			if len(args) < 1 {
				return reflect.Value{}, nil, fmt.Errorf("missing argument")
			}
			arg := args[0]
			args = args[1:]
			b, ok := arg.(bool)
			if !ok {
				return reflect.Value{}, nil, fmt.Errorf("cannot convert %v to bool", arg)
			}
			return reflect.ValueOf(b), args, nil
		}, nil
	case pt.Kind() == reflect.Slice:
		return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
			converter, err := e.buildConverter(pt.Elem())
			if err != nil {
				return reflect.Value{}, nil, err
			}
			result := reflect.MakeSlice(reflect.SliceOf(pt.Elem()), 0, len(args))
			for range args {
				value, remains, err := converter(n, args)
				if err != nil {
					return reflect.Value{}, nil, err
				}
				result = reflect.Append(result, value)
				args = remains
			}
			return result, args, nil
		}, nil
	default:
		if pt.Kind() == reflect.Interface && pt.NumMethod() == 0 {
			return func(n *expect.Note, args []any) (reflect.Value, []any, error) {
				if len(args) < 1 {
					return reflect.Value{}, nil, fmt.Errorf("missing argument")
				}
				return reflect.ValueOf(args[0]), args[1:], nil
			}, nil
		}
		return nil, fmt.Errorf("param has unexpected type %v (kind %v)", pt, pt.Kind())
	}
}

func (e *Exported) rangeConverter(n *expect.Note, args []any) (astutil.Range, []any, error) {
	tokFile := e.ExpectFileSet.File(n.Pos)
	if len(args) < 1 {
		return astutil.Range{}, nil, fmt.Errorf("missing argument")
	}
	arg := args[0]
	args = args[1:]
	switch arg := arg.(type) {
	case expect.Identifier:
		// handle the special identifiers
		switch arg {
		case eofIdentifier:
			// end of file identifier
			eof := tokFile.Pos(tokFile.Size())
			return newRange(tokFile, eof, eof), args, nil
		default:
			// look up a marker by name
			mark, ok := e.markers[string(arg)]
			if !ok {
				return astutil.Range{}, nil, fmt.Errorf("cannot find marker %v", arg)
			}
			return mark, args, nil
		}
	case string:
		start, end, err := expect.MatchBefore(tokFile, e.FileContents, n.Pos, arg)
		if err != nil {
			return astutil.Range{}, nil, err
		}
		if !start.IsValid() {
			return astutil.Range{}, nil, fmt.Errorf("%v: pattern %s did not match", tokFile.Position(n.Pos), arg)
		}
		return newRange(tokFile, start, end), args, nil
	case *regexp.Regexp:
		start, end, err := expect.MatchBefore(tokFile, e.FileContents, n.Pos, arg)
		if err != nil {
			return astutil.Range{}, nil, err
		}
		if !start.IsValid() {
			return astutil.Range{}, nil, fmt.Errorf("%v: pattern %s did not match", tokFile.Position(n.Pos), arg)
		}
		return newRange(tokFile, start, end), args, nil
	default:
		return astutil.Range{}, nil, fmt.Errorf("cannot convert %v to pos", arg)
	}
}

// newRange creates a new Range from valid positions within the specified token.File.
func newRange(file *token.File, start, end token.Pos) astutil.Range {
	fileBase := file.Base()
	fileEnd := fileBase + file.Size()
	if !start.IsValid() {
		panic("invalid start token.Pos")
	}
	if !end.IsValid() {
		panic("invalid end token.Pos")
	}
	if int(start) < fileBase || int(start) > fileEnd {
		panic(fmt.Sprintf("invalid start: %d not in [%d, %d]", start, fileBase, fileEnd))
	}
	if int(end) < fileBase || int(end) > fileEnd {
		panic(fmt.Sprintf("invalid end: %d not in [%d, %d]", end, fileBase, fileEnd))
	}
	if start > end {
		panic("invalid start: greater than end")
	}
	return astutil.RangeOf(start, end)
}
