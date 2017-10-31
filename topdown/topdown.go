// Copyright 2016 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package topdown

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/open-policy-agent/opa/topdown/unify"
	"github.com/open-policy-agent/opa/util"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/metrics"
	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/topdown/builtins"
	"github.com/pkg/errors"
)

// Topdown stores the state of the evaluation process and contains context
// needed to evaluate queries.
type Topdown struct {
	Cancel   Cancel
	Query    ast.Body
	Compiler *ast.Compiler
	Input    ast.Value
	Index    int
	Previous *Topdown
	Store    storage.Store
	Tracer   Tracer
	Context  context.Context

	txn      storage.Transaction
	bindings *unify.Unifier
	locals   *ast.ValueMap
	refs     *valueMapStack
	cache    *contextcache
	qid      uint64
	redos    *redoStack
	builtins builtins.Cache
}

// ResetQueryIDs resets the query ID generator. This is only for test purposes.
func ResetQueryIDs() {
	qidFactory.Reset()
}

type queryIDFactory struct {
	next uint64
	mtx  sync.Mutex
}

func (f *queryIDFactory) Next() uint64 {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	next := f.next
	f.next++
	return next
}

func (f *queryIDFactory) Reset() {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.next = uint64(1)
}

var qidFactory = &queryIDFactory{
	next: uint64(1),
}

type redoStack struct {
	events []*redoStackElement
}

type redoStackElement struct {
	t   *Topdown
	evt *Event
}

// New returns a new Topdown object without any bindings.
func New(ctx context.Context, query ast.Body, compiler *ast.Compiler, store storage.Store, txn storage.Transaction) *Topdown {
	t := &Topdown{
		Context:  ctx,
		Query:    query,
		Compiler: compiler,
		Store:    store,
		refs:     newValueMapStack(),
		bindings: unify.New(),
		txn:      txn,
		cache:    newContextCache(),
		qid:      qidFactory.Next(),
		redos:    &redoStack{},
		builtins: builtins.Cache{},
	}
	return t
}

// Vars represents a set of var bindings.
type Vars map[ast.Var]ast.Value

// Diff returns the var bindings in vs that are not in other.
func (vs Vars) Diff(other Vars) Vars {
	result := Vars{}
	for k := range vs {
		if _, ok := other[k]; !ok {
			result[k] = vs[k]
		}
	}
	return result
}

// Equal returns true if vs is equal to other.
func (vs Vars) Equal(other Vars) bool {
	return len(vs.Diff(other)) == 0 && len(other.Diff(vs)) == 0
}

// Vars returns bindings for the vars in the current query
func (t *Topdown) Vars() map[ast.Var]ast.Value {
	vars := map[ast.Var]ast.Value{}
	for _, pair := range t.bindings.Bindings() {
		vars[pair[0].Value.(ast.Var)] = pair[1].Value
	}
	return vars
}

// Binding returns the value bound to the given key.
func (t *Topdown) Binding(k ast.Value) ast.Value {
	switch k := k.(type) {
	case ast.Ref:
		x, u := t.refs.Binding(k)
		if x == nil {
			return nil
		}
		return u.Plug(ast.NewTerm(x)).Value
	case *ast.SetComprehension, *ast.ArrayComprehension, *ast.ObjectComprehension:
		return t.locals.Get(k)
	default:
		v := t.bindings.Plug(ast.NewTerm(k)).Value
		// TODO(tsandall): review need for this.
		if v.Compare(k) == 0 {
			return nil
		}
		return v
	}
}

// Undo represents a binding that can be undone.
type Undo struct {
	Key     ast.Value
	Value   interface{}
	Prev    *Undo
	wrapped unify.Undo
}

// Bind updates t to include a binding from the key to the value. The return
// value is used to return t to the state before the binding was added.
func (t *Topdown) Bind(key ast.Value, value ast.Value, u *unify.Unifier, prev *Undo) *Undo {
	switch key := key.(type) {
	case ast.Ref:
		if u == nil {
			panic("illegal value")
		}
		return t.refs.Bind(key, value, u, prev)
	case *ast.SetComprehension, *ast.ArrayComprehension, *ast.ObjectComprehension:
		if t.locals == nil {
			t.locals = ast.NewValueMap()
		}
		t.locals.Put(key, value)
		return &Undo{key, nil, prev, nil}
	default:
		wrapped := unify.Unify(ast.NewTerm(key), t.bindings, ast.NewTerm(value), t.bindings)
		return &Undo{nil, nil, prev, wrapped}
	}
}

// Unbind updates t by removing the binding represented by the undo.
func (t *Topdown) Unbind(undo *Undo) {
	if undo == nil {
		return
	}
	if _, ok := undo.Key.(ast.Ref); ok {
		for u := undo; u != nil; u = u.Prev {
			t.refs.Unbind(u)
		}
	} else {
		for u := undo; u != nil; u = u.Prev {
			if u.wrapped != nil {
				u.wrapped.Undo()
			} else {
				t.locals.Delete(u.Key)
			}
		}
	}
}

// Closure returns a new Topdown object to evaluate query with bindings from t.
func (t *Topdown) Closure(query ast.Body) *Topdown {
	cpy := *t
	cpy.Query = query
	cpy.Previous = t
	cpy.Index = 0
	cpy.qid = qidFactory.Next()
	return &cpy
}

// Child returns a new Topdown object to evaluate query without bindings from t.
func (t *Topdown) Child(query ast.Body) *Topdown {
	cpy := t.Closure(query)
	cpy.locals = nil
	cpy.refs = newValueMapStack()
	cpy.bindings = unify.New()
	return cpy
}

// Current returns the current expression to evaluate.
func (t *Topdown) Current() *ast.Expr {
	return t.Query[t.Index]
}

// Resolve returns the native Go value referred to by the ref.
func (t *Topdown) Resolve(ref ast.Ref) (interface{}, error) {

	if ref.IsNested() {
		cpy := make(ast.Ref, len(ref))
		for i := range ref {
			switch v := ref[i].Value.(type) {
			case ast.Ref:
				r, err := lookupValue(t, v)
				if err != nil {
					return nil, err
				}
				cpy[i] = ast.NewTerm(r)
			default:
				cpy[i] = ref[i]
			}
		}
		ref = cpy
	}

	path, err := storage.NewPathForRef(ref)
	if err != nil {
		return nil, err
	}

	doc, err := t.Store.Read(t.Context, t.txn, path)
	if err != nil {
		return nil, err
	}

	// When the root document is queried we hide the SystemDocumentKey.
	if len(path) == 0 {
		obj := doc.(map[string]interface{})
		tmp := map[string]interface{}{}
		for k := range obj {
			if k != string(ast.SystemDocumentKey) {
				tmp[k] = obj[k]
			}
		}
		doc = tmp
	}

	return doc, nil
}

// Step returns a new Topdown object to evaluate the next expression.
func (t *Topdown) Step() *Topdown {
	cpy := *t
	cpy.Index++
	return &cpy
}

// WithInput returns a new Topdown object that has the input document set.
func (t *Topdown) WithInput(input ast.Value) *Topdown {
	cpy := *t
	cpy.Input = input
	return &cpy
}

// WithTracer returns a new Topdown object that has a tracer set.
func (t *Topdown) WithTracer(tracer Tracer) *Topdown {
	cpy := *t
	cpy.Tracer = tracer
	return &cpy
}

// WithCancel returns a new Topdown object that has cancellation set.
func (t *Topdown) WithCancel(c Cancel) *Topdown {
	cpy := *t
	cpy.Cancel = c
	return &cpy
}

// currentLocation returns the current expression's location or the first
// fallback that has a location set, otherwise nil.
func (t *Topdown) currentLocation(fallback ...ast.Statement) *ast.Location {
	curr := t.Current().Location
	if curr != nil {
		return curr
	}
	for i := range fallback {
		curr = fallback[i].Loc()
		if curr != nil {
			return curr
		}
	}
	return nil
}

func (t *Topdown) traceEnter(node interface{}) {
	if t.tracingEnabled() {
		evt := t.makeEvent(EnterOp, node)
		t.flushRedos(evt)
		t.Tracer.Trace(t, evt)
	}
}

func (t *Topdown) traceExit(node interface{}) {
	if t.tracingEnabled() {
		evt := t.makeEvent(ExitOp, node)
		t.flushRedos(evt)
		t.Tracer.Trace(t, evt)
	}
}

func (t *Topdown) traceEval(node interface{}) {
	if t.tracingEnabled() {
		evt := t.makeEvent(EvalOp, node)
		t.flushRedos(evt)
		t.Tracer.Trace(t, evt)
	}
}

func (t *Topdown) traceRedo(node interface{}) {
	if t.tracingEnabled() {
		evt := t.makeEvent(RedoOp, node)
		t.saveRedo(evt)
	}
}

func (t *Topdown) traceFail(node interface{}) {
	if t.tracingEnabled() {
		evt := t.makeEvent(FailOp, node)
		t.flushRedos(evt)
		t.Tracer.Trace(t, evt)
	}
}

func (t *Topdown) tracingEnabled() bool {
	return t.Tracer != nil && t.Tracer.Enabled()
}

func (t *Topdown) saveRedo(evt *Event) {

	buf := &redoStackElement{
		t:   t,
		evt: evt,
	}

	// Search stack for redo that this (redo) event should follow.
	for len(t.redos.events) > 0 {
		idx := len(t.redos.events) - 1
		top := t.redos.events[idx]

		// Expression redo should follow rule/body redo from the same query.
		if evt.HasExpr() {
			if top.evt.QueryID == evt.QueryID && (top.evt.HasBody() || top.evt.HasRule()) {
				break
			}
		}

		// Rule/body redo should follow expression redo from the parent query.
		if evt.HasRule() || evt.HasBody() {
			if top.evt.QueryID == evt.ParentID && top.evt.HasExpr() {
				break
			}
		}

		// Top of stack can be discarded. This indicates the search terminated
		// without producing any more events.
		t.redos.events = t.redos.events[:idx]
	}

	t.redos.events = append(t.redos.events, buf)
}

func (t *Topdown) flushRedos(evt *Event) {

	idx := len(t.redos.events) - 1

	if idx != -1 {
		top := t.redos.events[idx]

		if top.evt.QueryID == evt.QueryID {
			for _, buf := range t.redos.events {
				t.Tracer.Trace(buf.t, buf.evt)
			}
		}

		t.redos.events = nil
	}

}

func (t *Topdown) makeEvent(op Op, node interface{}) *Event {
	locals := ast.NewValueMap()
	for _, pair := range t.bindings.Bindings() {
		locals.Put(pair[0].Value, pair[1].Value)
	}
	evt := Event{
		Op:      op,
		Node:    node,
		QueryID: t.qid,
		Locals:  locals,
	}
	if t.Previous != nil {
		evt.ParentID = t.Previous.qid
	}
	return &evt
}

// contextcache stores the result of rule evaluation for a query. The
// contextcache is inherited by child contexts. The contextcache is consulted
// when virtual document references are evaluated. If a miss occurs, the virtual
// document is generated and the contextcache is updated.
type contextcache struct {
	partialobjs partialObjDocCache
	complete    completeDocCache
}

type partialObjDocCache map[*ast.Rule]map[ast.Value]ast.Value
type completeDocCache map[*ast.Rule]ast.Value

func newContextCache() *contextcache {
	return &contextcache{
		partialobjs: partialObjDocCache{},
		complete:    completeDocCache{},
	}
}

func (c *contextcache) Invalidate() {
	c.partialobjs = partialObjDocCache{}
	c.complete = completeDocCache{}
}

// Iterator is the interface for processing evaluation results.
type Iterator func(*Topdown) error

func Continue(t *Topdown, key, value ast.Value, u *unify.Unifier, iter Iterator) error {
	undo := t.Bind(key, value, u, nil)
	err := iter(t)
	t.Unbind(undo)
	return err
}

// Eval evaluates the query in t and calls iter once for each set of bindings
// that satisfy all of the expressions in the query.
func Eval(t *Topdown, iter Iterator) error {
	t.traceEnter(t.Query)
	return eval(t, func(t *Topdown) error {
		t.traceExit(t.Query)
		if err := iter(t); err != nil {
			return err
		}
		t.traceRedo(t.Query)
		return nil
	})
}

// Binding defines the interface used to apply term bindings to terms,
// expressions, etc.
type Binding func(ast.Value) ast.Value

// PlugHead returns a copy of head with bound terms substituted for the binding.
func PlugHead(head *ast.Head, binding Binding) *ast.Head {
	plugged := *head
	if plugged.Key != nil {
		plugged.Key = PlugTerm(plugged.Key, binding)
	}
	if plugged.Value != nil {
		plugged.Value = PlugTerm(plugged.Value, binding)
	}
	return &plugged
}

// PlugExpr returns a copy of expr with bound terms substituted for the binding.
func PlugExpr(expr *ast.Expr, binding Binding) *ast.Expr {
	plugged := *expr
	switch ts := plugged.Terms.(type) {
	case []*ast.Term:
		var buf []*ast.Term
		buf = append(buf, ts[0])
		for _, term := range ts[1:] {
			buf = append(buf, PlugTerm(term, binding))
		}
		plugged.Terms = buf
	case *ast.Term:
		plugged.Terms = PlugTerm(ts, binding)
	default:
		panic(fmt.Sprintf("illegal argument: %v", ts))
	}
	return &plugged
}

// PlugTerm returns a copy of term with bound terms substituted for the binding.
func PlugTerm(term *ast.Term, binding Binding) *ast.Term {
	switch v := term.Value.(type) {
	case ast.Var, ast.Ref, ast.Array, ast.Object, *ast.Set, *ast.ArrayComprehension, *ast.ObjectComprehension, *ast.SetComprehension:
		plugged := *term
		plugged.Value = PlugValue(v, binding)
		return &plugged

	default:
		if !term.IsGround() {
			panic("unreachable")
		}
		return term
	}
}

// PlugValue returns a copy of v with bound terms substituted for the binding.
func PlugValue(v ast.Value, binding func(ast.Value) ast.Value) ast.Value {

	switch v := v.(type) {
	case ast.Var:
		if b := binding(v); b != nil {
			return PlugValue(b, binding)
		}
		return v

	case *ast.ArrayComprehension, *ast.ObjectComprehension, *ast.SetComprehension:
		b := binding(v)
		if b == nil {
			return v
		}
		return b

	case ast.Ref:
		if b := binding(v); b != nil {
			return PlugValue(b, binding)
		}
		buf := make(ast.Ref, len(v))
		buf[0] = v[0]
		for i, p := range v[1:] {
			buf[i+1] = PlugTerm(p, binding)
		}
		if b := binding(buf); b != nil {
			return PlugValue(b, binding)
		}
		return buf

	case ast.Array:
		buf := make(ast.Array, len(v))
		for i, e := range v {
			buf[i] = PlugTerm(e, binding)
		}
		return buf

	case ast.Object:
		buf := make(ast.Object, len(v))
		for i, e := range v {
			k := PlugTerm(e[0], binding)
			v := PlugTerm(e[1], binding)
			buf[i] = [...]*ast.Term{k, v}
		}
		return buf

	case *ast.Set:
		buf := &ast.Set{}
		for _, e := range *v {
			buf.Add(PlugTerm(e, binding))
		}
		return buf

	case nil:
		return nil

	default:
		if !v.IsGround() {
			panic(fmt.Sprintf("illegal value: %v", v))
		}
		return v
	}
}

// QueryParams defines input parameters for the query interface.
type QueryParams struct {
	Context     context.Context
	Cancel      Cancel
	Compiler    *ast.Compiler
	Store       storage.Store
	Transaction storage.Transaction
	Input       ast.Value
	Tracer      Tracer
	Metrics     metrics.Metrics
	Path        ast.Ref
}

// NewQueryParams returns a new QueryParams.
func NewQueryParams(ctx context.Context, compiler *ast.Compiler, store storage.Store, txn storage.Transaction, input ast.Value, path ast.Ref) *QueryParams {
	return &QueryParams{
		Context:     ctx,
		Compiler:    compiler,
		Store:       store,
		Transaction: txn,
		Input:       input,
		Path:        path,
	}
}

// NewTopdown returns a new Topdown object.
//
// This function will not propagate optional values such as the tracer, input
// document, etc. Those must be set by the caller.
func (q *QueryParams) NewTopdown(query ast.Body) *Topdown {
	return New(q.Context, query, q.Compiler, q.Store, q.Transaction).WithCancel(q.Cancel)
}

// QueryResult represents a single query result.
type QueryResult struct {
	Result   interface{}            // Result contains the document referred to by the params Path.
	Bindings map[string]interface{} // Bindings contains values for variables in the params Input.
}

func (qr *QueryResult) String() string {
	return fmt.Sprintf("[%v %v]", qr.Result, qr.Bindings)
}

// QueryResultSet represents a collection of query results.
type QueryResultSet []*QueryResult

// Undefined returns true if the query did not find any results.
func (qrs QueryResultSet) Undefined() bool {
	return len(qrs) == 0
}

// Add inserts a result into the query result set.
func (qrs *QueryResultSet) Add(qr *QueryResult) {
	*qrs = append(*qrs, qr)
}

// Query returns the value of the document referred to by the params' path. If
// the params' input contains non-ground terms, there may be multiple query
// results.
func Query(params *QueryParams) (QueryResultSet, error) {

	if params.Metrics == nil {
		params.Metrics = metrics.New()
	}

	t, resultVar, requestVars, err := makeTopdown(params)
	if err != nil {
		return nil, err
	}

	qrs := QueryResultSet{}

	params.Metrics.Timer(metrics.RegoQueryEval).Start()

	err = Eval(t, func(t *Topdown) error {

		// Gather bindings for vars from the request.
		bindings := map[string]interface{}{}
		for v := range requestVars {
			binding, err := ast.ValueToInterface(PlugValue(v, t.Binding), t)
			if err != nil {
				return err
			}
			bindings[v.String()] = binding
		}

		// Gather binding for result var.
		val, err := ast.ValueToInterface(PlugValue(resultVar, t.Binding), t)
		if err != nil {
			return err
		}

		// Aggregate results.
		qrs.Add(&QueryResult{val, bindings})
		return nil
	})

	params.Metrics.Timer(metrics.RegoQueryEval).Stop()

	return qrs, err
}

func makeTopdown(params *QueryParams) (*Topdown, ast.Var, ast.VarSet, error) {

	inputVar := ast.VarTerm(ast.WildcardPrefix + "0")
	pathVar := ast.VarTerm(ast.WildcardPrefix + "1")

	var query ast.Body

	params.Metrics.Timer(metrics.RegoQueryCompile).Start()

	if params.Input == nil {
		query = ast.NewBody(ast.Equality.Expr(ast.NewTerm(params.Path), pathVar))
	} else {
		// <input> = $0,
		// <path> = $1 with input as $0
		inputExpr := ast.Equality.Expr(ast.NewTerm(params.Input), inputVar)
		pathExpr := ast.Equality.Expr(ast.NewTerm(params.Path), pathVar).
			IncludeWith(ast.NewTerm(ast.InputRootRef), inputVar)
		query = ast.NewBody(inputExpr, pathExpr)
	}

	compiled, err := params.Compiler.QueryCompiler().Compile(query)
	if err != nil {
		return nil, "", nil, err
	}

	params.Metrics.Timer(metrics.RegoQueryCompile).Stop()

	vis := ast.NewVarVisitor().WithParams(ast.VarVisitorParams{
		SkipRefHead:  true,
		SkipClosures: true,
	})

	ast.Walk(vis, params.Input)

	t := params.NewTopdown(compiled).
		WithTracer(params.Tracer)

	return t, pathVar.Value.(ast.Var), vis.Vars(), nil
}

// Resolver defines the interface for resolving references to base documents to
// native Go values. The native Go value types map to JSON types.
type Resolver interface {
	Resolve(ref ast.Ref) (value interface{}, err error)
}

type resolver struct {
	context context.Context
	store   storage.Store
	txn     storage.Transaction
}

func (r resolver) Resolve(ref ast.Ref) (interface{}, error) {
	path, err := storage.NewPathForRef(ref)
	if err != nil {
		return nil, err
	}
	return r.store.Read(r.context, r.txn, path)
}

// ResolveRefs returns the value obtained by resolving references to base
// documents.
func ResolveRefs(v ast.Value, t *Topdown) (ast.Value, error) {
	result, err := ast.TransformRefs(v, func(r ast.Ref) (ast.Value, error) {
		return lookupValue(t, r)
	})
	if err != nil {
		return nil, err
	}
	return result.(ast.Value), nil
}

// ResolveRefsTerm returns a copy of term obtained by resolving references to
// base documents.
func ResolveRefsTerm(term *ast.Term, t *Topdown) (*ast.Term, error) {
	cpy := *term
	var err error
	cpy.Value, err = ResolveRefs(term.Value, t)
	if err != nil {
		return nil, err
	}
	return &cpy, nil
}

func eval(t *Topdown, iter Iterator) error {

	if t.Cancel != nil && t.Cancel.Cancelled() {
		return &Error{
			Code:    CancelErr,
			Message: "caller cancelled query execution",
		}
	}

	if t.Index >= len(t.Query) {
		return iter(t)
	}

	if len(t.Current().With) > 0 {
		return evalWith(t, iter)
	}

	return evalStep(t, func(t *Topdown) error {
		t = t.Step()
		return eval(t, iter)
	})
}

func evalStep(t *Topdown, iter Iterator) error {

	if t.Current().Negated {
		return evalNot(t, iter)
	}

	t.traceEval(t.Current())

	// isRedo indicates if the expression's terms are defined at least once. If
	// any of the terms are undefined, then the closure below will not run (but
	// a Fail event still needs to be emitted).
	isRedo := false

	err := evalTerms(t, func(t *Topdown) error {
		isRedo = true

		// isTrue indicates if the expression is true and is used to determine
		// if a Fail event should be emitted below.
		isTrue := false

		err := evalExpr(t, func(t *Topdown) error {
			isTrue = true
			return iter(t)
		})

		if err != nil {
			return err
		}

		if !isTrue {
			t.traceFail(t.Current())
		}

		t.traceRedo(t.Current())

		return nil
	})

	if err != nil {
		return err
	}

	if !isRedo {
		t.traceFail(t.Current())
	}

	return nil
}

func evalNot(t *Topdown, iter Iterator) error {

	negation := ast.NewBody(t.Current().Complement().NoWith())
	child := t.Closure(negation)

	t.traceEval(t.Current())

	isTrue := false

	err := Eval(child, func(*Topdown) error {
		isTrue = true
		return nil
	})

	if err != nil {
		return err
	}

	if !isTrue {
		return iter(t)
	}

	t.traceFail(t.Current())

	return nil
}

func evalWith(t *Topdown, iter Iterator) error {

	curr := t.Current()
	pairs := make([][2]*ast.Term, len(curr.With))

	for i := range curr.With {
		plugged := PlugTerm(curr.With[i].Value, t.Binding)
		resolved, err := ResolveRefsTerm(plugged, t)
		if err != nil {
			return err
		}
		pairs[i] = [...]*ast.Term{curr.With[i].Target, resolved}
	}

	input, err := MakeInput(pairs)
	if err != nil {
		return &Error{
			Code:     ConflictErr,
			Location: curr.Location,
			Message:  err.Error(),
		}
	}

	cpy := t.WithInput(input)

	// All ref bindings added during evaluation of this expression must be
	// discarded before moving to the next expression. Push a new binding map
	// onto the stack that will be popped below before continuing. Similarly,
	// the document caches must be invalidated before continuing.
	//
	// TODO(tsandall): analyze queries and invalidate only affected caches.
	cpy.refs.Push(newValueMap())

	err = evalStep(cpy, func(next *Topdown) error {
		next.refs.Pop()
		next.cache.Invalidate()
		next = next.Step()
		return eval(next, iter)
	})

	if err != nil {
		return err
	}

	if vm := cpy.refs.Peek(); vm != nil {
		cpy.refs.Pop()
	}

	cpy.cache.Invalidate()

	return nil
}

func evalExpr(t *Topdown, iter Iterator) error {
	expr := PlugExpr(t.Current(), t.Binding)
	switch tt := expr.Terms.(type) {
	case []*ast.Term:
		ref := tt[0].Value.(ast.Ref)
		if ast.DefaultRootDocument.Equal(ref[0]) {
			return evalRefRuleApply(t, ref, tt[1:], iter)
		}
		name := tt[0].String()
		builtin, ok := builtinFunctions[name]
		if !ok {
			return unsupportedBuiltinErr(expr.Location)
		}
		return builtin(t, expr, iter)
	case *ast.Term:
		v := tt.Value
		if r, ok := v.(ast.Ref); ok {
			var err error
			v, err = lookupValue(t, r)
			if err != nil {
				return err
			}
		}
		if v.Compare(ast.Boolean(false)) != 0 {
			if v.IsGround() {
				return iter(t)
			}
		}
		return nil
	default:
		panic(fmt.Sprintf("illegal argument: %v", tt))
	}
}

// evalRef evaluates the reference and invokes the iterator for each instance of
// the reference that is defined. The iterator is invoked with bindings for (1)
// all variables found in the reference and (2) the reference itself if that
// reference refers to a virtual document (ditto for nested references).
func evalRef(t *Topdown, ref, path ast.Ref, iter Iterator) error {
	return evalDot(t, ref, iter)
}
func evalRefRuleApply(t *Topdown, path ast.Ref, args []*ast.Term, iter Iterator) error {

	index := t.Compiler.RuleIndex(path)
	ir, err := index.Lookup(valueResolver{t})
	if err != nil || ir.Empty() {
		return err
	}

	// If function is being applied and return value is being ignored, append a
	// wildcard variable to the expression so that it will unify below.
	if len(args) == len(ir.Rules[0].Head.Args) {
		args = append(args, ast.VarTerm(ast.WildcardPrefix+"apply"))
	}

	resolved, err := resolveN(t, path.String(), args, len(args)-1)
	if err != nil {
		return err
	}

	resolvedArgs := make(ast.Array, len(resolved))
	for i := range resolved {
		resolvedArgs[i] = ast.NewTerm(resolved[i])
	}

	var redo bool
	var result *ast.Term

	for _, rule := range ir.Rules {
		next, err := evalRefRuleApplyOne(t, rule, resolvedArgs, redo, result)
		if err != nil {
			return err
		}
		redo = true
		if next != nil {
			result = next
		} else {
			chain := ir.Else[rule]
			for i := range chain {
				next, err := evalRefRuleApplyOne(t, chain[i], resolvedArgs, redo, result)
				if err != nil {
					return err
				}
				if next != nil {
					result = next
					break
				}
			}
		}
	}

	if result == nil {
		return nil
	}

	return unifyAndContinue(t, iter, result.Value, args[len(args)-1].Value)
}

func evalRefRuleApplyOne(t *Topdown, rule *ast.Rule, args ast.Array, redo bool, last *ast.Term) (*ast.Term, error) {
	child := t.Child(rule.Body)
	if !redo {
		child.traceEnter(rule)
	} else {
		child.traceRedo(rule)
	}
	var result *ast.Term
	ruleArgs := ast.Array(rule.Head.Args)
	undo, err := evalEqUnify(child, args, ruleArgs, nil, func(child *Topdown) error {
		return eval(child, func(child *Topdown) error {
			result = PlugTerm(rule.Head.Value, child.Binding)
			if last != nil && ast.Compare(last, result) != 0 {
				return completeDocConflictErr(t.currentLocation(rule))
			}
			if last == nil && result != nil {
				last = result
			}
			child.traceExit(rule)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	child.Unbind(undo)
	return result, nil
}

func evalTerms(t *Topdown, iter Iterator) error {

	expr := t.Current()

	// Attempt to evaluate the terms using indexing. Indexing can be used
	// if this is an equality expression where one side is a non-ground,
	// non-nested reference to a base document and the other side is a
	// reference or some ground term. If indexing is available for the terms,
	// the index is built lazily.
	if expr.IsEquality() {

		// The terms must be plugged otherwise the index may yield bindings
		// that would result in false positives.
		ts := expr.Terms.([]*ast.Term)
		t1 := PlugTerm(ts[1], t.Binding)
		t2 := PlugTerm(ts[2], t.Binding)
		r1, _ := t1.Value.(ast.Ref)
		r2, _ := t2.Value.(ast.Ref)

		if indexingAllowed(r1, t2) {
			index, err := indexBuildLazy(t, r1)
			if err != nil {
				return errors.Wrapf(err, "index build failed on %v", r1)
			}
			if index != nil {
				return evalTermsIndexed(t, iter, index, t2)
			}
		}

		if indexingAllowed(r2, t1) {
			index, err := indexBuildLazy(t, r2)
			if err != nil {
				return errors.Wrapf(err, "index build failed on %v", r2)
			}
			if index != nil {
				return evalTermsIndexed(t, iter, index, t1)
			}
		}
	}

	var ts []*ast.Term
	switch t := expr.Terms.(type) {
	case []*ast.Term:
		ts = t[1:]
	case *ast.Term:
		ts = append(ts, t)
	default:
		panic(fmt.Sprintf("illegal argument: %v", t))
	}

	return evalTermsRec(t, iter, ts)
}

func evalTermsComprehension(t *Topdown, comp ast.Value, iter Iterator) error {
	switch comp := comp.(type) {
	case *ast.ArrayComprehension:
		result := ast.Array{}
		child := t.Closure(comp.Body)

		err := Eval(child, func(child *Topdown) error {
			result = append(result, PlugTerm(comp.Term, child.Binding))
			return nil
		})

		if err != nil {
			return err
		}

		return Continue(t, comp, result, nil, iter)
	case *ast.SetComprehension:
		result := ast.Set{}
		child := t.Closure(comp.Body)

		err := Eval(child, func(child *Topdown) error {
			result.Add(PlugTerm(comp.Term, child.Binding))
			return nil
		})

		if err != nil {
			return err
		}

		return Continue(t, comp, &result, nil, iter)
	case *ast.ObjectComprehension:
		result := ast.Object{}
		child := t.Closure(comp.Body)
		err := Eval(child, func(child *Topdown) error {
			key := PlugTerm(comp.Key, child.Binding)
			value := PlugTerm(comp.Value, child.Binding)
			if v := result.Get(key); v != nil && !v.Equal(value) {
				return &Error{
					Code:     ConflictErr,
					Message:  "object comprehension produces conflicting outputs",
					Location: t.Current().Location,
				}
			}

			result = append(result, [2]*ast.Term{key, value})
			return nil
		})
		if err != nil {
			return err
		}

		return Continue(t, comp, result, nil, iter)
	default:
		panic(fmt.Sprintf("illegal argument: %v %v", t, comp))
	}
}

func evalTermsIndexed(t *Topdown, iter Iterator, index storage.Index, nonIndexed *ast.Term) error {

	iterateIndex := func(t *Topdown) error {

		// Evaluate the non-indexed term.
		value, err := ast.ValueToInterface(PlugValue(nonIndexed.Value, t.Binding), t)
		if err != nil {
			return err
		}

		// Iterate the bindings for the indexed term that when applied to the reference
		// would locate the non-indexed value obtained above.
		err = index.Lookup(t.Context, t.txn, value, func(bindings *ast.ValueMap) error {
			var prev *Undo

			// We will skip these bindings if the non-indexed term contains a
			// different binding for the same variable. This can arise if output
			// variables in references on either side intersect (e.g., a[i] = g[i][j]).
			skip := bindings.Iter(func(k, v ast.Value) bool {
				if o := t.Binding(k); o != nil && o.Compare(v) != 0 {
					return true
				}
				prev = t.Bind(k, v, nil, prev)
				return false
			})

			var err error

			if !skip {
				err = iter(t)
			}

			t.Unbind(prev)
			return err
		})

		return err
	}

	return evalTermsRec(t, iterateIndex, []*ast.Term{nonIndexed})
}

func evalTermsRec(t *Topdown, iter Iterator, ts []*ast.Term) error {

	if len(ts) == 0 {
		return iter(t)
	}

	head := ts[0]
	tail := ts[1:]

	rec := func(t *Topdown) error {
		return evalTermsRec(t, iter, tail)
	}

	switch head := head.Value.(type) {
	case ast.Ref:
		return evalRef(t, head, ast.Ref{}, rec)
	case ast.Array:
		return evalTermsRecArray(t, head, 0, rec)
	case ast.Object:
		return evalTermsRecObject(t, head, 0, rec)
	case *ast.Set:
		return evalTermsRecSet(t, head, 0, rec)
	case *ast.ArrayComprehension, *ast.ObjectComprehension, *ast.SetComprehension:
		return evalTermsComprehension(t, head, rec)
	default:
		return evalTermsRec(t, iter, tail)
	}
}

func evalTermsRecArray(t *Topdown, arr ast.Array, idx int, iter Iterator) error {
	if idx >= len(arr) {
		return iter(t)
	}

	rec := func(t *Topdown) error {
		return evalTermsRecArray(t, arr, idx+1, iter)
	}

	switch v := arr[idx].Value.(type) {
	case ast.Ref:
		return evalRef(t, v, ast.Ref{}, rec)
	case ast.Array:
		return evalTermsRecArray(t, v, 0, rec)
	case ast.Object:
		return evalTermsRecObject(t, v, 0, rec)
	case *ast.Set:
		return evalTermsRecSet(t, v, 0, rec)
	case *ast.ArrayComprehension, *ast.ObjectComprehension, *ast.SetComprehension:
		return evalTermsComprehension(t, v, rec)
	default:
		return evalTermsRecArray(t, arr, idx+1, iter)
	}
}

func evalTermsRecObject(t *Topdown, obj ast.Object, idx int, iter Iterator) error {
	if idx >= len(obj) {
		return iter(t)
	}

	rec := func(t *Topdown) error {
		return evalTermsRecObject(t, obj, idx+1, iter)
	}

	switch k := obj[idx][0].Value.(type) {
	case ast.Ref:
		return evalRef(t, k, ast.Ref{}, func(t *Topdown) error {
			switch v := obj[idx][1].Value.(type) {
			case ast.Ref:
				return evalRef(t, v, ast.Ref{}, rec)
			case ast.Array:
				return evalTermsRecArray(t, v, 0, rec)
			case ast.Object:
				return evalTermsRecObject(t, v, 0, rec)
			case *ast.Set:
				return evalTermsRecSet(t, v, 0, rec)
			case *ast.ArrayComprehension, *ast.ObjectComprehension, *ast.SetComprehension:
				return evalTermsComprehension(t, v, rec)
			default:
				return evalTermsRecObject(t, obj, idx+1, iter)
			}
		})
	default:
		switch v := obj[idx][1].Value.(type) {
		case ast.Ref:
			return evalRef(t, v, ast.Ref{}, rec)
		case ast.Array:
			return evalTermsRecArray(t, v, 0, rec)
		case ast.Object:
			return evalTermsRecObject(t, v, 0, rec)
		case *ast.Set:
			return evalTermsRecSet(t, v, 0, rec)
		case *ast.ArrayComprehension, *ast.ObjectComprehension, *ast.SetComprehension:
			return evalTermsComprehension(t, v, rec)
		default:
			return evalTermsRecObject(t, obj, idx+1, iter)
		}
	}
}

func evalTermsRecSet(t *Topdown, set *ast.Set, idx int, iter Iterator) error {
	if idx >= len(*set) {
		return iter(t)
	}

	rec := func(t *Topdown) error {
		return evalTermsRecSet(t, set, idx+1, iter)
	}

	switch v := (*set)[idx].Value.(type) {
	case ast.Ref:
		return evalRef(t, v, ast.Ref{}, rec)
	case ast.Array:
		return evalTermsRecArray(t, v, 0, rec)
	case ast.Object:
		return evalTermsRecObject(t, v, 0, rec)
	case *ast.ArrayComprehension, *ast.ObjectComprehension, *ast.SetComprehension:
		return evalTermsComprehension(t, v, rec)
	default:
		return evalTermsRecSet(t, set, idx+1, iter)
	}
}

// indexBuildLazy returns a storage index if an index exists or can be built
// for the ref.
func indexBuildLazy(t *Topdown, ref ast.Ref) (storage.Index, error) {

	// Ignore refs against variables.
	if !ref[0].Equal(ast.DefaultRootDocument) {
		return nil, nil
	}

	// Ignore refs against virtual docs.
	node := t.Compiler.RuleTree.Child(ref[0].Value)

	for i := 1; i < len(ref); i++ {
		if node == nil || !ref[i].IsGround() {
			break
		}
		if len(node.Values) > 0 {
			break
		}
		node = node.Child(ref[i].Value)
	}

	if node != nil {
		return nil, nil
	}

	index, err := t.Store.Build(t.Context, t.txn, ref)
	if err != nil {
		if storage.IsIndexingNotSupported(err) {
			return nil, nil
		}
		return nil, err
	}

	return index, nil
}

// indexingAllowed returns true if indexing can be used for the expression
// eq(ref, term).
func indexingAllowed(ref ast.Ref, term *ast.Term) bool {

	// Will not build indices for non-refs or refs that are ground as this would
	// be pointless. Also, storage does not support nested refs, so exclude
	// those.
	if ref == nil || ref.IsGround() || ref.IsNested() {
		return false
	}

	// Cannot perform index lookup for non-ground terms (except for refs which
	// will be evaluated in isolation).
	// TODO(tsandall): should be able to support non-ground terms that only
	// contain refs with output vars.
	if _, ok := term.Value.(ast.Ref); !ok && !term.IsGround() {
		return false
	}

	return true
}

type valueResolver struct {
	t *Topdown
}

func (r valueResolver) Resolve(ref ast.Ref) (ast.Value, error) {
	v, err := lookupValue(r.t, ref)
	if storage.IsNotFound(err) {
		return nil, nil
	}
	return v, nil
}

func lookupValue(t *Topdown, ref ast.Ref) (ast.Value, error) {
	if ref[0].Equal(ast.DefaultRootDocument) {
		r, err := t.Resolve(ref)
		if err != nil {
			return nil, err
		}
		return ast.InterfaceToValue(r)
	}
	if ref[0].Equal(ast.InputRootDocument) {
		if t.Input == nil {
			return nil, nil
		}
		r, err := t.Input.Find(ref[1:])
		if err != nil {
			return nil, nil
		}
		return r, nil
	}
	return nil, fmt.Errorf("bad ref in %v: %v bindings: %v refs: %v", t.Current(), ref, t.bindings, t.refs)
}

// valueMapStack is used to store a stack of bindings.
type valueMapStack struct {
	sl []*util.HashMap
}

func newValueMapStack() *valueMapStack {
	return &valueMapStack{}
}

func (s *valueMapStack) Push(vm *util.HashMap) {
	s.sl = append(s.sl, vm)
}

func (s *valueMapStack) Pop() *util.HashMap {
	idx := len(s.sl) - 1
	vm := s.sl[idx]
	s.sl = s.sl[:idx]
	return vm
}

func (s *valueMapStack) Peek() *util.HashMap {
	idx := len(s.sl) - 1
	if idx == -1 {
		return nil
	}
	return s.sl[idx]
}

func (s *valueMapStack) Binding(k ast.Value) (ast.Value, *unify.Unifier) {
	for i := len(s.sl) - 1; i >= 0; i-- {
		if v, ok := s.sl[i].Get(k); ok {
			elem := v.(valueMapStackElem)
			return elem.v, elem.u
		}
	}
	return nil, nil
}

type valueMapStackElem struct {
	u *unify.Unifier
	v ast.Value
}

func (e valueMapStackElem) IsZero() bool {
	return e.u == nil
}

func (e valueMapStackElem) String() string {
	return fmt.Sprintf("<%v e %v>", e.v, e.u)
}
func valueMapStackElemEq(a, b util.T) bool {
	return a.(ast.Ref).Equal(b.(ast.Ref))
}

func valueMapStackElemHash(x util.T) int {
	return x.(ast.Ref).Hash()
}

func newValueMap() *util.HashMap {
	return util.NewHashMap(valueMapStackElemEq, valueMapStackElemHash)
}

func (s *valueMapStack) Bind(k, v ast.Value, u *unify.Unifier, prev *Undo) *Undo {
	if len(s.sl) == 0 {
		s.Push(newValueMap())
	}
	vm := s.Peek()
	orig, ok := vm.Get(k)
	var elem valueMapStackElem
	if ok {
		elem = orig.(valueMapStackElem)
	}
	newElem := valueMapStackElem{u, v}
	vm.Put(k, newElem)
	return &Undo{k, elem, prev, nil}
}

func (s *valueMapStack) Unbind(u *Undo) {
	vm := s.Peek()
	elem := u.Value.(valueMapStackElem)
	if !elem.IsZero() {
		vm.Put(u.Key, u.Value)
	} else {
		vm.Delete(u.Key)
	}
}

func (s *valueMapStack) String() string {
	buf := make([]string, len(s.sl))
	for i := range s.sl {
		buf[i] = s.sl[i].String()
	}
	return "[" + strings.Join(buf, ", ") + "]"
}
