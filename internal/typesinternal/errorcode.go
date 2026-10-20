// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package typesinternal

//go:generate stringer -type=ErrorCode

type ErrorCode int

// This file defines the error codes that can be produced during
// type-checking.

const (
	// InvalidSyntaxTree occurs if an invalid syntax tree is provided to the type checker. It should never happen.
	InvalidSyntaxTree ErrorCode = -1
)

const (
	_ ErrorCode = iota

	// Test is reserved for errors that only apply while in self-test mode.
	Test

	/* package names */

	// BlankPkgName occurs when a package name is the blank identifier "_".
	BlankPkgName

	// MismatchedPkgName occurs when a file's package name doesn't match the package name already established by other files.
	MismatchedPkgName

	// InvalidPkgUse occurs when a package identifier is used outside of a selector expression.
	InvalidPkgUse

	/* imports */

	// BadImportPath occurs when an import path is not valid.
	BadImportPath

	// BrokenImport occurs when importing a package fails.
	BrokenImport

	// ImportCRenamed occurs when the special import "C" is renamed. "C" is a pseudo-package, and must not be renamed.
	ImportCRenamed

	// UnusedImport occurs when an import is unused.
	UnusedImport

	/* initialization */

	// InvalidInitCycle occurs when an invalid cycle is detected within the initialization graph.
	InvalidInitCycle

	/* decls */

	// DuplicateDecl occurs when an identifier is declared multiple times.
	DuplicateDecl

	// InvalidDeclCycle occurs when a declaration cycle is not valid.
	InvalidDeclCycle

	// InvalidTypeCycle occurs when a cycle in type definitions results in a type that is not well-defined.
	InvalidTypeCycle

	/* decls > const */

	// InvalidConstInit occurs when a const declaration has a non-constant initializer.
	InvalidConstInit

	// InvalidConstVal occurs when a const value cannot be converted to its target type.
	InvalidConstVal

	// InvalidConstType occurs when the underlying type in a const declaration is not a valid constant type.
	InvalidConstType

	/* decls > var (+ other variable assignment codes) */

	// UntypedNilUse occurs when the predeclared.
	UntypedNilUse

	// WrongAssignCount occurs when the number of values.
	WrongAssignCount

	// UnassignableOperand occurs when the left-hand side of an assignment is not assignable.
	UnassignableOperand

	// NoNewVar occurs when a short variable declaration (':=') does not declare new variables.
	NoNewVar

	// MultiValAssignOp occurs when an assignment operation.
	MultiValAssignOp

	// InvalidIfaceAssign occurs when a value of type T is used as an interface.
	InvalidIfaceAssign

	// InvalidChanAssign occurs when a chan assignment is invalid.
	InvalidChanAssign

	// IncompatibleAssign occurs when the type of the right-hand side expression in an assignment cannot be assigned to the type of the variable being assigned.
	IncompatibleAssign

	// UnaddressableFieldAssign occurs when trying to assign to a struct field in a map value.
	UnaddressableFieldAssign

	/* decls > type (+ other type expression codes) */

	// NotAType occurs when the identifier used as the underlying type.
	NotAType

	// InvalidArrayLen occurs when an array length is not a constant value.
	InvalidArrayLen

	// BlankIfaceMethod occurs when a method name is '_'.
	BlankIfaceMethod

	// IncomparableMapKey occurs when a map key type does not support the == and != operators.
	IncomparableMapKey

	// InvalidIfaceEmbed occurs when a non-interface type is embedded in an interface.
	InvalidIfaceEmbed

	// InvalidPtrEmbed occurs when an embedded field is of the pointer form *T, and T itself is itself a pointer, an unsafe.Pointer.
	InvalidPtrEmbed

	/* decls > func and method */

	// BadRecv occurs when a method declaration does not have exactly one receiver parameter.
	BadRecv

	// InvalidRecv occurs when a receiver type expression is not of the form T or *T, or T is a pointer type.
	InvalidRecv

	// DuplicateFieldAndMethod occurs when an identifier appears as both a field and method name.
	DuplicateFieldAndMethod

	// DuplicateMethod occurs when methods on the same receiver type have the same name.
	DuplicateMethod

	/* decls > special */

	// InvalidBlank occurs when a blank identifier is used as a value or type.
	InvalidBlank

	// InvalidIota occurs when the predeclared identifier iota is used outside of a constant declaration.
	InvalidIota

	// MissingInitBody occurs when an init function is missing its body.
	MissingInitBody

	// InvalidInitSig occurs when an init function declares parameters or results.
	InvalidInitSig

	// InvalidInitDecl occurs when init is declared as anything other than a function.
	InvalidInitDecl

	// InvalidMainDecl occurs when main is declared as anything other than a function, in a main package.
	InvalidMainDecl

	/* exprs */

	// TooManyValues occurs when a function returns too many values for the expression context in which it is used.
	TooManyValues

	// NotAnExpr occurs when a type expression is used where a value expression is expected.
	NotAnExpr

	/* exprs > const */

	// TruncatedFloat occurs when a float constant is truncated to an integer value.
	TruncatedFloat

	// NumericOverflow occurs when a numeric constant overflows its target type.
	NumericOverflow

	/* exprs > operation */

	// UndefinedOp occurs when an operator is not defined for the type(s) used in an operation.
	UndefinedOp

	// MismatchedTypes occurs when operand types are incompatible in a binary operation.
	MismatchedTypes

	// DivByZero occurs when a division operation is provable at compile time to be a division by zero.
	DivByZero

	// NonNumericIncDec occurs when an increment or decrement operator is applied to a non-numeric value.
	NonNumericIncDec

	/* exprs > ptr */

	// UnaddressableOperand occurs when the & operator is applied to an unaddressable expression.
	UnaddressableOperand

	// InvalidIndirection occurs when a non-pointer value is indirected via the '*' operator.
	InvalidIndirection

	/* exprs > [] */

	// NonIndexableOperand occurs when an index operation is applied to a value that cannot be indexed.
	NonIndexableOperand

	// InvalidIndex occurs when an index argument is not of integer type, negative, or out-of-bounds.
	InvalidIndex

	// SwappedSliceIndices occurs when constant indices in a slice expression are decreasing in value.
	SwappedSliceIndices

	/* operators > slice */

	// NonSliceableOperand occurs when a slice operation is applied to a value whose type is not sliceable.
	NonSliceableOperand

	// InvalidSliceExpr occurs when a three-index slice expression (a[x:y:z]) is applied to a string.
	InvalidSliceExpr

	/* exprs > shift */

	// InvalidShiftCount occurs when the right-hand side of a shift operation is either non-integer, negative.
	InvalidShiftCount

	// InvalidShiftOperand occurs when the shifted operand is not an integer.
	InvalidShiftOperand

	/* exprs > chan */

	// InvalidReceive occurs when there is a channel receive from a value that is either not a channel.
	InvalidReceive

	// InvalidSend occurs when there is a channel send to a value that is not a channel, or is a receive-only channel.
	InvalidSend

	/* exprs > literal */

	// DuplicateLitKey occurs when an index is duplicated in a slice, array, or map literal.
	DuplicateLitKey

	// MissingLitKey occurs when a map literal is missing a key expression.
	MissingLitKey

	// InvalidLitIndex occurs when the key in a key-value element of a slice or array literal is not an integer constant.
	InvalidLitIndex

	// OversizeArrayLit occurs when an array literal exceeds its length.
	OversizeArrayLit

	// MixedStructLit occurs when a struct literal contains a mix of positional and named elements.
	MixedStructLit

	// InvalidStructLit occurs when a positional struct literal has an incorrect number of values.
	InvalidStructLit

	// MissingLitField occurs when a struct literal refers to a field that does not exist on the struct type.
	MissingLitField

	// DuplicateLitField occurs when a struct literal contains duplicated fields.
	DuplicateLitField

	// UnexportedLitField occurs when a positional struct literal implicitly assigns an unexported field of an imported type.
	UnexportedLitField

	// InvalidLitField occurs when a field name is not a valid identifier.
	InvalidLitField

	// UntypedLit occurs when a composite literal omits a required type identifier.
	UntypedLit

	// InvalidLit occurs when a composite literal expression does not match its type.
	InvalidLit

	/* exprs > selector */

	// AmbiguousSelector occurs when a selector is ambiguous.
	AmbiguousSelector

	// UndeclaredImportedName occurs when a package-qualified identifier is undeclared by the imported package.
	UndeclaredImportedName

	// UnexportedName occurs when a selector refers to an unexported identifier of an imported package.
	UnexportedName

	// UndeclaredName occurs when an identifier is not declared in the current scope.
	UndeclaredName

	// MissingFieldOrMethod occurs when a selector references a field or method that does not exist.
	MissingFieldOrMethod

	/* exprs > ... */

	// BadDotDotDotSyntax occurs when a "..." occurs in a context where it is not valid.
	BadDotDotDotSyntax

	// NonVariadicDotDotDot occurs when a "..." is used on the final argument to a non-variadic function.
	NonVariadicDotDotDot

	// MisplacedDotDotDot occurs when a "..." is used somewhere other than the final argument to a function call.
	MisplacedDotDotDot

	// InvalidDotDotDotOperand occurs when a "..." operator is applied to a single-valued operand.
	InvalidDotDotDotOperand

	// InvalidDotDotDot occurs when a "..." is used in a non-variadic built-in function.
	InvalidDotDotDot

	/* exprs > built-in */

	// UncalledBuiltin occurs when a built-in function is used as a function-valued expression, instead of being called.
	UncalledBuiltin

	// InvalidAppend occurs when append is called with a first argument that is not a slice.
	InvalidAppend

	// InvalidCap occurs when an argument to the cap built-in function is not of supported type.
	InvalidCap

	// InvalidClose occurs when close(...) is called with an argument that is not of channel type.
	InvalidClose

	// InvalidCopy occurs when the arguments are not of slice type or do not have compatible type.
	InvalidCopy

	// InvalidComplex occurs when the complex built-in function is called with arguments with incompatible types.
	InvalidComplex

	// InvalidDelete occurs when the delete built-in function is called with a first argument that is not a map.
	InvalidDelete

	// InvalidImag occurs when the imag built-in function is called with an argument that does not have complex type.
	InvalidImag

	// InvalidLen occurs when an argument to the len built-in function is not of supported type.
	InvalidLen

	// SwappedMakeArgs occurs when make is called with arguments.
	SwappedMakeArgs

	// InvalidMake occurs when make is called with an unsupported type argument.
	InvalidMake

	// InvalidReal occurs when the real built-in function is called with an argument that does not have complex type.
	InvalidReal

	/* exprs > assertion */

	// InvalidAssert occurs when a type assertion is applied to a value that is not of interface type.
	InvalidAssert

	// ImpossibleAssert occurs for a type assertion x.(T).
	ImpossibleAssert

	/* exprs > conversion */

	// InvalidConversion occurs when the argument type cannot be converted to the target.
	InvalidConversion

	// InvalidUntypedConversion occurs when there is no valid implicit conversion from an untyped value satisfying the type constraints of the context in which it is used.
	InvalidUntypedConversion

	/* offsetof */

	// BadOffsetofSyntax occurs when unsafe.Offsetof is called with an argument that is not a selector expression.
	BadOffsetofSyntax

	// InvalidOffsetof occurs when unsafe.Offsetof is called with a method selector, rather than a field selector.
	InvalidOffsetof

	/* control flow > scope */

	// UnusedExpr occurs when a side-effect free expression is used as a statement. Such a statement has no effect.
	UnusedExpr

	// UnusedVar occurs when a variable is declared but unused.
	UnusedVar

	// MissingReturn occurs when a function with results is missing a return statement.
	MissingReturn

	// WrongResultCount occurs when a return statement returns an incorrect number of values.
	WrongResultCount

	// OutOfScopeResult occurs when the name of a value implicitly returned by an empty return statement is shadowed in a nested scope.
	OutOfScopeResult

	// InvalidCond occurs when an if condition is not a boolean expression.
	InvalidCond

	// InvalidPostDecl occurs when there is a declaration in a for-loop post statement.
	InvalidPostDecl

	// InvalidChanRange occurs when a send-only channel used in a range expression.
	InvalidChanRange

	// InvalidIterVar occurs when iteration variables are used while ranging over a channel.
	InvalidIterVar

	// InvalidRangeExpr occurs when the type of a range expression is not array, slice, string, map, or channel.
	InvalidRangeExpr

	/* control flow > switch */

	// MisplacedBreak occurs when a break statement is not within a for, switch.
	MisplacedBreak

	// MisplacedContinue occurs when a continue statement is not within a for loop of the innermost function definition.
	MisplacedContinue

	// MisplacedFallthrough occurs when a fallthrough statement is not within an expression switch.
	MisplacedFallthrough

	// DuplicateCase occurs when a type or expression switch has duplicate cases.
	DuplicateCase

	// DuplicateDefault occurs when a type or expression switch has multiple default clauses.
	DuplicateDefault

	// BadTypeKeyword occurs when a .(type) expression is used anywhere other than a type switch.
	BadTypeKeyword

	// InvalidTypeSwitch occurs when .(type) is used on an expression that is not of interface type.
	InvalidTypeSwitch

	// InvalidExprSwitch occurs when a switch expression is not comparable.
	InvalidExprSwitch

	/* control flow > select */

	// InvalidSelectCase occurs when a select case is not a channel send or receive.
	InvalidSelectCase

	/* control flow > labels and jumps */

	// UndeclaredLabel occurs when an undeclared label is jumped to.
	UndeclaredLabel

	// DuplicateLabel occurs when a label is declared more than once.
	DuplicateLabel

	// MisplacedLabel occurs when a break or continue label is not on a for, switch, or select statement.
	MisplacedLabel

	// UnusedLabel occurs when a label is declared but not used.
	UnusedLabel

	// JumpOverDecl occurs when a label jumps over a variable declaration.
	JumpOverDecl

	// JumpIntoBlock occurs when a forward jump goes to a label inside a nested block.
	JumpIntoBlock

	/* control flow > calls */

	// InvalidMethodExpr occurs when a pointer method is called but the argument is not addressable.
	InvalidMethodExpr

	// WrongArgCount occurs when too few or too many arguments are passed by a function call.
	WrongArgCount

	// InvalidCall occurs when an expression is called that is not of function type.
	InvalidCall

	/* control flow > suspended */

	// UnusedResults occurs when a restricted expression-only built-in function is suspended via go or defer.
	UnusedResults

	// InvalidDefer occurs when a deferred expression is not a function call.
	InvalidDefer

	// InvalidGo occurs when a go expression is not a function call, for example if the expression is a type conversion.
	InvalidGo

	// All codes below.17.

	/* decl */

	// BadDecl occurs when a declaration has invalid syntax.
	BadDecl

	// RepeatedDecl occurs when an identifier occurs more than once on the left hand side of a short variable declaration.
	RepeatedDecl

	/* unsafe */

	// InvalidUnsafeAdd occurs when unsafe.Add is called with a length argument that is not of integer type.
	InvalidUnsafeAdd

	// InvalidUnsafeSlice occurs when unsafe.Slice is called with a pointer argument that is not of pointer type or a length.
	InvalidUnsafeSlice

	// All codes below.18.

	/* features */

	// UnsupportedFeature occurs when a language feature is used that is not supported at this Go version.
	UnsupportedFeature

	/* type params */

	// NotAGenericType occurs when a non-generic type is used where a generic type is expected.
	NotAGenericType

	// WrongTypeArgCount occurs when a type or function is instantiated with an incorrect number of type arguments.
	WrongTypeArgCount

	// CannotInferTypeArgs occurs when type or function type argument inference fails to infer all type arguments.
	CannotInferTypeArgs

	// InvalidTypeArg occurs when a type argument does not satisfy its corresponding type parameter constraints.
	InvalidTypeArg // arguments? InferenceFailed

	// InvalidInstanceCycle occurs when an invalid cycle is detected within the instantiation graph.
	InvalidInstanceCycle

	// InvalidUnion occurs when an embedded union or approximation element is not valid.
	InvalidUnion

	// MisplacedConstraintIface occurs when a constraint-type interface is used outside of constraint position.
	MisplacedConstraintIface

	// InvalidMethodTypeParams occurs when methods have type parameters.
	InvalidMethodTypeParams

	// MisplacedTypeParam occurs when a type parameter is used in a place where it is not permitted.
	MisplacedTypeParam

	// InvalidUnsafeSliceData occurs when unsafe.SliceData is called with an argument that is not of slice type.
	InvalidUnsafeSliceData

	// InvalidUnsafeString occurs when unsafe.String is called with a length argument that is not of integer type, negative.
	InvalidUnsafeString

	// InvalidUnsafeStringData occurs if it is used in a package compiled for a language version before go1.20.
	_ // not used anymore

)
