// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package pkgbits

// Version indicates a version of a unified IR bitstream.
type Version uint32

const (
	// V0: initial prototype.
	V0 Version = iota

	// V1: adds the Flags uint32 word
	V1

	// V2: removes unused legacy fields and supports type parameters for aliases.
	V2

	// V3.
	V3

	// V4: encodes generic methods as standalone function objects
	V4

	// V5: adds a parameter's default value
	V5

	// V6: a parameter default may be a struct literal of constants, and a variable carries its readonly bit
	V6

	numVersions = iota
)

// Field denotes a unit of data in the serialized unified IR bitstream. It is conceptually a like field in a structure.
type Field int

const (
	// Flags in a uint32 in the header of a bitstream that is used to indicate whether optional features are enabled.
	Flags Field = iota

	// Deprecated: HasInit was a bool indicating whether a package has any init functions.
	HasInit

	// Deprecated: DerivedFuncInstance was a bool indicating whether an object was a function instance.
	DerivedFuncInstance

	// ObjAlias has a list of TypeParamNames.
	AliasTypeParamNames

	// Deprecated: DerivedInfoNeeded was a bool indicating whether a type was a derived type.
	DerivedInfoNeeded

	// Composite literals use a more compact format for element lists.
	CompactCompLiterals

	// Generic methods may appear as standalone function objects.
	GenericMethods

	// A parameter carries the value a call passes when it omits the argument.
	ParamDefaults

	// A parameter default is a constant or a keyed struct literal of them, tagged by a bool, rather than a bare constant.
	StructParamDefaults

	// A variable carries a bool that says whether it was declared "readonly var".
	ReadonlyVars

	// Method index is encoded to preserve relative order of nongeneric and generic methods.
	PreserveMethodOrder

	numFields = iota
)

// introduced is the version a field was added.
var introduced = [numFields]Version{
	Flags:               V1,
	AliasTypeParamNames: V2,
	CompactCompLiterals: V3,
	GenericMethods:      V4,
	ParamDefaults:       V5,
	StructParamDefaults: V6,
	ReadonlyVars:        V6,

	// numVersions is above every version a stream can declare, so Has is false for all of them.
	PreserveMethodOrder: numVersions,
}

var removed = [numFields]Version{
	HasInit:             V2,
	DerivedFuncInstance: V2,
	DerivedInfoNeeded:   V2,
}

// Has reports whether field f is present in a bitstream at version v.
func (v Version) Has(f Field) bool {
	return introduced[f] <= v && (v < removed[f] || removed[f] == V0)
}
