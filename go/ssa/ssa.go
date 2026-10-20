// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package ssa

// This package defines a high-level intermediate representation for
// Go programs using static single-assignment (SSA) form.

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/tools/go/types/typeutil"
	"golang.org/x/tools/internal/typeparams"
)

// A Program is a partial or complete Go program converted to SSA form.
type Program struct {
	Fset       *token.FileSet              // position information for the files of this Program
	imported   map[string]*Package         // all importable Packages, keyed by import path
	packages   map[*types.Package]*Package // all created Packages
	mode       BuilderMode                 // set of mode bits for SSA construction
	MethodSets typeutil.MethodSetCache     // cache of type-checker's method-sets

	canon *canonizer     // type canonicalization map
	ctxt  *types.Context // cache for type checking instantiations

	methodsMu  sync.Mutex
	methodSets typeutil.Map // maps type to its concrete *methodSet

	// memoization of whether a type refers to type parameters
	hasParamsMu sync.Mutex
	hasParams   typeparams.Free

	// set of concrete types used as MakeInterface operands
	makeInterfaceTypesMu sync.Mutex
	makeInterfaceTypes   map[types.Type]unit // (may contain redundant identical types)

	// objectMethods is a memoization of objectMethod to avoid creation of duplicate methods from type information.
	objectMethodsMu sync.Mutex
	objectMethods   map[*types.Func]*Function

	noReturn func(*types.Func) bool // (optional) predicate that decides whether a given call cannot return
}

// A Package is a single analyzed Go package containing Members for all
// package-level functions, variables, constants and types it declares. These
// may be accessed directly via Members, or via the type-specific accessor
// methods Func, Type, Var and Const.
type Package struct {
	Prog    *Program                // the owning program
	Pkg     *types.Package          // the corresponding go/types.Package
	Members map[string]Member       // all package members keyed by name (incl. init and init#%d)
	objects map[types.Object]Member // mapping of package objects to members (incl. methods). Contains *NamedConst, *Global, *Function (values but not types)
	init    *Function               // Func("init"); the package's init function
	debug   bool                    // include full debug info in this package
	syntax  bool                    // package was loaded from syntax

	// The following fields are set transiently, then cleared after building.
	buildOnce   sync.Once           // ensures package building occurs once
	ninit       int32               // number of init functions
	info        *types.Info         // package type information
	files       []*ast.File         // package ASTs
	created     []*Function         // members created as a result of building this package (includes declared functions, wrappers)
	initVersion map[ast.Expr]string // goversion to use for each global var init expr
}

// A Member is a member of a Go package, implemented by *NamedConst,
// *Global, *Function, or *Type. They are created by package-level
// const, var, func and type declarations respectively.
type Member interface {
	Name() string                    // declared name of the package member
	String() string                  // package-qualified name of the package member
	RelString(*types.Package) string // like String, but relative refs are unqualified
	Object() types.Object            // typechecker's object for this member, if any
	Pos() token.Pos                  // position of member's declaration, if known
	Type() types.Type                // type of the package member
	Token() token.Token              // token.{VAR,FUNC,CONST,TYPE}
	Package() *Package               // the containing package
}

// A Type is a Member of a Package representing a package-level named type.
type Type struct {
	object *types.TypeName
	pkg    *Package
}

// A NamedConst is a Member of a Package representing a package-level
// named constant.
type NamedConst struct {
	object *types.Const
	Value  *Const
	pkg    *Package
}

// A Value is an SSA value that can be referenced by an instruction.
type Value interface {
	// Name returns the name of this value.
	Name() string

	// If this value is an Instruction, String returns its disassembled form.
	String() string

	// Type returns the type of this value.
	Type() types.Type

	// Parent returns the function to which this Value belongs.
	Parent() *Function

	// Referrers returns the list of instructions that have this value as one of their operands.
	Referrers() *[]Instruction

	// Pos returns the location of the AST.
	Pos() token.Pos
}

// An Instruction is an SSA instruction that computes a new Value or has some
// effect.
type Instruction interface {
	// String returns the disassembled form of this value.
	String() string

	// Parent returns the function to which this instruction belongs.
	Parent() *Function

	// Block returns the basic block to which this instruction belongs.
	Block() *BasicBlock

	// setBlock sets the basic block to which this instruction belongs.
	setBlock(*BasicBlock)

	// Operands returns the operands of this instruction: the set of Values it references.
	Operands(rands []*Value) []*Value

	// Pos returns the location of the AST.
	Pos() token.Pos
}

// A Node is a node in the SSA value graph. Every concrete type that
// implements Node is also either a Value, an Instruction, or both.
type Node interface {
	// Common methods:
	String() string
	Pos() token.Pos
	Parent() *Function

	// Partial methods:
	Operands(rands []*Value) []*Value // nil for non-Instructions
	Referrers() *[]Instruction        // nil for non-Values
}

// Function represents the parameters, results, and code of a function or
// method.
//
// If Blocks is nil, this indicates an external function for which no Go
// source code is available. In this case, FreeVars, Locals, and Params are
// nil too. Clients performing whole-program analysis must handle external
// functions specially.
//
// Blocks contains the function's control-flow graph (CFG). Blocks[0] is the
// function entry point; block order is not otherwise semantically
// significant, though it may affect the readability of the disassembly. To
// iterate over the blocks in dominance order, use DomPreorder().
type Function struct {
	name      string
	object    *types.Func // symbol for declared function (nil for FuncLit or synthetic init)
	method    *selection  // info about provenance of synthetic methods; thunk => non-nil
	Signature *types.Signature
	pos       token.Pos

	// source information
	Synthetic string      // provenance of synthetic function; "" for true source functions
	syntax    ast.Node    // *ast.Func{Decl,Lit}, if from syntax (incl. generic instances) or (*ast.RangeStmt if a yield function)
	info      *types.Info // type annotations (if syntax != nil)
	goversion string      // Go version of syntax (NB: init is special)

	parent *Function // enclosing function if anon; nil if global
	Pkg    *Package  // enclosing package; nil for shared funcs (wrappers and error.Error)
	Prog   *Program  // enclosing program

	buildshared *task // wait for a shared function to be done building (may be nil if <=1 builder ever needs to wait)

	// These fields are populated only when the function body is built:

	Params    []*Parameter  // function parameters; for methods, includes receiver
	FreeVars  []*FreeVar    // free variables whose values must be supplied by closure
	Locals    []*Alloc      // frame-allocated variables of this function
	Blocks    []*BasicBlock // basic blocks of the function; nil => external
	Recover   *BasicBlock   // optional; control transfers here after recovered panic
	AnonFuncs []*Function   // anonymous functions (from FuncLit,RangeStmt) directly beneath this one
	referrers []Instruction // referring instructions (iff Parent() != nil)
	anonIdx   int32         // position of a nested function in parent's AnonFuncs. fn.Parent()!=nil => fn.Parent().AnonFunc[fn.anonIdx] == fn.

	recvtypeparams *types.TypeParamList // receiver type parameters of this function. recvtypeparams.Len() > 0 => method on generic or instance of generic type
	recvtypeargs   []types.Type         // type arguments that instantiated recvtypeparams. len(recvtypeargs) > 0 => method on instance of generic type
	typeparams     *types.TypeParamList // type parameters of this function. typeparams.Len() > 0 => generic or instance of generic function or method
	typeargs       []types.Type         // type arguments that instantiated typeparams. len(typeargs) > 0 => instance of generic function or method
	topLevelOrigin *Function            // the origin function if this is an instance of a source function. nil if Parent()!=nil.
	generic        *generic             // instances of this function, if generic

	// The following fields are cleared after building.
	build        buildFunc                // algorithm to build function body (nil => built)
	currentBlock *BasicBlock              // where to emit code
	vars         map[*types.Var]Value     // addresses of local variables
	results      []*Alloc                 // result allocations of the current function
	returnVars   []*types.Var             // variables for a return statement. Either results or for range-over-func a parent's results
	targets      *targets                 // linked stack of branch targets
	lblocks      map[*types.Label]*lblock // labelled blocks
	subst        *subster                 // type parameter substitutions (if non-nil)
	jump         *types.Var               // synthetic variable for the yield state (non-nil => range-over-func)
	deferstack   *types.Var               // synthetic variable holding enclosing ssa:deferstack()
	source       *Function                // nearest enclosing source function
	exits        []*exit                  // exits of the function that need to be resolved
	uniq         int64                    // source of unique ints within the source tree while building
}

// BasicBlock represents an SSA basic block. The final element of Instrs is
// always an explicit transfer of control (If, Jump, Return, or Panic).
type BasicBlock struct {
	Index        int            // index of this block within Parent().Blocks
	Comment      string         // optional label; no semantic significance
	parent       *Function      // parent function
	Instrs       []Instruction  // instructions in order
	Preds, Succs []*BasicBlock  // predecessors and successors
	succs2       [2]*BasicBlock // initial space for Succs
	dom          domInfo        // dominator tree info
	gaps         int            // number of nil Instrs (transient)
	rundefers    int            // number of rundefers (transient)
}

// Pure values ----------------------------------------

// A FreeVar represents a free variable of the function to which it belongs.
type FreeVar struct {
	name      string
	typ       types.Type
	pos       token.Pos
	parent    *Function
	referrers []Instruction

	// Transiently needed during building.
	outer Value // the Value captured from the enclosing context.
}

// A Parameter represents an input parameter of a function.
type Parameter struct {
	name      string
	object    *types.Var // non-nil
	typ       types.Type
	parent    *Function
	referrers []Instruction
}

// A Const represents a value known at build time.
type Const struct {
	typ   types.Type
	Value constant.Value
}

// A Global is a named Value holding the address of a package-level variable.
type Global struct {
	name   string
	object types.Object // a *types.Var; may be nil for synthetics e.g. init$guard
	typ    types.Type
	pos    token.Pos

	Pkg *Package
}

// A Builtin represents a specific use of a built-in function, e.g. len.
// Builtins are immutable values. Builtins do not have addresses.
type Builtin struct {
	name string
	sig  *types.Signature
}

// Value-defining instructions  ----------------------------------------

// The Alloc instruction reserves space for a variable of the given type,
// zero-initializes it, and yields its address.
type Alloc struct {
	register
	Comment string
	Heap    bool
	index   int // dense numbering; for lifting
}

// The Phi instruction represents an SSA φ-node, which combines values that
// differ across incoming control-flow edges and yields a new value.
type Phi struct {
	register
	Comment string  // a hint as to its purpose
	Edges   []Value // Edges[i] is value for Block().Preds[i]
}

// The Call instruction represents a function or method call. The Call
// instruction yields the function result if there is exactly one.
type Call struct {
	register
	Call CallCommon
}

// The BinOp instruction yields the result of binary operation X Op Y.
//
// Pos() returns the ast.BinaryExpr.OpPos, if explicit in the source.
type BinOp struct {
	register
	// One of: ADD SUB MUL QUO REM + - * / % AND OR XOR SHL SHR AND_NOT & | ^ << >> &^ EQL NEQ LSS LEQ GTR GEQ == != < <= < >=
	Op   token.Token
	X, Y Value
}

// The UnOp instruction yields the result of Op X. ARROW is channel receive.
// MUL is pointer indirection (load). XOR is bitwise complement.
type UnOp struct {
	register
	Op      token.Token // One of: NOT SUB ARROW MUL XOR ! - <- * ^
	X       Value
	CommaOk bool
}

// The ChangeType instruction applies to X a value-preserving type
// change to Type().
type ChangeType struct {
	register
	X Value
}

// The Convert instruction yields the conversion of value X to type Type().
// One or both of those types is basic (but possibly named).
type Convert struct {
	register
	X Value
}

// The MultiConvert instruction yields the conversion of value X to type
// Type(). Either X.Type() or Type() must be a type parameter.
type MultiConvert struct {
	register
	X        Value
	from, to types.Type
}

// ChangeInterface constructs a value of one interface type from a value of
// another interface type known to be assignable to it.
type ChangeInterface struct {
	register
	X Value
}

// The SliceToArrayPointer instruction yields the conversion of slice X to
// array pointer.
type SliceToArrayPointer struct {
	register
	X Value
}

// MakeInterface constructs an instance of an interface type from a value of a
// concrete type.
type MakeInterface struct {
	register
	X Value
}

// The MakeClosure instruction yields a closure value whose code is
// Fn and whose free variables' values are supplied by Bindings.
type MakeClosure struct {
	register
	Fn       Value   // always a *Function
	Bindings []Value // values for each free variable in Fn.FreeVars
}

// The MakeMap instruction creates a new hash-table-based map object
// and yields a value of kind map.
type MakeMap struct {
	register
	Reserve Value // initial space reservation; nil => default
}

// The MakeChan instruction creates a new channel object and yields a value of kind chan.
//
// Type() returns a (possibly named) *types.Chan.
type MakeChan struct {
	register
	Size Value // int; size of buffer; zero => synchronous.
}

// The MakeSlice instruction yields a slice of length Len backed by a newly
// allocated array of length Cap.
type MakeSlice struct {
	register
	Len Value
	Cap Value
}

// The Slice instruction yields a slice of an existing string, slice or *array
// X between optional integer bounds Low and High.
type Slice struct {
	register
	X              Value // slice, string, or *array
	Low, High, Max Value // each may be nil
}

// The FieldAddr instruction yields the address of Field of *struct X.
type FieldAddr struct {
	register
	X     Value // *struct
	Field int   // index into CoreType(CoreType(X.Type()).(*types.Pointer).Elem()).(*types.Struct).Fields
}

// The Field instruction yields the Field of struct X.

// Example printed form:
//
//	t1 = t0.name [#1]
type Field struct {
	register
	X     Value // struct
	Field int   // index into CoreType(X.Type()).(*types.Struct).Fields
}

// The IndexAddr instruction yields the address of the element at index Index
// of collection X. Index is an integer expression.
type IndexAddr struct {
	register
	X     Value // *array, slice or type parameter with types array, *array, or slice.
	Index Value // numeric index
}

// The Index instruction yields element Index of collection X, an array,
// string or type parameter containing an array or a string.
type Index struct {
	register
	X     Value // array, string or type parameter with types array, *array, slice, or string.
	Index Value // integer index
}

// The Lookup instruction yields element Index of collection map X.
// Index is the appropriate key type.
type Lookup struct {
	register
	X       Value // map
	Index   Value // key-typed index
	CommaOk bool  // return a value,ok pair
}

// SelectState is a helper for Select.
// It represents one goal state and its corresponding communication.
type SelectState struct {
	Dir       types.ChanDir // direction of case (SendOnly or RecvOnly)
	Chan      Value         // channel to use (for send or receive)
	Send      Value         // value to send (for send)
	Pos       token.Pos     // position of token.ARROW
	DebugNode ast.Node      // ast.SendStmt or ast.UnaryExpr(<-) [debug mode]
}

// The Select instruction tests whether (or blocks until) one of the specified
// sent or received states is entered.
type Select struct {
	register
	States   []*SelectState
	Blocking bool
}

// The Range instruction yields an iterator over the domain and range
// of X, which must be a string or map.
//
// Elements are accessed via Next.
type Range struct {
	register
	X Value // string or map
}

// The Next instruction reads and advances the (map or string) iterator Iter
// and returns a 3-tuple value (ok, k, v).
type Next struct {
	register
	Iter     Value
	IsString bool // true => string iterator; false => map iterator.
}

// The TypeAssert instruction tests whether interface value X has type
// AssertedType.
type TypeAssert struct {
	register
	X            Value
	AssertedType types.Type
	CommaOk      bool
}

// The Extract instruction yields component Index of Tuple.
type Extract struct {
	register
	Tuple Value
	Index int
}

// Instructions executed for effect. They do not yield a value.

// The Jump instruction transfers control to the sole successor of its
// owning block.
type Jump struct {
	anInstruction
}

// The If instruction transfers control to one of the successors of its owning
// block, depending on the boolean Cond.
type If struct {
	anInstruction
	Cond Value
}

// The Return instruction returns values and control back to the calling
// function.
type Return struct {
	anInstruction
	Results []Value
	pos     token.Pos
}

// The RunDefers instruction pops and invokes the entire stack of
// procedure calls pushed by Defer instructions in this function.
type RunDefers struct {
	anInstruction
}

// The Panic instruction initiates a panic with value X.
type Panic struct {
	anInstruction
	X   Value // an interface{}
	pos token.Pos
}

// The Go instruction creates a new goroutine and calls the specified
// function within it.
type Go struct {
	anInstruction
	Call CallCommon
	pos  token.Pos
}

// The Defer instruction pushes the specified call onto a stack of functions
// to be called by a RunDefers instruction or by a panic.
type Defer struct {
	anInstruction
	Call       CallCommon
	DeferStack Value // stack of deferred functions (from ssa:deferstack() intrinsic) onto which this function is pushed
	pos        token.Pos
}

// The Send instruction sends X on channel Chan.
//
// Pos() returns the ast.SendStmt.Arrow, if explicit in the source.
type Send struct {
	anInstruction
	Chan, X Value
	pos     token.Pos
}

// The Store instruction stores Val at address Addr.
// Stores can be of arbitrary types.
type Store struct {
	anInstruction
	Addr Value
	Val  Value
	pos  token.Pos
}

// The MapUpdate instruction updates the association of Map[Key] to Value.
type MapUpdate struct {
	anInstruction
	Map   Value
	Key   Value
	Value Value
	pos   token.Pos
}

// A DebugRef instruction maps a source-level expression Expr to the SSA value
// X that represents the value (!IsAddr) or address (IsAddr) of that
// expression.
type DebugRef struct {
	// TODO(generics): Reconsider what DebugRefs are for generics.
	anInstruction
	Expr   ast.Expr     // the referring expression (never *ast.ParenExpr)
	object types.Object // the identity of the source var/func
	IsAddr bool         // Expr is addressable and X is the address it denotes
	X      Value        // the value or address of Expr
}

// Embeddable mix-ins and helpers for common parts of other structs. -----------

// register is a mix-in embedded by all SSA values that are also instructions,
// i.e. virtual registers.
type register struct {
	anInstruction
	num       int        // "name" of virtual register, e.g. "t0". Not guaranteed unique.
	typ       types.Type // type of virtual register
	pos       token.Pos  // position of source expression, or NoPos
	referrers []Instruction
}

// anInstruction is a mix-in embedded by all Instructions.
// It provides the implementations of the Block and setBlock methods.
type anInstruction struct {
	block *BasicBlock // the basic block of this instruction
}

// CallCommon is contained by Go, Defer and Call to hold the common parts of a
// function or method call.
type CallCommon struct {
	Value  Value       // receiver (invoke mode) or func value (call mode)
	Method *types.Func // interface method (invoke mode)
	Args   []Value     // actual parameters (in static method call, includes receiver)
	pos    token.Pos   // position of CallExpr.Lparen, iff explicit in source
}

// IsInvoke returns true if this call has "invoke" (not "call") mode.
func (c *CallCommon) IsInvoke() bool {
	return c.Method != nil
}

func (c *CallCommon) Pos() token.Pos { return c.pos }

// Signature returns the signature of the called function.
//
// For an "invoke"-mode call, the signature of the interface method is
// returned.
func (c *CallCommon) Signature() *types.Signature {
	if c.Method != nil {
		return c.Method.Type().(*types.Signature)
	}
	return typeparams.CoreType(c.Value.Type()).(*types.Signature)
}

// StaticCallee returns the callee if this is a trivially static
// "call"-mode call to a function.
func (c *CallCommon) StaticCallee() *Function {
	switch fn := c.Value.(type) {
	case *Function:
		return fn
	case *MakeClosure:
		return fn.Fn.(*Function)
	}
	return nil
}

// Description returns a description of the mode of this call suitable
// for a user interface, e.g., "static method call".
func (c *CallCommon) Description() string {
	switch fn := c.Value.(type) {
	case *Builtin:
		return "built-in function call"
	case *MakeClosure:
		return "static function closure call"
	case *Function:
		if fn.Signature.Recv() != nil {
			return "static method call"
		}
		return "static function call"
	}
	if c.IsInvoke() {
		return "dynamic method call" // ("invoke" mode)
	}
	return "dynamic function call"
}

// The CallInstruction interface, implemented by *Go, *Defer and *Call,
// exposes the common parts of function-calling instructions, yet provides a
// way back to the Value.
type CallInstruction interface {
	Instruction
	Common() *CallCommon // returns the common parts of the call
	Value() *Call        // returns the result value of the call (*Call) or nil (*Go, *Defer)
}

func (s *Call) Common() *CallCommon  { return &s.Call }
func (s *Defer) Common() *CallCommon { return &s.Call }
func (s *Go) Common() *CallCommon    { return &s.Call }

func (s *Call) Value() *Call  { return s }
func (s *Defer) Value() *Call { return nil }
func (s *Go) Value() *Call    { return nil }

func (v *Builtin) Type() types.Type        { return v.sig }
func (v *Builtin) Name() string            { return v.name }
func (*Builtin) Referrers() *[]Instruction { return nil }
func (v *Builtin) Pos() token.Pos          { return token.NoPos }
func (v *Builtin) Object() types.Object    { return types.Universe.Lookup(v.name) }
func (v *Builtin) Parent() *Function       { return nil }

func (v *FreeVar) Type() types.Type          { return v.typ }
func (v *FreeVar) Name() string              { return v.name }
func (v *FreeVar) Referrers() *[]Instruction { return &v.referrers }
func (v *FreeVar) Pos() token.Pos            { return v.pos }
func (v *FreeVar) Parent() *Function         { return v.parent }

func (v *Global) Type() types.Type                     { return v.typ }
func (v *Global) Name() string                         { return v.name }
func (v *Global) Parent() *Function                    { return nil }
func (v *Global) Pos() token.Pos                       { return v.pos }
func (v *Global) Referrers() *[]Instruction            { return nil }
func (v *Global) Token() token.Token                   { return token.VAR }
func (v *Global) Object() types.Object                 { return v.object }
func (v *Global) String() string                       { return v.RelString(nil) }
func (v *Global) Package() *Package                    { return v.Pkg }
func (v *Global) RelString(from *types.Package) string { return relString(v, from) }

func (v *Function) Name() string       { return v.name }
func (v *Function) Type() types.Type   { return v.Signature }
func (v *Function) Pos() token.Pos     { return v.pos }
func (v *Function) Token() token.Token { return token.FUNC }
func (v *Function) Object() types.Object {
	if v.object != nil {
		return types.Object(v.object)
	}
	return nil
}
func (v *Function) String() string    { return v.RelString(nil) }
func (v *Function) Package() *Package { return v.Pkg }
func (v *Function) Parent() *Function { return v.parent }
func (v *Function) Referrers() *[]Instruction {
	if v.parent != nil {
		return &v.referrers
	}
	return nil
}

// TypeParams are the function's type parameters if generic or the type
// parameters that were instantiated if fn is an instantiation.
func (fn *Function) TypeParams() *types.TypeParamList {
	return consTypeParamLists(fn.recvtypeparams, fn.typeparams)
}

func consTypeParamLists(l, r *types.TypeParamList) *types.TypeParamList {
	if l.Len() == 0 {
		return r
	}
	if r.Len() == 0 {
		return l
	}

	tpars := make([]*types.TypeParam, l.Len()+r.Len())
	for i := range l.Len() {
		tpars[i] = l.At(i)
	}
	for i := range r.Len() {
		tpars[i+l.Len()] = r.At(i)
	}
	// This logic unsafely assumes.
	t := reflect.TypeFor[types.TypeParamList]()
	if t.NumField() != 1 {
		panic("TypeParamList has unexpected fields")
	}
	if f := t.Field(0); f.Offset != 0 || f.Type != reflect.TypeFor[[]*types.TypeParam]() {
		panic("TypeParamList field is not []*TypeParam")
	}
	return (*types.TypeParamList)(unsafe.Pointer(&tpars))
}

// TypeArgs are the types that TypeParams() were instantiated by to create fn
// from fn.Origin().
func (fn *Function) TypeArgs() []types.Type {
	return slices.Concat(fn.recvtypeargs, fn.typeargs)
}

// Origin returns the generic function from which fn was instantiated,
// or nil if fn is not an instantiation.
func (fn *Function) Origin() *Function {
	if fn.parent != nil && fn.parent.hasTypeArgs() {
		// Nested functions are BUILT at a different time than their instances. Build declared package if not yet BUILT.
		fn.declaredPackage().Build()
	}
	return origin(fn)
}

// hasTypeParams returns whether fn has any type parameters
func (fn *Function) hasTypeParams() bool {
	return fn.recvtypeparams.Len()+fn.typeparams.Len() > 0
}

// hasTypeArgs returns whether fn has any type arguments
func (fn *Function) hasTypeArgs() bool {
	return len(fn.recvtypeargs)+len(fn.typeargs) > 0
}

// subrtargs returns fn's receiver type parameters substituted with receiver type arguments
func (fn *Function) subrtargs(m *types.Func) []types.Type {
	return fn.subst.types(receiverTypeArgs(m))
}

// subtargs returns fn's type parameters substituted with (possibly implied) type arguments
func (fn *Function) subtargs(id *ast.Ident) []types.Type {
	return fn.subst.types(instanceArgs(fn.info, id))
}

// targstr returns a comma-separated string of the types in targs
func targstr(targs []types.Type) string {
	var sb strings.Builder
	if len(targs) > 0 {
		sb.WriteString("[")
		for i := range targs {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(targs[i].String())
		}
		sb.WriteString("]")
	}
	return sb.String()
}

// origin is the function that fn is an instantiation of. Returns nil if fn is
// not an instantiation.
//
// Precondition: fn and the origin function are done building.
func origin(fn *Function) *Function {
	if fn.parent != nil && fn.parent.hasTypeArgs() {
		return origin(fn.parent).AnonFuncs[fn.anonIdx]
	}
	return fn.topLevelOrigin
}

func (v *Parameter) Type() types.Type          { return v.typ }
func (v *Parameter) Name() string              { return v.name }
func (v *Parameter) Object() types.Object      { return v.object }
func (v *Parameter) Referrers() *[]Instruction { return &v.referrers }
func (v *Parameter) Pos() token.Pos            { return v.object.Pos() }
func (v *Parameter) Parent() *Function         { return v.parent }

func (v *Alloc) Type() types.Type          { return v.typ }
func (v *Alloc) Referrers() *[]Instruction { return &v.referrers }
func (v *Alloc) Pos() token.Pos            { return v.pos }

func (v *register) Type() types.Type          { return v.typ }
func (v *register) setType(typ types.Type)    { v.typ = typ }
func (v *register) Name() string              { return fmt.Sprintf("t%d", v.num) }
func (v *register) setNum(num int)            { v.num = num }
func (v *register) Referrers() *[]Instruction { return &v.referrers }
func (v *register) Pos() token.Pos            { return v.pos }
func (v *register) setPos(pos token.Pos)      { v.pos = pos }

func (v *anInstruction) Parent() *Function          { return v.block.parent }
func (v *anInstruction) Block() *BasicBlock         { return v.block }
func (v *anInstruction) setBlock(block *BasicBlock) { v.block = block }
func (v *anInstruction) Referrers() *[]Instruction  { return nil }

func (t *Type) Name() string                         { return t.object.Name() }
func (t *Type) Pos() token.Pos                       { return t.object.Pos() }
func (t *Type) Type() types.Type                     { return t.object.Type() }
func (t *Type) Token() token.Token                   { return token.TYPE }
func (t *Type) Object() types.Object                 { return t.object }
func (t *Type) String() string                       { return t.RelString(nil) }
func (t *Type) Package() *Package                    { return t.pkg }
func (t *Type) RelString(from *types.Package) string { return relString(t, from) }

func (c *NamedConst) Name() string                         { return c.object.Name() }
func (c *NamedConst) Pos() token.Pos                       { return c.object.Pos() }
func (c *NamedConst) String() string                       { return c.RelString(nil) }
func (c *NamedConst) Type() types.Type                     { return c.object.Type() }
func (c *NamedConst) Token() token.Token                   { return token.CONST }
func (c *NamedConst) Object() types.Object                 { return c.object }
func (c *NamedConst) Package() *Package                    { return c.pkg }
func (c *NamedConst) RelString(from *types.Package) string { return relString(c, from) }

func (d *DebugRef) Object() types.Object { return d.object }

// Func returns the package-level function of the specified name,
// or nil if not found.
func (p *Package) Func(name string) (f *Function) {
	f, _ = p.Members[name].(*Function)
	return
}

// Var returns the package-level variable of the specified name,
// or nil if not found.
func (p *Package) Var(name string) (g *Global) {
	g, _ = p.Members[name].(*Global)
	return
}

// Const returns the package-level constant of the specified name,
// or nil if not found.
func (p *Package) Const(name string) (c *NamedConst) {
	c, _ = p.Members[name].(*NamedConst)
	return
}

// Type returns the package-level type of the specified name,
// or nil if not found.
func (p *Package) Type(name string) (t *Type) {
	t, _ = p.Members[name].(*Type)
	return
}

func (v *Call) Pos() token.Pos      { return v.Call.pos }
func (s *Defer) Pos() token.Pos     { return s.pos }
func (s *Go) Pos() token.Pos        { return s.pos }
func (s *MapUpdate) Pos() token.Pos { return s.pos }
func (s *Panic) Pos() token.Pos     { return s.pos }
func (s *Return) Pos() token.Pos    { return s.pos }
func (s *Send) Pos() token.Pos      { return s.pos }
func (s *Store) Pos() token.Pos     { return s.pos }
func (s *If) Pos() token.Pos        { return token.NoPos }
func (s *Jump) Pos() token.Pos      { return token.NoPos }
func (s *RunDefers) Pos() token.Pos { return token.NoPos }
func (s *DebugRef) Pos() token.Pos  { return s.Expr.Pos() }

// Operands.

func (v *Alloc) Operands(rands []*Value) []*Value {
	return rands
}

func (v *BinOp) Operands(rands []*Value) []*Value {
	return append(rands, &v.X, &v.Y)
}

func (c *CallCommon) Operands(rands []*Value) []*Value {
	rands = append(rands, &c.Value)
	for i := range c.Args {
		rands = append(rands, &c.Args[i])
	}
	return rands
}

func (s *Go) Operands(rands []*Value) []*Value {
	return s.Call.Operands(rands)
}

func (s *Call) Operands(rands []*Value) []*Value {
	return s.Call.Operands(rands)
}

func (s *Defer) Operands(rands []*Value) []*Value {
	return append(s.Call.Operands(rands), &s.DeferStack)
}

func (v *ChangeInterface) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

func (v *ChangeType) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

func (v *Convert) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

func (v *MultiConvert) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

func (v *SliceToArrayPointer) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

func (s *DebugRef) Operands(rands []*Value) []*Value {
	return append(rands, &s.X)
}

func (v *Extract) Operands(rands []*Value) []*Value {
	return append(rands, &v.Tuple)
}

func (v *Field) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

func (v *FieldAddr) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

func (s *If) Operands(rands []*Value) []*Value {
	return append(rands, &s.Cond)
}

func (v *Index) Operands(rands []*Value) []*Value {
	return append(rands, &v.X, &v.Index)
}

func (v *IndexAddr) Operands(rands []*Value) []*Value {
	return append(rands, &v.X, &v.Index)
}

func (*Jump) Operands(rands []*Value) []*Value {
	return rands
}

func (v *Lookup) Operands(rands []*Value) []*Value {
	return append(rands, &v.X, &v.Index)
}

func (v *MakeChan) Operands(rands []*Value) []*Value {
	return append(rands, &v.Size)
}

func (v *MakeClosure) Operands(rands []*Value) []*Value {
	rands = append(rands, &v.Fn)
	for i := range v.Bindings {
		rands = append(rands, &v.Bindings[i])
	}
	return rands
}

func (v *MakeInterface) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

func (v *MakeMap) Operands(rands []*Value) []*Value {
	return append(rands, &v.Reserve)
}

func (v *MakeSlice) Operands(rands []*Value) []*Value {
	return append(rands, &v.Len, &v.Cap)
}

func (v *MapUpdate) Operands(rands []*Value) []*Value {
	return append(rands, &v.Map, &v.Key, &v.Value)
}

func (v *Next) Operands(rands []*Value) []*Value {
	return append(rands, &v.Iter)
}

func (s *Panic) Operands(rands []*Value) []*Value {
	return append(rands, &s.X)
}

func (v *Phi) Operands(rands []*Value) []*Value {
	for i := range v.Edges {
		rands = append(rands, &v.Edges[i])
	}
	return rands
}

func (v *Range) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

func (s *Return) Operands(rands []*Value) []*Value {
	for i := range s.Results {
		rands = append(rands, &s.Results[i])
	}
	return rands
}

func (*RunDefers) Operands(rands []*Value) []*Value {
	return rands
}

func (v *Select) Operands(rands []*Value) []*Value {
	for i := range v.States {
		rands = append(rands, &v.States[i].Chan, &v.States[i].Send)
	}
	return rands
}

func (s *Send) Operands(rands []*Value) []*Value {
	return append(rands, &s.Chan, &s.X)
}

func (v *Slice) Operands(rands []*Value) []*Value {
	return append(rands, &v.X, &v.Low, &v.High, &v.Max)
}

func (s *Store) Operands(rands []*Value) []*Value {
	return append(rands, &s.Addr, &s.Val)
}

func (v *TypeAssert) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

func (v *UnOp) Operands(rands []*Value) []*Value {
	return append(rands, &v.X)
}

// Non-Instruction Values:
func (v *Builtin) Operands(rands []*Value) []*Value   { return rands }
func (v *FreeVar) Operands(rands []*Value) []*Value   { return rands }
func (v *Const) Operands(rands []*Value) []*Value     { return rands }
func (v *Function) Operands(rands []*Value) []*Value  { return rands }
func (v *Global) Operands(rands []*Value) []*Value    { return rands }
func (v *Parameter) Operands(rands []*Value) []*Value { return rands }
