/*
 Copyright 2021 The GoPlus Authors (goplus.org)
 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at
     http://www.apache.org/licenses/LICENSE-2.0
 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package gox

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"log"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/goplus/gox/internal"
)

func getSrc(node []ast.Node) ast.Node {
	if node != nil {
		return node[0]
	}
	return nil
}

// ----------------------------------------------------------------------------

type codeBlock interface {
	End(cb *CodeBuilder)
}

type codeBlockCtx struct {
	codeBlock
	scope *types.Scope
	base  int
	stmts []ast.Stmt
	label *ast.LabeledStmt
	flows int // flow flags
}

const (
	flowFlagBreak = 1 << iota
	flowFlagContinue
	flowFlagReturn
	flowFlagGoto
	flowFlagWithLabel
)

type Label struct {
	types.Label
	used bool
}

type funcBodyCtx struct {
	codeBlockCtx
	fn     *Func
	labels map[string]*Label
}

func (p *funcBodyCtx) checkLabels(cb *CodeBuilder) {
	for name, l := range p.labels {
		if !l.used {
			cb.handleErr(cb.newCodePosErrorf(l.Pos(), "label %s defined and not used", name))
		}
	}
}

type CodeError struct {
	Msg   string
	Pos   *token.Position
	Scope *types.Scope
	Func  *Func
}

func (p *CodeError) Error() string {
	if p.Pos != nil {
		return fmt.Sprintf("%v: %s", *p.Pos, p.Msg)
	}
	return p.Msg
}

// CodeBuilder type
type CodeBuilder struct {
	stk       internal.Stack
	current   funcBodyCtx
	comments  *ast.CommentGroup
	pkg       *Package
	varDecl   *VarDecl
	interp    NodeInterpreter
	loadNamed LoadNamedFunc
	handleErr func(err error)
	closureParamInsts
	iotav       int
	commentOnce bool
}

func (p *CodeBuilder) init(pkg *Package) {
	conf := pkg.conf
	p.pkg = pkg
	p.handleErr = conf.HandleErr
	if p.handleErr == nil {
		p.handleErr = defaultHandleErr
	}
	p.interp = conf.NodeInterpreter
	if p.interp == nil {
		p.interp = nodeInterp{}
	}
	p.loadNamed = conf.LoadNamed
	if p.loadNamed == nil {
		p.loadNamed = defaultLoadNamed
	}
	p.current.scope = pkg.Types.Scope()
	p.stk.Init()
	p.closureParamInsts.init()
}

func defaultLoadNamed(at *Package, t *types.Named) {
	// no delay-loaded named types
}

func defaultHandleErr(err error) {
	panic(err)
}

type nodeInterp struct{}

func (p nodeInterp) Position(pos token.Pos) (ret token.Position) {
	return
}

func (p nodeInterp) Caller(expr ast.Node) string {
	return "the function call"
}

func (p nodeInterp) LoadExpr(expr ast.Node) (src string, pos token.Position) {
	return
}

func (p *CodeBuilder) position(pos token.Pos) (ret token.Position) {
	return p.interp.Position(pos)
}

func (p *CodeBuilder) nodePosition(expr ast.Node) (ret token.Position) {
	if expr == nil {
		return
	}
	_, ret = p.interp.LoadExpr(expr) // TODO: optimize
	return
}

func (p *CodeBuilder) getCaller(expr ast.Node) string {
	if expr == nil {
		return ""
	}
	return p.interp.Caller(expr)
}

func (p *CodeBuilder) loadExpr(expr ast.Node) (src string, pos token.Position) {
	if expr == nil {
		return
	}
	return p.interp.LoadExpr(expr)
}

func (p *CodeBuilder) newCodeError(pos *token.Position, msg string) *CodeError {
	return &CodeError{Msg: msg, Pos: pos, Scope: p.Scope(), Func: p.Func()}
}

func (p *CodeBuilder) newCodePosError(pos token.Pos, msg string) *CodeError {
	tpos := p.position(pos)
	return &CodeError{Msg: msg, Pos: &tpos, Scope: p.Scope(), Func: p.Func()}
}

func (p *CodeBuilder) newCodePosErrorf(pos token.Pos, format string, args ...interface{}) *CodeError {
	return p.newCodePosError(pos, fmt.Sprintf(format, args...))
}

func (p *CodeBuilder) panicCodeError(pos *token.Position, msg string) {
	panic(p.newCodeError(pos, msg))
}

func (p *CodeBuilder) panicCodePosError(pos token.Pos, msg string) {
	panic(p.newCodePosError(pos, msg))
}

func (p *CodeBuilder) panicCodeErrorf(pos *token.Position, format string, args ...interface{}) {
	panic(p.newCodeError(pos, fmt.Sprintf(format, args...)))
}

func (p *CodeBuilder) panicCodePosErrorf(pos token.Pos, format string, args ...interface{}) {
	panic(p.newCodePosError(pos, fmt.Sprintf(format, args...)))
}

// Scope returns current scope.
func (p *CodeBuilder) Scope() *types.Scope {
	return p.current.scope
}

// Func returns current func (nil means in global scope).
func (p *CodeBuilder) Func() *Func {
	return p.current.fn
}

// Pkg returns the package instance.
func (p *CodeBuilder) Pkg() *Package {
	return p.pkg
}

func (p *CodeBuilder) startFuncBody(fn *Func, old *funcBodyCtx) *CodeBuilder {
	p.current.fn, old.fn = fn, p.current.fn
	p.startBlockStmt(fn, "func "+fn.Name(), &old.codeBlockCtx)
	scope := p.current.scope
	sig := fn.Type().(*types.Signature)
	insertParams(scope, sig.Params())
	insertParams(scope, sig.Results())
	if recv := sig.Recv(); recv != nil {
		scope.Insert(recv)
	}
	return p
}

func insertParams(scope *types.Scope, params *types.Tuple) {
	for i, n := 0, params.Len(); i < n; i++ {
		v := params.At(i)
		if name := v.Name(); name != "" {
			scope.Insert(v)
		}
	}
}

func (p *CodeBuilder) endFuncBody(old funcBodyCtx) []ast.Stmt {
	p.current.checkLabels(p)
	p.current.fn = old.fn
	stmts, _ := p.endBlockStmt(&old.codeBlockCtx)
	return stmts
}

func (p *CodeBuilder) startBlockStmt(current codeBlock, comment string, old *codeBlockCtx) *CodeBuilder {
	scope := types.NewScope(p.current.scope, token.NoPos, token.NoPos, comment)
	p.current.codeBlockCtx, *old = codeBlockCtx{current, scope, p.stk.Len(), nil, nil, 0}, p.current.codeBlockCtx
	return p
}

func (p *CodeBuilder) endBlockStmt(old *codeBlockCtx) ([]ast.Stmt, int) {
	flows := p.current.flows
	if p.current.label != nil {
		p.emitStmt(&ast.EmptyStmt{})
	}
	stmts := p.current.stmts
	p.stk.SetLen(p.current.base)
	p.current.codeBlockCtx = *old
	return stmts, flows
}

func (p *CodeBuilder) clearBlockStmt() []ast.Stmt {
	stmts := p.current.stmts
	p.current.stmts = nil
	return stmts
}

func (p *CodeBuilder) popStmt() ast.Stmt {
	stmts := p.current.stmts
	n := len(stmts) - 1
	stmt := stmts[n]
	p.current.stmts = stmts[:n]
	return stmt
}

func (p *CodeBuilder) startStmtAt(stmt ast.Stmt) int {
	idx := len(p.current.stmts)
	p.emitStmt(stmt)
	return idx
}

// Usage:
//   idx := cb.startStmtAt(stmt)
//   ...
//   cb.commitStmt(idx)
func (p *CodeBuilder) commitStmt(idx int) {
	stmts := p.current.stmts
	n := len(stmts) - 1
	if n > idx {
		stmt := stmts[idx]
		copy(stmts[idx:], stmts[idx+1:])
		stmts[n] = stmt
	}
}

func (p *CodeBuilder) emitStmt(stmt ast.Stmt) {
	if p.comments != nil {
		p.pkg.setStmtComments(stmt, p.comments)
		if p.commentOnce {
			p.comments = nil
		}
	}
	if p.current.label != nil {
		p.current.label.Stmt = stmt
		stmt, p.current.label = p.current.label, nil
	}
	p.current.stmts = append(p.current.stmts, stmt)
}

func (p *CodeBuilder) startInitExpr(current codeBlock) (old codeBlock) {
	p.current.codeBlock, old = current, p.current.codeBlock
	return
}

func (p *CodeBuilder) endInitExpr(old codeBlock) {
	p.current.codeBlock = old
}

// Comments returns the comments of next statement.
func (p *CodeBuilder) Comments() *ast.CommentGroup {
	return p.comments
}

func (p *CodeBuilder) BackupComments() (*ast.CommentGroup, bool) {
	return p.comments, p.commentOnce
}

// SetComments sets comments to next statement.
func (p *CodeBuilder) SetComments(comments *ast.CommentGroup, once bool) *CodeBuilder {
	if debugComments && comments != nil {
		for i, c := range comments.List {
			log.Println("SetComments", i, c.Text)
		}
	}
	p.comments, p.commentOnce = comments, once
	return p
}

// ReturnErr func
func (p *CodeBuilder) ReturnErr(outer bool) *CodeBuilder {
	if debugInstr {
		log.Println("ReturnErr", outer)
	}
	fn := p.current.fn
	if outer {
		if !fn.isInline() {
			panic("only support ReturnOuterErr in an inline call")
		}
		fn = fn.old.fn
	}
	results := fn.Type().(*types.Signature).Results()
	n := results.Len()
	if n > 0 {
		last := results.At(n - 1)
		if last.Type() == TyError { // last result is error
			err := p.stk.Pop()
			for i := 0; i < n-1; i++ {
				p.doZeroLit(results.At(i).Type(), false)
			}
			p.stk.Push(err)
			p.returnResults(n)
			p.current.flows |= flowFlagReturn
			return p
		}
	}
	panic("TODO: last result type isn't an error")
}

func (p *CodeBuilder) returnResults(n int) {
	var rets []ast.Expr
	if n > 0 {
		args := p.stk.GetArgs(n)
		rets = make([]ast.Expr, n)
		for i := 0; i < n; i++ {
			rets[i] = args[i].Val
		}
		p.stk.PopN(n)
	}
	p.emitStmt(&ast.ReturnStmt{Results: rets})
}

// Return func
func (p *CodeBuilder) Return(n int, src ...ast.Node) *CodeBuilder {
	if debugInstr {
		log.Println("Return", n)
	}
	fn := p.current.fn
	results := fn.Type().(*types.Signature).Results()
	checkFuncResults(p.pkg, p.stk.GetArgs(n), results, getSrc(src))
	if fn.isInline() {
		for i := n - 1; i >= 0; i-- {
			key := closureParamInst{fn, results.At(i)}
			elem := p.stk.Pop()
			p.doVarRef(p.paramInsts[key], nil, false)
			p.stk.Push(elem)
			p.doAssignWith(1, 1, nil)
		}
		p.Goto(p.getEndingLabel(fn))
	} else {
		p.current.flows |= flowFlagReturn
		p.returnResults(n)
	}
	return p
}

// Call func
func (p *CodeBuilder) Call(n int, ellipsis ...bool) *CodeBuilder {
	return p.CallWith(n, ellipsis != nil && ellipsis[0])
}

// CallWith func
func (p *CodeBuilder) CallWith(n int, ellipsis bool, src ...ast.Node) *CodeBuilder {
	fn := p.stk.Get(-(n + 1))
	if t, ok := fn.Type.(*btiMethodType); ok {
		n++
		fn.Type = t.Type
		fn = p.stk.Get(-(n + 1))
		if t.eargs != nil {
			for _, arg := range t.eargs {
				p.Val(arg)
			}
			n += len(t.eargs)
		}
	}
	args := p.stk.GetArgs(n)
	var flags InstrFlags
	if ellipsis {
		flags = InstrFlagEllipsis
	}
	if debugInstr {
		log.Println("Call", n, int(flags))
	}
	s := getSrc(src)
	fn.Src = s
	ret := toFuncCall(p.pkg, fn, args, flags)
	ret.Src = s
	p.stk.Ret(n+1, ret)
	return p
}

type closureParamInst struct {
	inst  *Func
	param *types.Var
}

type closureParamInsts struct {
	paramInsts map[closureParamInst]*types.Var
}

func (p *closureParamInsts) init() {
	p.paramInsts = make(map[closureParamInst]*types.Var)
}

func (p *CodeBuilder) getEndingLabel(fn *Func) *Label {
	key := closureParamInst{fn, nil}
	if v, ok := p.paramInsts[key]; ok {
		return p.current.labels[v.Name()]
	}
	ending := p.pkg.autoName()
	p.paramInsts[key] = types.NewParam(token.NoPos, nil, ending, nil)
	return p.NewLabel(token.NoPos, ending)
}

func (p *CodeBuilder) needEndingLabel(fn *Func) (*Label, bool) {
	key := closureParamInst{fn, nil}
	if v, ok := p.paramInsts[key]; ok {
		return p.current.labels[v.Name()], true
	}
	return nil, false
}

func (p *Func) inlineClosureEnd(cb *CodeBuilder) {
	if ending, ok := cb.needEndingLabel(p); ok {
		cb.Label(ending)
	}
	sig := p.Type().(*types.Signature)
	cb.emitStmt(&ast.BlockStmt{List: cb.endFuncBody(p.old)})
	cb.stk.PopN(p.getInlineCallArity())
	results := sig.Results()
	for i, n := 0, results.Len(); i < n; i++ { // return results & clean env
		key := closureParamInst{p, results.At(i)}
		cb.pushVal(cb.paramInsts[key], nil)
		delete(cb.paramInsts, key)
	}
	for i, n := 0, getParamLen(sig); i < n; i++ { // clean env
		key := closureParamInst{p, getParam(sig, i)}
		delete(cb.paramInsts, key)
	}
}

func (p *Func) getInlineCallArity() int {
	return int(p.Pos() &^ closureFlagInline)
}

func makeInlineCall(arity int) closureType {
	return closureFlagInline | closureType(arity)
}

// CallInlineClosureStart func
func (p *CodeBuilder) CallInlineClosureStart(sig *types.Signature, arity int, ellipsis bool) *CodeBuilder {
	if debugInstr {
		log.Println("CallInlineClosureStart", arity, ellipsis)
	}
	pkg := p.pkg
	closure := pkg.newClosure(sig, makeInlineCall(arity))
	results := sig.Results()
	for i, n := 0, results.Len(); i < n; i++ {
		p.emitVar(pkg, closure, results.At(i), false)
	}
	p.startFuncBody(closure, &closure.old)
	args := p.stk.GetArgs(arity)
	var flags InstrFlags
	if ellipsis {
		flags = InstrFlagEllipsis
	}
	if err := matchFuncType(pkg, args, flags, sig, nil); err != nil {
		panic(err)
	}
	n1 := getParamLen(sig) - 1
	if sig.Variadic() && !ellipsis {
		p.SliceLit(getParam(sig, n1).Type().(*types.Slice), arity-n1)
	}
	for i := n1; i >= 0; i-- {
		p.emitVar(pkg, closure, getParam(sig, i), true)
	}
	return p
}

func (p *CodeBuilder) emitVar(pkg *Package, closure *Func, param *types.Var, withInit bool) {
	name := pkg.autoName()
	if withInit {
		p.NewVarStart(param.Type(), name).EndInit(1)
	} else {
		p.NewVar(param.Type(), name)
	}
	key := closureParamInst{closure, param}
	p.paramInsts[key] = p.current.scope.Lookup(name).(*types.Var)
}

// NewClosure func
func (p *CodeBuilder) NewClosure(params, results *Tuple, variadic bool) *Func {
	sig := types.NewSignature(nil, params, results, variadic)
	return p.NewClosureWith(sig)
}

// NewClosureWith func
func (p *CodeBuilder) NewClosureWith(sig *types.Signature) *Func {
	if debugInstr {
		t := sig.Params()
		for i, n := 0, t.Len(); i < n; i++ {
			v := t.At(i)
			if _, ok := v.Type().(*unboundType); ok {
				panic("can't use unbound type in func parameters")
			}
		}
	}
	return p.pkg.newClosure(sig, closureNormal)
}

// NewType func
func (p *CodeBuilder) NewType(name string, pos ...token.Pos) *TypeDecl {
	if debugInstr {
		log.Println("NewType", name)
	}
	return p.pkg.doNewType(p.current.scope, getPos(pos), name, nil, 0)
}

// AliasType func
func (p *CodeBuilder) AliasType(name string, typ types.Type, pos ...token.Pos) *types.Named {
	if debugInstr {
		log.Println("AliasType", name, typ)
	}
	decl := p.pkg.doNewType(p.current.scope, getPos(pos), name, typ, 1)
	return decl.typ
}

// NewConstStart func
func (p *CodeBuilder) NewConstStart(typ types.Type, names ...string) *CodeBuilder {
	if debugInstr {
		log.Println("NewConstStart", names)
	}
	return p.pkg.newValueDecl(nil, p.current.scope, token.NoPos, token.CONST, typ, names...).InitStart(p.pkg)
}

// NewVar func
func (p *CodeBuilder) NewVar(typ types.Type, names ...string) *CodeBuilder {
	if debugInstr {
		log.Println("NewVar", names)
	}
	p.pkg.newValueDecl(nil, p.current.scope, token.NoPos, token.VAR, typ, names...)
	return p
}

// NewVarStart func
func (p *CodeBuilder) NewVarStart(typ types.Type, names ...string) *CodeBuilder {
	if debugInstr {
		log.Println("NewVarStart", names)
	}
	return p.pkg.newValueDecl(nil, p.current.scope, token.NoPos, token.VAR, typ, names...).InitStart(p.pkg)
}

// DefineVarStart func
func (p *CodeBuilder) DefineVarStart(pos token.Pos, names ...string) *CodeBuilder {
	if debugInstr {
		log.Println("DefineVarStart", names)
	}
	return p.pkg.newValueDecl(nil, p.current.scope, pos, token.DEFINE, nil, names...).InitStart(p.pkg)
}

// NewAutoVar func
func (p *CodeBuilder) NewAutoVar(pos token.Pos, name string, pv **types.Var) *CodeBuilder {
	spec := &ast.ValueSpec{Names: []*ast.Ident{ident(name)}}
	decl := &ast.GenDecl{Tok: token.VAR, Specs: []ast.Spec{spec}}
	stmt := &ast.DeclStmt{
		Decl: decl,
	}
	if debugInstr {
		log.Println("NewAutoVar", name)
	}
	p.emitStmt(stmt)
	typ := &unboundType{ptypes: []*ast.Expr{&spec.Type}}
	*pv = types.NewVar(pos, p.pkg.Types, name, typ)
	if old := p.current.scope.Insert(*pv); old != nil {
		oldPos := p.position(old.Pos())
		p.panicCodePosErrorf(
			pos, "%s redeclared in this block\n\tprevious declaration at %v", name, oldPos)
	}
	return p
}

// VarRef func: p.VarRef(nil) means underscore (_)
func (p *CodeBuilder) VarRef(ref interface{}, src ...ast.Node) *CodeBuilder {
	return p.doVarRef(ref, getSrc(src), true)
}

func (p *CodeBuilder) doVarRef(ref interface{}, src ast.Node, allowDebug bool) *CodeBuilder {
	if ref == nil {
		if allowDebug && debugInstr {
			log.Println("VarRef _")
		}
		p.stk.Push(&internal.Elem{
			Val: underscore, // _
		})
	} else {
		switch v := ref.(type) {
		case *types.Var:
			if allowDebug && debugInstr {
				log.Println("VarRef", v.Name(), v.Type())
			}
			fn := p.current.fn
			if fn != nil && fn.isInline() { // is in an inline call
				key := closureParamInst{fn, v}
				if arg, ok := p.paramInsts[key]; ok { // replace param with arg
					v = arg
				}
			}
			p.stk.Push(&internal.Elem{
				Val: toObjectExpr(p.pkg, v), Type: &refType{typ: v.Type()}, Src: src,
			})
		default:
			code, pos := p.loadExpr(src)
			p.panicCodeErrorf(&pos, "%s is not a variable", code)
		}
	}
	return p
}

var (
	elemNone = &internal.Elem{}
)

// None func
func (p *CodeBuilder) None() *CodeBuilder {
	if debugInstr {
		log.Println("None")
	}
	p.stk.Push(elemNone)
	return p
}

// ZeroLit func
func (p *CodeBuilder) ZeroLit(typ types.Type) *CodeBuilder {
	return p.doZeroLit(typ, true)
}

func (p *CodeBuilder) doZeroLit(typ types.Type, allowDebug bool) *CodeBuilder {
	typ0 := typ
	if allowDebug && debugInstr {
		log.Println("ZeroLit")
	}
retry:
	switch t := typ.(type) {
	case *types.Basic:
		switch kind := t.Kind(); kind {
		case types.Bool:
			return p.Val(false)
		case types.String:
			return p.Val("")
		case types.UnsafePointer:
			return p.Val(nil)
		default:
			return p.Val(0)
		}
	case *types.Interface:
		return p.Val(nil)
	case *types.Map:
		return p.Val(nil)
	case *types.Slice:
		return p.Val(nil)
	case *types.Pointer:
		return p.Val(nil)
	case *types.Chan:
		return p.Val(nil)
	case *types.Named:
		typ = p.getUnderlying(t)
		goto retry
	}
	ret := &ast.CompositeLit{}
	switch t := typ.(type) {
	case *unboundType:
		if t.tBound == nil {
			t.ptypes = append(t.ptypes, &ret.Type)
		} else {
			typ = t.tBound
			typ0 = typ
			ret.Type = toType(p.pkg, typ)
		}
	default:
		ret.Type = toType(p.pkg, typ)
	}
	p.stk.Push(&internal.Elem{Type: typ0, Val: ret})
	return p
}

// MapLit func
func (p *CodeBuilder) MapLit(typ types.Type, arity int) *CodeBuilder {
	if debugInstr {
		log.Println("MapLit", typ, arity)
	}
	var t *types.Map
	var typExpr ast.Expr
	var pkg = p.pkg
	if typ != nil {
		switch tt := typ.(type) {
		case *types.Named:
			typExpr = toNamedType(pkg, tt)
			t = p.getUnderlying(tt).(*types.Map)
		case *types.Map:
			typExpr = toMapType(pkg, tt)
			t = tt
		default:
			log.Panicln("MapLit: typ isn't a map type -", reflect.TypeOf(typ))
		}
	}
	if arity == 0 {
		if t == nil {
			t = types.NewMap(types.Typ[types.String], TyEmptyInterface)
			typ = t
			typExpr = toMapType(pkg, t)
		}
		ret := &ast.CompositeLit{Type: typExpr}
		p.stk.Push(&internal.Elem{Type: typ, Val: ret})
		return p
	}
	if (arity & 1) != 0 {
		log.Panicln("MapLit: invalid arity, can't be odd -", arity)
	}
	var key, val types.Type
	var args = p.stk.GetArgs(arity)
	var check = (t != nil)
	if check {
		key, val = t.Key(), t.Elem()
	} else {
		key = boundElementType(pkg, args, 0, arity, 2)
		val = boundElementType(pkg, args, 1, arity, 2)
		t = types.NewMap(Default(pkg, key), Default(pkg, val))
		typ = t
		typExpr = toMapType(pkg, t)
	}
	elts := make([]ast.Expr, arity>>1)
	for i := 0; i < arity; i += 2 {
		elts[i>>1] = &ast.KeyValueExpr{Key: args[i].Val, Value: args[i+1].Val}
		if check {
			if !AssignableTo(pkg, args[i].Type, key) {
				src, pos := p.loadExpr(args[i].Src)
				p.panicCodeErrorf(
					&pos, "cannot use %s (type %v) as type %v in map key", src, args[i].Type, key)
			} else if !AssignableTo(pkg, args[i+1].Type, val) {
				src, pos := p.loadExpr(args[i+1].Src)
				p.panicCodeErrorf(
					&pos, "cannot use %s (type %v) as type %v in map value", src, args[i+1].Type, val)
			}
		}
	}
	p.stk.Ret(arity, &internal.Elem{Type: typ, Val: &ast.CompositeLit{Type: typExpr, Elts: elts}})
	return p
}

func (p *CodeBuilder) toBoundArrayLen(elts []*internal.Elem, arity, limit int) int {
	n := -1
	max := -1
	for i := 0; i < arity; i += 2 {
		if elts[i].Val != nil {
			n = p.toIntVal(elts[i], "index which must be non-negative integer constant")
		} else {
			n++
		}
		if limit >= 0 && n >= limit { // error message
			if elts[i].Src == nil {
				_, pos := p.loadExpr(elts[i+1].Src)
				p.panicCodeErrorf(&pos, "array index %d out of bounds [0:%d]", n, limit)
			}
			src, pos := p.loadExpr(elts[i].Src)
			p.panicCodeErrorf(&pos, "array index %s (value %d) out of bounds [0:%d]", src, n, limit)
		}
		if max < n {
			max = n
		}
	}
	return max + 1
}

func (p *CodeBuilder) toIntVal(v *internal.Elem, msg string) int {
	if cval := v.CVal; cval != nil && cval.Kind() == constant.Int {
		if v, ok := constant.Int64Val(cval); ok {
			return int(v)
		}
	}
	code, pos := p.loadExpr(v.Src)
	p.panicCodeErrorf(&pos, "cannot use %s as %s", code, msg)
	return 0
}

func (p *CodeBuilder) indexElemExpr(args []*internal.Elem, i int) ast.Expr {
	key := args[i].Val
	if key == nil { // none
		return args[i+1].Val
	}
	p.toIntVal(args[i], "index which must be non-negative integer constant")
	return &ast.KeyValueExpr{Key: key, Value: args[i+1].Val}
}

// SliceLit func
func (p *CodeBuilder) SliceLit(typ types.Type, arity int, keyVal ...bool) *CodeBuilder {
	var elts []ast.Expr
	var keyValMode = (keyVal != nil && keyVal[0])
	if debugInstr {
		log.Println("SliceLit", typ, arity, keyValMode)
	}
	var t *types.Slice
	var typExpr ast.Expr
	var pkg = p.pkg
	if typ != nil {
		switch tt := typ.(type) {
		case *types.Named:
			typExpr = toNamedType(pkg, tt)
			t = p.getUnderlying(tt).(*types.Slice)
		case *types.Slice:
			typExpr = toSliceType(pkg, tt)
			t = tt
		default:
			log.Panicln("SliceLit: typ isn't a slice type -", reflect.TypeOf(typ))
		}
	}
	if keyValMode { // in keyVal mode
		if (arity & 1) != 0 {
			log.Panicln("SliceLit: invalid arity, can't be odd in keyVal mode -", arity)
		}
		args := p.stk.GetArgs(arity)
		val := t.Elem()
		n := arity >> 1
		elts = make([]ast.Expr, n)
		for i := 0; i < arity; i += 2 {
			arg := args[i+1]
			if !AssignableConv(pkg, arg.Type, val, arg) {
				src, pos := p.loadExpr(args[i+1].Src)
				p.panicCodeErrorf(
					&pos, "cannot use %s (type %v) as type %v in slice literal", src, args[i+1].Type, val)
			}
			elts[i>>1] = p.indexElemExpr(args, i)
		}
	} else {
		if arity == 0 {
			if t == nil {
				t = types.NewSlice(TyEmptyInterface)
				typ = t
				typExpr = toSliceType(pkg, t)
			}
			p.stk.Push(&internal.Elem{Type: typ, Val: &ast.CompositeLit{Type: typExpr}})
			return p
		}
		var val types.Type
		var args = p.stk.GetArgs(arity)
		var check = (t != nil)
		if check {
			val = t.Elem()
		} else {
			val = boundElementType(pkg, args, 0, arity, 1)
			t = types.NewSlice(Default(pkg, val))
			typ = t
			typExpr = toSliceType(pkg, t)
		}
		elts = make([]ast.Expr, arity)
		for i, arg := range args {
			elts[i] = arg.Val
			if check {
				if !AssignableConv(pkg, arg.Type, val, arg) {
					src, pos := p.loadExpr(arg.Src)
					p.panicCodeErrorf(
						&pos, "cannot use %s (type %v) as type %v in slice literal", src, arg.Type, val)
				}
			}
		}
	}
	p.stk.Ret(arity, &internal.Elem{Type: typ, Val: &ast.CompositeLit{Type: typExpr, Elts: elts}})
	return p
}

// ArrayLit func
func (p *CodeBuilder) ArrayLit(typ types.Type, arity int, keyVal ...bool) *CodeBuilder {
	var elts []ast.Expr
	var keyValMode = (keyVal != nil && keyVal[0])
	if debugInstr {
		log.Println("ArrayLit", typ, arity, keyValMode)
	}
	var t *types.Array
	var typExpr ast.Expr
	var pkg = p.pkg
	switch tt := typ.(type) {
	case *types.Named:
		typExpr = toNamedType(pkg, tt)
		t = p.getUnderlying(tt).(*types.Array)
	case *types.Array:
		typExpr = toArrayType(pkg, tt)
		t = tt
	default:
		log.Panicln("ArrayLit: typ isn't a array type -", reflect.TypeOf(typ))
	}
	if keyValMode { // in keyVal mode
		if (arity & 1) != 0 {
			log.Panicln("ArrayLit: invalid arity, can't be odd in keyVal mode -", arity)
		}
		n := int(t.Len())
		args := p.stk.GetArgs(arity)
		max := p.toBoundArrayLen(args, arity, n)
		val := t.Elem()
		if n < 0 {
			t = types.NewArray(val, int64(max))
			typ = t
		}
		elts = make([]ast.Expr, arity>>1)
		for i := 0; i < arity; i += 2 {
			if !AssignableTo(pkg, args[i+1].Type, val) {
				src, pos := p.loadExpr(args[i+1].Src)
				p.panicCodeErrorf(
					&pos, "cannot use %s (type %v) as type %v in array literal", src, args[i+1].Type, val)
			}
			elts[i>>1] = p.indexElemExpr(args, i)
		}
	} else {
		args := p.stk.GetArgs(arity)
		val := t.Elem()
		if n := t.Len(); n < 0 {
			t = types.NewArray(val, int64(arity))
			typ = t
		} else if int(n) < arity {
			_, pos := p.loadExpr(args[n].Src)
			p.panicCodeErrorf(&pos, "array index %d out of bounds [0:%d]", n, n)
		}
		elts = make([]ast.Expr, arity)
		for i, arg := range args {
			elts[i] = arg.Val
			if !AssignableTo(pkg, arg.Type, val) {
				src, pos := p.loadExpr(arg.Src)
				p.panicCodeErrorf(
					&pos, "cannot use %s (type %v) as type %v in array literal", src, arg.Type, val)
			}
		}
	}
	p.stk.Ret(arity, &internal.Elem{Type: typ, Val: &ast.CompositeLit{Type: typExpr, Elts: elts}})
	return p
}

// StructLit func
func (p *CodeBuilder) StructLit(typ types.Type, arity int, keyVal bool) *CodeBuilder {
	if debugInstr {
		log.Println("StructLit", typ, arity, keyVal)
	}
	var t *types.Struct
	var typExpr ast.Expr
	var pkg = p.pkg
	switch tt := typ.(type) {
	case *types.Named:
		typExpr = toNamedType(pkg, tt)
		t = p.getUnderlying(tt).(*types.Struct)
	case *types.Struct:
		typExpr = toStructType(pkg, tt)
		t = tt
	default:
		log.Panicln("StructLit: typ isn't a struct type -", reflect.TypeOf(typ))
	}
	var elts []ast.Expr
	var n = t.NumFields()
	var args = p.stk.GetArgs(arity)
	if keyVal {
		if (arity & 1) != 0 {
			log.Panicln("StructLit: invalid arity, can't be odd in keyVal mode -", arity)
		}
		elts = make([]ast.Expr, arity>>1)
		for i := 0; i < arity; i += 2 {
			idx := p.toIntVal(args[i], "field which must be non-negative integer constant")
			if idx >= n {
				panic("invalid struct field index")
			}
			elt := t.Field(idx)
			eltTy, eltName := elt.Type(), elt.Name()
			if !AssignableTo(pkg, args[i+1].Type, eltTy) {
				src, pos := p.loadExpr(args[i+1].Src)
				p.panicCodeErrorf(
					&pos, "cannot use %s (type %v) as type %v in value of field %s",
					src, args[i+1].Type, eltTy, eltName)
			}
			elts[i>>1] = &ast.KeyValueExpr{Key: ident(eltName), Value: args[i+1].Val}
		}
	} else if arity != n {
		if arity != 0 {
			fewOrMany := "few"
			if arity > n {
				fewOrMany = "many"
			}
			_, pos := p.loadExpr(args[arity-1].Src)
			p.panicCodeErrorf(&pos, "too %s values in %v{...}", fewOrMany, typ)
		}
	} else {
		elts = make([]ast.Expr, arity)
		for i, arg := range args {
			elts[i] = arg.Val
			eltTy := t.Field(i).Type()
			if !AssignableTo(pkg, arg.Type, eltTy) {
				src, pos := p.loadExpr(arg.Src)
				p.panicCodeErrorf(
					&pos, "cannot use %s (type %v) as type %v in value of field %s",
					src, arg.Type, eltTy, t.Field(i).Name())
			}
		}
	}
	p.stk.Ret(arity, &internal.Elem{Type: typ, Val: &ast.CompositeLit{Type: typExpr, Elts: elts}})
	return p
}

// Slice func
func (p *CodeBuilder) Slice(slice3 bool, src ...ast.Node) *CodeBuilder { // a[i:j:k]
	if debugInstr {
		log.Println("Slice", slice3)
	}
	n := 3
	if slice3 {
		n++
	}
	srcExpr := getSrc(src)
	args := p.stk.GetArgs(n)
	x := args[0]
	typ := x.Type
	switch t := typ.(type) {
	case *types.Slice:
		// nothing to do
	case *types.Basic:
		if t.Kind() == types.String || t.Kind() == types.UntypedString {
			if slice3 {
				code, pos := p.loadExpr(srcExpr)
				p.panicCodeErrorf(&pos, "invalid operation %s (3-index slice of string)", code)
			}
		} else {
			code, pos := p.loadExpr(x.Src)
			p.panicCodeErrorf(&pos, "cannot slice %s (type %v)", code, typ)
		}
	case *types.Array:
		typ = types.NewSlice(t.Elem())
	case *types.Pointer:
		if tt, ok := t.Elem().(*types.Array); ok {
			typ = types.NewSlice(tt.Elem())
		} else {
			code, pos := p.loadExpr(x.Src)
			p.panicCodeErrorf(&pos, "cannot slice %s (type %v)", code, typ)
		}
	}
	var exprMax ast.Expr
	if slice3 {
		exprMax = args[3].Val
	}
	// TODO: check type
	elem := &internal.Elem{
		Val: &ast.SliceExpr{
			X: x.Val, Low: args[1].Val, High: args[2].Val, Max: exprMax, Slice3: slice3,
		},
		Type: typ, Src: srcExpr,
	}
	p.stk.Ret(n, elem)
	return p
}

// Index func
func (p *CodeBuilder) Index(nidx int, twoValue bool, src ...ast.Node) *CodeBuilder {
	if debugInstr {
		log.Println("Index", nidx, twoValue)
	}
	if nidx != 1 {
		panic("Index doesn't support a[i, j...] yet")
	}
	args := p.stk.GetArgs(2)
	srcExpr := getSrc(src)
	typs, allowTwoValue := p.getIdxValTypes(args[0].Type, false, srcExpr)
	var tyRet types.Type
	if twoValue { // elem, ok = a[key]
		if !allowTwoValue {
			_, pos := p.loadExpr(srcExpr)
			p.panicCodeError(&pos, "assignment mismatch: 2 variables but 1 values")
		}
		pkg := p.pkg
		tyRet = types.NewTuple(
			pkg.NewParam(token.NoPos, "", typs[1]),
			pkg.NewParam(token.NoPos, "", types.Typ[types.Bool]))
	} else { // elem = a[key]
		tyRet = typs[1]
	}
	elem := &internal.Elem{
		Val: &ast.IndexExpr{X: args[0].Val, Index: args[1].Val}, Type: tyRet, Src: srcExpr,
	}
	// TODO: check index type
	p.stk.Ret(2, elem)
	return p
}

// IndexRef func
func (p *CodeBuilder) IndexRef(nidx int, src ...ast.Node) *CodeBuilder {
	if debugInstr {
		log.Println("IndexRef", nidx)
	}
	if nidx != 1 {
		panic("IndexRef doesn't support a[i, j...] = val yet")
	}
	args := p.stk.GetArgs(2)
	typ := args[0].Type
	elemRef := &internal.Elem{
		Val: &ast.IndexExpr{X: args[0].Val, Index: args[1].Val},
		Src: getSrc(src),
	}
	if t, ok := typ.(*unboundType); ok {
		tyMapElem := &unboundMapElemType{key: args[1].Type, typ: t}
		elemRef.Type = &refType{typ: tyMapElem}
	} else {
		typs, _ := p.getIdxValTypes(typ, true, elemRef.Src)
		elemRef.Type = &refType{typ: typs[1]}
		// TODO: check index type
	}
	p.stk.Ret(2, elemRef)
	return p
}

func (p *CodeBuilder) getIdxValTypes(typ types.Type, ref bool, idxSrc ast.Node) ([]types.Type, bool) {
retry:
	switch t := typ.(type) {
	case *types.Slice:
		return []types.Type{tyInt, t.Elem()}, false
	case *types.Map:
		return []types.Type{t.Key(), t.Elem()}, true
	case *types.Array:
		return []types.Type{tyInt, t.Elem()}, false
	case *types.Pointer:
		elem := t.Elem()
		if named, ok := elem.(*types.Named); ok {
			elem = p.getUnderlying(named)
		}
		if e, ok := elem.(*types.Array); ok {
			return []types.Type{tyInt, e.Elem()}, false
		}
	case *types.Basic:
		if (t.Info() & types.IsString) != 0 {
			if ref {
				src, pos := p.loadExpr(idxSrc)
				p.panicCodeErrorf(&pos, "cannot assign to %s (strings are immutable)", src)
			}
			return []types.Type{tyInt, TyByte}, false
		}
	case *types.Named:
		typ = p.getUnderlying(t)
		goto retry
	}
	src, pos := p.loadExpr(idxSrc)
	p.panicCodeErrorf(&pos, "invalid operation: %s (type %v does not support indexing)", src, typ)
	return nil, false
}

var (
	tyInt = types.Typ[types.Int]
)

// Typ func
func (p *CodeBuilder) Typ(typ types.Type, src ...ast.Node) *CodeBuilder {
	if debugInstr {
		log.Println("Typ", typ)
	}
	p.stk.Push(&internal.Elem{
		Val:  toType(p.pkg, typ),
		Type: NewTypeType(typ),
		Src:  getSrc(src),
	})
	return p
}

// UntypedBigInt func
func (p *CodeBuilder) UntypedBigInt(v *big.Int, src ...ast.Node) *CodeBuilder {
	pkg := p.pkg
	bigPkg := pkg.big()
	if v.IsInt64() {
		val := &ast.BasicLit{Kind: token.INT, Value: strconv.FormatInt(v.Int64(), 10)}
		p.Val(bigPkg.Ref("NewInt")).Val(val).Call(1)
	} else {
		/*
			func() *typ {
				v, _ := new(typ).SetString(strVal, 10)
				return v
			}()
		*/
		typ := bigPkg.Ref("Int").Type()
		retTyp := types.NewPointer(typ)
		ret := pkg.NewParam(token.NoPos, "", retTyp)
		p.NewClosure(nil, types.NewTuple(ret), false).BodyStart(pkg).
			DefineVarStart(token.NoPos, "v", "_").
			Val(pkg.builtin.Scope().Lookup("new")).Typ(typ).Call(1).
			MemberVal("SetString").Val(v.String()).Val(10).Call(2).EndInit(1).
			Val(p.Scope().Lookup("v")).Return(1).
			End().Call(0)
	}
	ret := p.stk.Get(-1)
	ret.Type, ret.CVal, ret.Src = pkg.utBigInt, constant.Make(v), getSrc(src)
	return p
}

// UntypedBigRat func
func (p *CodeBuilder) UntypedBigRat(v *big.Rat, src ...ast.Node) *CodeBuilder {
	pkg := p.pkg
	bigPkg := pkg.big()
	a, b := v.Num(), v.Denom()
	if a.IsInt64() && b.IsInt64() {
		va := &ast.BasicLit{Kind: token.INT, Value: strconv.FormatInt(a.Int64(), 10)}
		vb := &ast.BasicLit{Kind: token.INT, Value: strconv.FormatInt(b.Int64(), 10)}
		p.Val(bigPkg.Ref("NewRat")).Val(va).Val(vb).Call(2)
	} else {
		// new(big.Rat).SetFrac(a, b)
		p.Val(p.pkg.builtin.Scope().Lookup("new")).Typ(bigPkg.Ref("Rat").Type()).Call(1).
			MemberVal("SetFrac").UntypedBigInt(a).UntypedBigInt(b).Call(2)
	}
	ret := p.stk.Get(-1)
	ret.Type, ret.CVal, ret.Src = pkg.utBigRat, constant.Make(v), getSrc(src)
	return p
}

// Val func
func (p *CodeBuilder) Val(v interface{}, src ...ast.Node) *CodeBuilder {
	if debugInstr {
		if o, ok := v.(types.Object); ok {
			log.Println("Val", o.Name(), o.Type())
		} else {
			log.Println("Val", v, reflect.TypeOf(v))
		}
	}
	fn := p.current.fn
	if fn != nil && fn.isInline() { // is in an inline call
		if param, ok := v.(*types.Var); ok {
			key := closureParamInst{fn, param}
			if arg, ok := p.paramInsts[key]; ok { // replace param with arg
				v = arg
			}
		}
	}
	return p.pushVal(v, getSrc(src))
}

func (p *CodeBuilder) pushVal(v interface{}, src ast.Node) *CodeBuilder {
	p.stk.Push(toExpr(p.pkg, v, src))
	return p
}

// Star func
func (p *CodeBuilder) Star(src ...ast.Node) *CodeBuilder {
	if debugInstr {
		log.Println("Star")
	}
	arg := p.stk.Get(-1)
	ret := &internal.Elem{Val: &ast.StarExpr{X: arg.Val}, Src: getSrc(src)}
	argType := arg.Type
retry:
	switch t := argType.(type) {
	case *TypeType:
		ret.Type = &TypeType{typ: types.NewPointer(t.typ)}
	case *types.Pointer:
		ret.Type = t.Elem()
	case *types.Named:
		argType = p.getUnderlying(t)
		goto retry
	default:
		code, pos := p.loadExpr(arg.Src)
		p.panicCodeErrorf(&pos, "invalid indirect of %s (type %v)", code, t)
	}
	p.stk.Ret(1, ret)
	return p
}

// Elem func
func (p *CodeBuilder) Elem(src ...ast.Node) *CodeBuilder {
	if debugInstr {
		log.Println("Elem")
	}
	arg := p.stk.Get(-1)
	t, ok := arg.Type.(*types.Pointer)
	if !ok {
		code, pos := p.loadExpr(arg.Src)
		p.panicCodeErrorf(&pos, "invalid indirect of %s (type %v)", code, arg.Type)
	}
	p.stk.Ret(1, &internal.Elem{Val: &ast.StarExpr{X: arg.Val}, Type: t.Elem(), Src: getSrc(src)})
	return p
}

// ElemRef func
func (p *CodeBuilder) ElemRef(src ...ast.Node) *CodeBuilder {
	if debugInstr {
		log.Println("ElemRef")
	}
	arg := p.stk.Get(-1)
	t, ok := arg.Type.(*types.Pointer)
	if !ok {
		code, pos := p.loadExpr(arg.Src)
		p.panicCodeErrorf(&pos, "invalid indirect of %s (type %v)", code, arg.Type)
	}
	p.stk.Ret(1, &internal.Elem{
		Val: &ast.StarExpr{X: arg.Val}, Type: &refType{typ: t.Elem()}, Src: getSrc(src),
	})
	return p
}

// MemberVal func
func (p *CodeBuilder) MemberVal(name string, src ...ast.Node) *CodeBuilder {
	_, err := p.Member(name, MemberFlagVal, src...)
	if err != nil {
		panic(err)
	}
	return p
}

// MemberRef func
func (p *CodeBuilder) MemberRef(name string, src ...ast.Node) *CodeBuilder {
	_, err := p.Member(name, MemberFlagRef, src...)
	if err != nil {
		panic(err)
	}
	return p
}

func (p *CodeBuilder) refMember(typ types.Type, name string, argVal ast.Expr) MemberKind {
	switch o := indirect(typ).(type) {
	case *types.Named:
		if struc, ok := p.getUnderlying(o).(*types.Struct); ok {
			if p.fieldRef(argVal, struc, name) {
				return MemberField
			}
		}
	case *types.Struct:
		if p.fieldRef(argVal, o, name) {
			return MemberField
		}
	}
	return MemberInvalid
}

func (p *CodeBuilder) fieldRef(x ast.Expr, struc *types.Struct, name string) bool {
	if t := p.structFieldType(struc, name); t != nil {
		p.stk.Ret(1, &internal.Elem{
			Val:  &ast.SelectorExpr{X: x, Sel: ident(name)},
			Type: &refType{typ: t},
		})
		return true
	}
	return false
}

func (p *CodeBuilder) structFieldType(o *types.Struct, name string) types.Type {
	for i, n := 0, o.NumFields(); i < n; i++ {
		fld := o.Field(i)
		if fld.Name() == name {
			return fld.Type()
		} else if fld.Embedded() {
			fldt := fld.Type()
			if o, ok := fldt.(*types.Pointer); ok {
				fldt = o.Elem()
			}
			if t, ok := fldt.(*types.Named); ok {
				u := p.getUnderlying(t)
				if struc, ok := u.(*types.Struct); ok {
					if typ := p.structFieldType(struc, name); typ != nil {
						return typ
					}
				}
			}
		}
	}
	return nil
}

type (
	MemberKind int
	MemberFlag int
)

const (
	MemberInvalid MemberKind = iota
	MemberMethod
	MemberAutoProperty
	MemberField
	memberBad MemberKind = -1
)

const (
	MemberFlagVal MemberFlag = iota
	MemberFlagMethodAlias
	MemberFlagAutoProperty
	MemberFlagRef MemberFlag = -1
)

func aliasNameOf(name string, flag MemberFlag) (string, MemberFlag) {
	if flag > 0 && name != "" {
		if c := name[0]; c >= 'a' && c <= 'z' {
			return string(rune(c)+('A'-'a')) + name[1:], flag
		}
	}
	return "", MemberFlagVal
}

// Member func
func (p *CodeBuilder) Member(name string, flag MemberFlag, src ...ast.Node) (kind MemberKind, err error) {
	srcExpr := getSrc(src)
	arg := p.stk.Get(-1)
	if debugInstr {
		log.Println("Member", name, flag, "//", arg.Type)
	}
	at := arg.Type
	if flag == MemberFlagRef {
		kind = p.refMember(at, name, arg.Val)
	} else {
		t, isType := at.(*TypeType)
		if isType {
			at = t.typ
			if flag == MemberFlagAutoProperty {
				flag = MemberFlagVal // can't use auto property to type
			}
		}
		aliasName, flag := aliasNameOf(name, flag)
		kind = p.findMember(at, name, aliasName, flag, arg, srcExpr)
		if isType {
			if kind == MemberMethod {
				e := p.Get(-1)
				if sig, ok := e.Type.(*types.Signature); ok {
					sp := sig.Params()
					spLen := sp.Len()
					vars := make([]*types.Var, spLen+1)
					vars[0] = types.NewVar(token.NoPos, nil, "", at)
					for i := 0; i < spLen; i++ {
						vars[i+1] = sp.At(i)
					}
					e.Type = types.NewSignature(nil, types.NewTuple(vars...), sig.Results(), sig.Variadic())
					return
				}
			}
			code, pos := p.loadExpr(srcExpr)
			return MemberInvalid, p.newCodeError(
				&pos, fmt.Sprintf("%s undefined (type %v has no method %s)", code, at, name))
		}
	}
	if kind > 0 {
		return
	}
	code, pos := p.loadExpr(srcExpr)
	return MemberInvalid, p.newCodeError(
		&pos, fmt.Sprintf("%s undefined (type %v has no field or method %s)", code, arg.Type, name))
}

func (p *CodeBuilder) getUnderlying(t *types.Named) types.Type {
	u := t.Underlying()
	if u == nil {
		p.loadNamed(p.pkg, t)
		u = t.Underlying()
	}
	return u
}

func (p *CodeBuilder) ensureLoaded(typ types.Type) {
	if t, ok := typ.(*types.Pointer); ok {
		typ = t.Elem()
	}
	if t, ok := typ.(*types.Named); ok && (t.NumMethods() == 0 || t.Underlying() == nil) {
		if debugMatch {
			log.Println("==> EnsureLoaded", typ)
		}
		p.loadNamed(p.pkg, t)
	}
}

func getUnderlying(pkg *Package, typ types.Type) types.Type {
	u := typ.Underlying()
	if u == nil {
		if t, ok := typ.(*types.Named); ok {
			pkg.cb.loadNamed(pkg, t)
			u = t.Underlying()
		}
	}
	return u
}

func (p *CodeBuilder) findMember(
	typ types.Type, name, aliasName string, flag MemberFlag, arg *Element, srcExpr ast.Node) MemberKind {
retry:
	switch o := typ.(type) {
	case *types.Pointer:
		switch t := o.Elem().(type) {
		case *types.Named:
			u := p.getUnderlying(t)
			if kind := p.method(t, name, aliasName, flag, arg, srcExpr); kind != MemberInvalid {
				return kind
			}
			if struc, ok := u.(*types.Struct); ok {
				if kind := p.field(struc, name, aliasName, flag, arg, srcExpr); kind != MemberInvalid {
					return kind
				}
			}
		case *types.Struct:
			if kind := p.field(t, name, aliasName, flag, arg, srcExpr); kind != MemberInvalid {
				return kind
			}
		}
	case *types.Named:
		typ = p.getUnderlying(o)
		if kind := p.method(o, name, aliasName, flag, arg, srcExpr); kind != MemberInvalid {
			return kind
		}
		goto retry
	case *types.Struct:
		if kind := p.field(o, name, aliasName, flag, arg, srcExpr); kind != MemberInvalid {
			return kind
		}
	case *types.Interface:
		o.Complete()
		if kind := p.method(o, name, aliasName, flag, arg, srcExpr); kind != MemberInvalid {
			return kind
		}
	case *types.Basic, *types.Slice, *types.Map, *types.Chan:
		return p.btiMethod(getBuiltinTI(o), name, aliasName, flag, arg, srcExpr)
	}
	return MemberInvalid
}

type methodList interface {
	NumMethods() int
	Method(i int) *types.Func
}

func selector(arg *Element, name string) *ast.SelectorExpr {
	denoted := &ast.Object{Data: arg}
	return &ast.SelectorExpr{X: arg.Val, Sel: &ast.Ident{Name: name, Obj: denoted}}
}

func denoteRecv(v *ast.SelectorExpr) *Element {
	if o := v.Sel.Obj; o != nil {
		if e, ok := o.Data.(*Element); ok {
			return e
		}
	}
	return nil
}

func (p *CodeBuilder) method(
	o methodList, name, aliasName string, flag MemberFlag, arg *Element, src ast.Node) MemberKind {
	for i, n := 0, o.NumMethods(); i < n; i++ {
		method := o.Method(i)
		v := method.Name()
		if v == name || (flag > 0 && v == aliasName) {
			autoprop := flag == MemberFlagAutoProperty && v == aliasName
			typ := method.Type()
			if autoprop && !methodHasAutoProperty(typ, 0) {
				return memberBad
			}
			p.stk.Ret(1, &internal.Elem{
				Val:  selector(arg, v),
				Type: methodTypeOf(typ),
				Src:  src,
			})
			if autoprop {
				p.Call(0)
				return MemberAutoProperty
			}
			return MemberMethod
		}
	}
	return MemberInvalid
}

func (p *CodeBuilder) btiMethod(
	o *builtinTI, name, aliasName string, flag MemberFlag, arg *Element, src ast.Node) MemberKind {
	if o != nil {
		for i, n := 0, o.NumMethods(); i < n; i++ {
			method := o.Method(i)
			v := method.name
			if v == name || (flag > 0 && v == aliasName) {
				autoprop := flag == MemberFlagAutoProperty && v == aliasName
				this := p.stk.Pop()
				this.Type = &btiMethodType{Type: this.Type, eargs: method.eargs}
				p.Val(method.fn, src)
				p.stk.Push(this)
				if autoprop {
					p.Call(0)
					return MemberAutoProperty
				}
				return MemberMethod
			}
		}
	}
	return MemberInvalid
}

func (p *CodeBuilder) field(
	o *types.Struct, name, aliasName string, flag MemberFlag, arg *Element, src ast.Node) MemberKind {
	for i, n := 0, o.NumFields(); i < n; i++ {
		fld := o.Field(i)
		if fld.Name() == name {
			p.stk.Ret(1, &internal.Elem{
				Val:  selector(arg, name),
				Type: fld.Type(),
				Src:  src,
			})
			return MemberField
		} else if fld.Embedded() {
			if kind := p.findMember(fld.Type(), name, aliasName, flag, arg, src); kind != MemberInvalid {
				return kind
			}
		}
	}
	return MemberInvalid
}

func methodTypeOf(typ types.Type) types.Type {
	sig := typ.(*types.Signature)
	switch t := sig.Recv().Type(); t.(type) {
	case *overloadFuncType:
		// is overload method
		return typ
	case *templateRecvMethodType:
		// is template recv method
		return t
	}
	return types.NewSignature(nil, sig.Params(), sig.Results(), sig.Variadic())
}

func indirect(typ types.Type) types.Type {
	if t, ok := typ.(*types.Pointer); ok {
		typ = t.Elem()
	}
	return typ
}

// AssignOp func
func (p *CodeBuilder) AssignOp(op token.Token, src ...ast.Node) *CodeBuilder {
	args := p.stk.GetArgs(2)
	stmt := callAssignOp(p.pkg, op, args)
	p.emitStmt(stmt)
	p.stk.PopN(2)
	return p
}

func callAssignOp(pkg *Package, tok token.Token, args []*internal.Elem) ast.Stmt {
	name := goxPrefix + assignOps[tok]
	if debugInstr {
		log.Println("AssignOp", tok, name)
	}
	if t, ok := args[0].Type.(*refType).typ.(*types.Named); ok {
		op := lookupMethod(t, name)
		if op != nil {
			fn := &internal.Elem{
				Val:  &ast.SelectorExpr{X: args[0].Val, Sel: ident(name)},
				Type: realType(op.Type()),
			}
			ret := toFuncCall(pkg, fn, args, 0)
			if ret.Type != nil {
				log.Panicf("TODO: AssignOp %s should return no results\n", name)
			}
			return &ast.ExprStmt{X: ret.Val}
		}
	}
	op := pkg.builtin.Scope().Lookup(name)
	if op == nil {
		panic("TODO: operator not matched")
	}
	fn := &internal.Elem{
		Val: ident(op.Name()), Type: op.Type(),
	}
	toFuncCall(pkg, fn, args, 0)
	return &ast.AssignStmt{
		Tok: tok,
		Lhs: []ast.Expr{args[0].Val},
		Rhs: []ast.Expr{args[1].Val},
	}
}

var (
	assignOps = [...]string{
		token.ADD_ASSIGN: "AddAssign", // +=
		token.SUB_ASSIGN: "SubAssign", // -=
		token.MUL_ASSIGN: "MulAssign", // *=
		token.QUO_ASSIGN: "QuoAssign", // /=
		token.REM_ASSIGN: "RemAssign", // %=

		token.AND_ASSIGN:     "AndAssign",    // &=
		token.OR_ASSIGN:      "OrAssign",     // |=
		token.XOR_ASSIGN:     "XorAssign",    // ^=
		token.AND_NOT_ASSIGN: "AndNotAssign", // &^=
		token.SHL_ASSIGN:     "LshAssign",    // <<=
		token.SHR_ASSIGN:     "RshAssign",    // >>=
	}
)

// Assign func
func (p *CodeBuilder) Assign(lhs int, rhs ...int) *CodeBuilder {
	var v int
	if rhs != nil {
		v = rhs[0]
	} else {
		v = lhs
	}
	if debugInstr {
		log.Println("Assign", lhs, v)
	}
	return p.doAssignWith(lhs, v, nil)
}

// AssignWith func
func (p *CodeBuilder) AssignWith(lhs, rhs int, src ...ast.Node) *CodeBuilder {
	if debugInstr {
		log.Println("Assign", lhs, rhs)
	}
	return p.doAssignWith(lhs, rhs, getSrc(src))
}

func (p *CodeBuilder) doAssignWith(lhs, rhs int, src ast.Node) *CodeBuilder {
	args := p.stk.GetArgs(lhs + rhs)
	stmt := &ast.AssignStmt{
		Tok: token.ASSIGN,
		Lhs: make([]ast.Expr, lhs),
		Rhs: make([]ast.Expr, rhs),
	}
	if rhs == 1 {
		if rhsVals, ok := args[lhs].Type.(*types.Tuple); ok {
			if lhs != rhsVals.Len() {
				pos := p.nodePosition(src)
				caller := p.getCaller(args[lhs].Src)
				p.panicCodeErrorf(
					&pos, "assignment mismatch: %d variables but %v returns %d values",
					lhs, caller, rhsVals.Len())
			}
			for i := 0; i < lhs; i++ {
				val := &internal.Elem{Type: rhsVals.At(i).Type()}
				checkAssignType(p.pkg, args[i].Type, val)
				stmt.Lhs[i] = args[i].Val
			}
			stmt.Rhs[0] = args[lhs].Val
			goto done
		}
	}
	if lhs == rhs {
		for i := 0; i < lhs; i++ {
			checkAssignType(p.pkg, args[i].Type, args[lhs+i])
			stmt.Lhs[i] = args[i].Val
			stmt.Rhs[i] = args[lhs+i].Val
		}
	} else {
		pos := p.nodePosition(src)
		p.panicCodeErrorf(
			&pos, "assignment mismatch: %d variables but %d values", lhs, rhs)
	}
done:
	p.emitStmt(stmt)
	p.stk.PopN(lhs + rhs)
	return p
}

func lookupMethod(t *types.Named, name string) types.Object {
	for i, n := 0, t.NumMethods(); i < n; i++ {
		m := t.Method(i)
		if m.Name() == name {
			return m
		}
	}
	return nil
}

func callOpFunc(cb *CodeBuilder, op token.Token, tokenOps []string, args []*internal.Elem, flags InstrFlags) (ret *internal.Elem, err error) {
	name := goxPrefix + tokenOps[op]
	typ := args[0].Type
retry:
	switch t := typ.(type) {
	case *types.Named:
		lm := lookupMethod(t, name)
		if lm != nil {
			fn := &internal.Elem{
				Val:  &ast.SelectorExpr{X: args[0].Val, Sel: ident(name)},
				Type: realType(lm.Type()),
			}
			return matchFuncCall(cb.pkg, fn, args, flags)
		}
	case *types.Pointer:
		typ = t.Elem()
		goto retry
	}
	if op == token.EQL || op == token.NEQ {
		if !ComparableTo(cb.pkg, args[0], args[1]) {
			return nil, errors.New("mismatched types")
		}
		ret = &internal.Elem{
			Val:  &ast.BinaryExpr{X: checkParenExpr(args[0].Val), Op: op, Y: checkParenExpr(args[1].Val)},
			Type: types.Typ[types.UntypedBool],
			CVal: binaryOp(cb, op, args),
		}
		return
	}
	lm := cb.pkg.builtin.Scope().Lookup(name)
	if lm == nil {
		panic("TODO: operator not matched")
	}
	return matchFuncCall(cb.pkg, toObject(cb.pkg, lm, nil), args, flags)
}

// BinaryOp func
func (p *CodeBuilder) BinaryOp(op token.Token, src ...ast.Node) *CodeBuilder {
	if debugInstr {
		log.Println("BinaryOp", op)
	}
	expr := getSrc(src)
	args := p.stk.GetArgs(2)
	var ret *internal.Elem
	var err error
	if ret, err = callOpFunc(p, op, binaryOps[0:], args, 0); err != nil {
		src, pos := p.loadExpr(expr)
		p.panicCodeErrorf(
			&pos, "invalid operation: %s (mismatched types %v and %v)", src, args[0].Type, args[1].Type)
	}
	ret.Src = expr
	p.stk.Ret(2, ret)
	return p
}

var (
	unaryOps = [...]string{
		token.SUB:   "Neg",
		token.ADD:   "Pos",
		token.XOR:   "Not",
		token.NOT:   "LNot",
		token.ARROW: "Recv",
		token.AND:   "Addr",
	}
	binaryOps = [...]string{
		token.ADD: "Add", // +
		token.SUB: "Sub", // -
		token.MUL: "Mul", // *
		token.QUO: "Quo", // /
		token.REM: "Rem", // %

		token.AND:     "And",    // &
		token.OR:      "Or",     // |
		token.XOR:     "Xor",    // ^
		token.AND_NOT: "AndNot", // &^
		token.SHL:     "Lsh",    // <<
		token.SHR:     "Rsh",    // >>

		token.LAND: "LAnd", // &&
		token.LOR:  "LOr",  // ||

		token.LSS: "LT",
		token.LEQ: "LE",
		token.GTR: "GT",
		token.GEQ: "GE",
		token.EQL: "EQ",
		token.NEQ: "NE",
	}
)

// CompareNil func
func (p *CodeBuilder) CompareNil(op token.Token, src ...ast.Node) *CodeBuilder {
	return p.Val(nil).BinaryOp(op)
}

// UnaryOp func
func (p *CodeBuilder) UnaryOp(op token.Token, twoValue ...bool) *CodeBuilder {
	var flags InstrFlags
	if twoValue != nil && twoValue[0] {
		flags = InstrFlagTwoValue
	}
	if debugInstr {
		log.Println("UnaryOp", op, flags)
	}
	ret, err := callOpFunc(p, op, unaryOps[0:], p.stk.GetArgs(1), flags)
	if err != nil {
		panic(err)
	}
	p.stk.Ret(1, ret)
	return p
}

// IncDec func
func (p *CodeBuilder) IncDec(op token.Token) *CodeBuilder {
	if debugInstr {
		log.Println("IncDec", op)
	}
	pkg := p.pkg
	args := p.stk.GetArgs(1)
	name := goxPrefix + incdecOps[op]
	fn := pkg.builtin.Scope().Lookup(name)
	if fn == nil {
		panic("TODO: operator not matched")
	}
	switch t := fn.Type().(type) {
	case *instructionType:
		if _, err := t.instr.Call(pkg, args, token.NoPos, nil); err != nil {
			panic(err)
		}
	default:
		panic("TODO: IncDec not found?")
	}
	p.stk.Pop()
	return p
}

var (
	incdecOps = [...]string{
		token.INC: "Inc",
		token.DEC: "Dec",
	}
)

// Send func
func (p *CodeBuilder) Send() *CodeBuilder {
	if debugInstr {
		log.Println("Send")
	}
	val := p.stk.Pop()
	ch := p.stk.Pop()
	// TODO: check types
	p.emitStmt(&ast.SendStmt{Chan: ch.Val, Value: val.Val})
	return p
}

// Defer func
func (p *CodeBuilder) Defer() *CodeBuilder {
	if debugInstr {
		log.Println("Defer")
	}
	arg := p.stk.Pop()
	call, ok := arg.Val.(*ast.CallExpr)
	if !ok {
		panic("TODO: please use defer callExpr()")
	}
	p.emitStmt(&ast.DeferStmt{Call: call})
	return p
}

// Go func
func (p *CodeBuilder) Go() *CodeBuilder {
	if debugInstr {
		log.Println("Go")
	}
	arg := p.stk.Pop()
	call, ok := arg.Val.(*ast.CallExpr)
	if !ok {
		panic("TODO: please use go callExpr()")
	}
	p.emitStmt(&ast.GoStmt{Call: call})
	return p
}

// Block func
func (p *CodeBuilder) Block() *CodeBuilder {
	if debugInstr {
		log.Println("Block")
	}
	stmt := &blockStmt{}
	p.startBlockStmt(stmt, "block statement", &stmt.old)
	return p
}

// If func
func (p *CodeBuilder) If() *CodeBuilder {
	if debugInstr {
		log.Println("If")
	}
	stmt := &ifStmt{}
	p.startBlockStmt(stmt, "if statement", &stmt.old)
	return p
}

// Then func
func (p *CodeBuilder) Then() *CodeBuilder {
	if debugInstr {
		log.Println("Then")
	}
	if p.stk.Len() == p.current.base {
		panic("use None() for empty expr")
	}
	if flow, ok := p.current.codeBlock.(controlFlow); ok {
		flow.Then(p)
		return p
	}
	panic("use if..then or switch..then please")
}

// Else func
func (p *CodeBuilder) Else() *CodeBuilder {
	if debugInstr {
		log.Println("Else")
	}
	if flow, ok := p.current.codeBlock.(*ifStmt); ok {
		flow.Else(p)
		return p
	}
	panic("use if..else please")
}

// TypeSwitch func
func (p *CodeBuilder) TypeSwitch(name string) *CodeBuilder {
	if debugInstr {
		log.Println("TypeSwitch")
	}
	stmt := &typeSwitchStmt{name: name}
	p.startBlockStmt(stmt, "type switch statement", &stmt.old)
	return p
}

// TypeAssert func
func (p *CodeBuilder) TypeAssert(typ types.Type, twoValue bool, src ...ast.Node) *CodeBuilder {
	if debugInstr {
		log.Println("TypeAssert", typ, twoValue)
	}
	arg := p.stk.Get(-1)
	xType, ok := p.checkInterface(arg.Type)
	if !ok {
		text, pos := p.loadExpr(getSrc(src))
		p.panicCodeErrorf(
			&pos, "invalid type assertion: %s (non-interface type %v on left)", text, arg.Type)
	}
	if missing := p.missingMethod(typ, xType); missing != "" {
		pos := p.nodePosition(getSrc(src))
		p.panicCodeErrorf(
			&pos, "impossible type assertion:\n\t%v does not implement %v (missing %s method)",
			typ, arg.Type, missing)
	}
	pkg := p.pkg
	ret := &ast.TypeAssertExpr{X: arg.Val, Type: toType(pkg, typ)}
	if twoValue {
		tyRet := types.NewTuple(
			pkg.NewParam(token.NoPos, "", typ),
			pkg.NewParam(token.NoPos, "", types.Typ[types.Bool]))
		p.stk.Ret(1, &internal.Elem{Type: tyRet, Val: ret})
	} else {
		p.stk.Ret(1, &internal.Elem{Type: typ, Val: ret})
	}
	return p
}

func (p *CodeBuilder) missingMethod(T types.Type, V *types.Interface) (missing string) {
	p.ensureLoaded(T)
	if m, _ := types.MissingMethod(T, V, false); m != nil {
		missing = m.Name()
	}
	return
}

func (p *CodeBuilder) checkInterface(typ types.Type) (*types.Interface, bool) {
retry:
	switch t := typ.(type) {
	case *types.Interface:
		return t, true
	case *types.Named:
		typ = p.getUnderlying(t)
		goto retry
	}
	return nil, false
}

// TypeAssertThen func
func (p *CodeBuilder) TypeAssertThen() *CodeBuilder {
	if debugInstr {
		log.Println("TypeAssertThen")
	}
	if flow, ok := p.current.codeBlock.(*typeSwitchStmt); ok {
		flow.TypeAssertThen(p)
		return p
	}
	panic("use typeSwitch..typeAssertThen please")
}

// TypeCase func
func (p *CodeBuilder) TypeCase(n int) *CodeBuilder { // n=0 means default case
	if debugInstr {
		log.Println("TypeCase", n)
	}
	if flow, ok := p.current.codeBlock.(*typeSwitchStmt); ok {
		flow.TypeCase(p, n)
		return p
	}
	panic("use switch x.(type) .. case please")
}

// Select
func (p *CodeBuilder) Select() *CodeBuilder {
	if debugInstr {
		log.Println("Select")
	}
	stmt := &selectStmt{}
	p.startBlockStmt(stmt, "select statement", &stmt.old)
	return p
}

// CommCase
func (p *CodeBuilder) CommCase(n int) *CodeBuilder {
	if debugInstr {
		log.Println("CommCase", n)
	}
	if n > 1 {
		panic("TODO: multi commStmt in select..case?")
	}
	if flow, ok := p.current.codeBlock.(*selectStmt); ok {
		flow.CommCase(p, n)
		return p
	}
	panic("use select..case please")
}

// Switch func
func (p *CodeBuilder) Switch() *CodeBuilder {
	if debugInstr {
		log.Println("Switch")
	}
	stmt := &switchStmt{}
	p.startBlockStmt(stmt, "switch statement", &stmt.old)
	return p
}

// Case func
func (p *CodeBuilder) Case(n int) *CodeBuilder { // n=0 means default case
	if debugInstr {
		log.Println("Case", n)
	}
	if flow, ok := p.current.codeBlock.(*switchStmt); ok {
		flow.Case(p, n)
		return p
	}
	panic("use switch..case please")
}

func (p *CodeBuilder) NewLabel(pos token.Pos, name string) *Label {
	if p.current.fn == nil {
		panic(p.newCodePosError(pos, "syntax error: non-declaration statement outside function body"))
	}
	if old, ok := p.current.labels[name]; ok {
		oldPos := p.position(old.Pos())
		p.handleErr(p.newCodePosErrorf(pos, "label %s already defined at %v", name, oldPos))
		return nil
	}
	if p.current.labels == nil {
		p.current.labels = make(map[string]*Label)
	}
	l := &Label{Label: *types.NewLabel(pos, p.pkg.Types, name)}
	p.current.labels[name] = l
	return l
}

// LookupLabel func
func (p *CodeBuilder) LookupLabel(name string) (l *Label, ok bool) {
	l, ok = p.current.labels[name]
	return
}

// Label func
func (p *CodeBuilder) Label(l *Label) *CodeBuilder {
	name := l.Name()
	if debugInstr {
		log.Println("Label", name)
	}
	p.current.label = &ast.LabeledStmt{Label: ident(name)}
	return p
}

// Goto func
func (p *CodeBuilder) Goto(l *Label) *CodeBuilder {
	name := l.Name()
	if debugInstr {
		log.Println("Goto", name)
	}
	l.used = true
	p.current.flows |= flowFlagGoto
	p.emitStmt(&ast.BranchStmt{Tok: token.GOTO, Label: ident(name)})
	return p
}

func (p *CodeBuilder) labelFlow(flow int, l *Label) (string, *ast.Ident) {
	if l != nil {
		l.used = true
		p.current.flows |= (flow | flowFlagWithLabel)
		return l.Name(), ident(l.Name())
	}
	p.current.flows |= flow
	return "", nil
}

// Break func
func (p *CodeBuilder) Break(l *Label) *CodeBuilder {
	name, label := p.labelFlow(flowFlagBreak, l)
	if debugInstr {
		log.Println("Break", name)
	}
	p.emitStmt(&ast.BranchStmt{Tok: token.BREAK, Label: label})
	return p
}

// Continue func
func (p *CodeBuilder) Continue(l *Label) *CodeBuilder {
	name, label := p.labelFlow(flowFlagContinue, l)
	if debugInstr {
		log.Println("Continue", name)
	}
	p.emitStmt(&ast.BranchStmt{Tok: token.CONTINUE, Label: label})
	return p
}

// Fallthrough func
func (p *CodeBuilder) Fallthrough() *CodeBuilder {
	if debugInstr {
		log.Println("Fallthrough")
	}
	if flow, ok := p.current.codeBlock.(*caseStmt); ok {
		flow.Fallthrough(p)
		return p
	}
	panic("please use fallthrough in case statement")
}

// For func
func (p *CodeBuilder) For() *CodeBuilder {
	if debugInstr {
		log.Println("For")
	}
	stmt := &forStmt{}
	p.startBlockStmt(stmt, "for statement", &stmt.old)
	return p
}

// Post func
func (p *CodeBuilder) Post() *CodeBuilder {
	if debugInstr {
		log.Println("Post")
	}
	if flow, ok := p.current.codeBlock.(*forStmt); ok {
		flow.Post(p)
		return p
	}
	panic("please use Post() in for statement")
}

// ForRange func
func (p *CodeBuilder) ForRange(names ...string) *CodeBuilder {
	if debugInstr {
		log.Println("ForRange", names)
	}
	stmt := &forRangeStmt{names: names}
	p.startBlockStmt(stmt, "for range statement", &stmt.old)
	return p
}

// RangeAssignThen func
func (p *CodeBuilder) RangeAssignThen(pos token.Pos) *CodeBuilder {
	if debugInstr {
		log.Println("RangeAssignThen")
	}
	if flow, ok := p.current.codeBlock.(*forRangeStmt); ok {
		flow.RangeAssignThen(p, pos)
		return p
	}
	panic("please use RangeAssignThen() in for range statement")
}

// ResetStmt resets the statement state of CodeBuilder.
func (p *CodeBuilder) ResetStmt() {
	if debugInstr {
		log.Println("ResetStmt")
	}
	p.stk.SetLen(p.current.base)
}

// EndStmt func
func (p *CodeBuilder) EndStmt() *CodeBuilder {
	n := p.stk.Len() - p.current.base
	if n > 0 {
		if n != 1 {
			panic("syntax error: unexpected newline, expecting := or = or comma")
		}
		stmt := &ast.ExprStmt{X: p.stk.Pop().Val}
		p.emitStmt(stmt)
	}
	return p
}

// End func
func (p *CodeBuilder) End() *CodeBuilder {
	if debugInstr {
		typ := reflect.TypeOf(p.current.codeBlock)
		if typ.Kind() == reflect.Ptr {
			typ = typ.Elem()
		}
		name := strings.TrimSuffix(strings.Title(typ.Name()), "Stmt")
		log.Println("End //", name)
		if p.stk.Len() > p.current.base {
			panic("forget to call EndStmt()?")
		}
	}
	p.current.End(p)
	return p
}

func (p *CodeBuilder) SetBodyHandler(handle func(body *ast.BlockStmt, kind int)) *CodeBuilder {
	if ini, ok := p.current.codeBlock.(interface {
		SetBodyHandler(func(body *ast.BlockStmt, kind int))
	}); ok {
		ini.SetBodyHandler(handle)
	}
	return p
}

// ResetInit resets the variable init state of CodeBuilder.
func (p *CodeBuilder) ResetInit() {
	if debugInstr {
		log.Println("ResetInit")
	}
	p.varDecl = p.varDecl.resetInit(p)
}

// EndInit func
func (p *CodeBuilder) EndInit(n int) *CodeBuilder {
	if debugInstr {
		log.Println("EndInit", n)
	}
	p.varDecl = p.varDecl.endInit(p, n)
	return p
}

// Debug func
func (p *CodeBuilder) Debug(dbg func(cb *CodeBuilder)) *CodeBuilder {
	dbg(p)
	return p
}

// Get func
func (p *CodeBuilder) Get(idx int) *Element {
	return p.stk.Get(idx)
}

// ----------------------------------------------------------------------------

type InternalStack = internal.Stack

// InternalStack: don't call it (only for internal use)
func (p *CodeBuilder) InternalStack() *InternalStack {
	return &p.stk
}

// ----------------------------------------------------------------------------
