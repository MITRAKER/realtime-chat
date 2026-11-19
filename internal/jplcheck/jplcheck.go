// Package jplcheck is a static analyzer for the parts of NASA/JPL's
// "Power of Ten" rules that no off-the-shelf Go linter checks:
//
//   - Rule 1, simple control flow: no goto, and no recursion, whether a
//     function calls itself directly or through other functions.
//   - Rule 2, bounded loops: a loop with no condition (`for { ... }`) or one
//     that is always true, and a range over a channel or a function
//     iterator, can run forever. Each one must explain how it ends, in a
//     comment that starts with "jpl:loop" on the line just above it (or
//     above its label).
//
// Run it with `go run ./cmd/jplcheck ./...` (the Makefile and CI do).
// See docs/CODING_STANDARDS.md for all ten rules and how this repo applies them.
package jplcheck

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
)

// LoopDirective is the comment prefix that documents an unbounded loop.
const LoopDirective = "jpl:loop"

// Analyzer reports goto statements, recursion, and undocumented unbounded loops.
var Analyzer = &analysis.Analyzer{
	Name:     "jplcheck",
	Doc:      "checks NASA/JPL Power of Ten rule 1 (no goto, no recursion) and rule 2 (unbounded loops must document how they end)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	insp, ok := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	if !ok {
		return nil, fmt.Errorf("jplcheck: inspect result has type %T", pass.ResultOf[inspect.Analyzer])
	}
	checkGoto(pass, insp)
	checkLoops(pass, insp)
	checkRecursion(pass, insp)
	// This analyzer only reports problems; it has no result for other
	// analyzers (its ResultType is nil), so there is nothing to return.
	return nil, nil //nolint:nilnil // the go/analysis API expects (nil, nil) here
}

// checkGoto reports every goto statement (rule 1).
func checkGoto(pass *analysis.Pass, insp *inspector.Inspector) {
	insp.Preorder([]ast.Node{(*ast.BranchStmt)(nil)}, func(n ast.Node) {
		if b, ok := n.(*ast.BranchStmt); ok && b.Tok == token.GOTO {
			pass.Reportf(b.Pos(), "goto is not allowed (JPL rule 1: simple control flow)")
		}
	})
}

// checkLoops reports loops that can run forever without a "jpl:loop"
// comment saying how they end (rule 2). The comment goes on the line just
// above the loop, or above its label: gofmt puts a label on a line of its
// own, and above the label is where people document a labeled loop.
func checkLoops(pass *analysis.Pass, insp *inspector.Inspector) {
	documented := loopComments(pass)
	labels := loopLabels(insp)
	insp.Preorder([]ast.Node{(*ast.ForStmt)(nil), (*ast.RangeStmt)(nil)}, func(n ast.Node) {
		kind := unboundedKind(pass, n)
		if kind == "" || commentAbove(pass, documented, n) {
			return
		}
		if label, ok := labels[n]; ok && commentAbove(pass, documented, label) {
			return
		}
		pass.Reportf(n.Pos(), "%s can run forever: add a %q comment on the line above that says how it ends (JPL rule 2: bounded loops)", kind, LoopDirective+" <how it ends>")
	})
}

// loopLabels maps each labeled statement to its label.
func loopLabels(insp *inspector.Inspector) map[ast.Node]*ast.LabeledStmt {
	labels := map[ast.Node]*ast.LabeledStmt{}
	insp.Preorder([]ast.Node{(*ast.LabeledStmt)(nil)}, func(n ast.Node) {
		if ls, ok := n.(*ast.LabeledStmt); ok {
			labels[ls.Stmt] = ls
		}
	})
	return labels
}

// commentAbove reports whether a "jpl:loop" comment ends on the line just
// above n.
func commentAbove(pass *analysis.Pass, documented map[lineKey]bool, n ast.Node) bool {
	pos := pass.Fset.Position(n.Pos())
	return documented[lineKey{pos.Filename, pos.Line - 1}]
}

// unboundedKind describes why a loop may be unbounded, or returns "" if it is
// bounded by its own syntax: a condition that can become false, or a range
// over a finite value (a slice, an array, a string, a map or a number).
func unboundedKind(pass *analysis.Pass, n ast.Node) string {
	switch loop := n.(type) {
	case *ast.ForStmt:
		if loop.Cond == nil {
			return "loop without a condition"
		}
		// `for i := 0; true; i++` has a condition, but not one that can end it.
		if v := pass.TypesInfo.Types[loop.Cond].Value; v != nil && v.Kind() == constant.Bool && constant.BoolVal(v) {
			return "loop whose condition is always true"
		}
	case *ast.RangeStmt:
		return rangeKind(pass.TypesInfo.TypeOf(loop.X))
	}
	return ""
}

// rangeKind describes a range over a value of type t that can go on
// forever: over a channel (until someone closes it), or over a function
// iterator (until it stops calling yield). It returns "" for anything
// else. For a type parameter, it looks at the types its constraint allows.
func rangeKind(t types.Type) string {
	if t == nil {
		return "" // code that didn't type-check
	}
	for _, possible := range possibleTypes(t) {
		switch possible.Underlying().(type) {
		case *types.Chan:
			return "range over a channel"
		case *types.Signature:
			return "range over a function iterator"
		}
	}
	return ""
}

// possibleTypes returns the types a value of type t can have: t itself, or,
// for a type parameter, the types its constraint lists. Constraints can be
// unions (~chan int | ~chan string) and can embed other constraints, so it
// keeps a work list. Each type is looked at once (seen), so the loop ends.
func possibleTypes(t types.Type) []types.Type {
	var found []types.Type
	work := []types.Type{t}
	seen := map[types.Type]bool{}
	for len(work) > 0 {
		next := types.Unalias(work[len(work)-1])
		work = work[:len(work)-1]
		if seen[next] {
			continue
		}
		seen[next] = true
		if inner, isConstraint := constraintParts(next); isConstraint {
			work = append(work, inner...)
		} else {
			found = append(found, next)
		}
	}
	return found
}

// constraintParts returns what a constraint is made of: a type parameter's
// constraint, a union's terms, or the types an interface embeds. ok is
// false if t isn't any of those.
func constraintParts(t types.Type) (parts []types.Type, ok bool) {
	switch t := t.(type) {
	case *types.TypeParam:
		return []types.Type{t.Constraint()}, true
	case *types.Union:
		for i := range t.Len() {
			parts = append(parts, t.Term(i).Type())
		}
		return parts, true
	}
	iface, ok := t.Underlying().(*types.Interface)
	if !ok {
		return nil, false
	}
	for i := range iface.NumEmbeddeds() {
		parts = append(parts, iface.EmbeddedType(i))
	}
	return parts, true
}

type lineKey struct {
	file string
	line int
}

// loopComments finds every line that ends a "jpl:loop" comment.
func loopComments(pass *analysis.Pass) map[lineKey]bool {
	lines := make(map[lineKey]bool)
	for _, f := range pass.Files {
		for _, group := range f.Comments {
			for _, c := range group.List {
				text := strings.TrimPrefix(strings.TrimPrefix(c.Text, "//"), "/*")
				text = strings.TrimSpace(strings.TrimSuffix(text, "*/"))
				if reason, ok := strings.CutPrefix(text, LoopDirective); ok && strings.TrimSpace(reason) != "" {
					end := pass.Fset.Position(group.End())
					lines[lineKey{end.Filename, end.Line}] = true
				}
			}
		}
	}
	return lines
}

// callGraph maps each function in the package to the package functions it
// calls, and remembers where each function is declared.
type callGraph struct {
	edges map[*types.Func]map[*types.Func]bool
	decl  map[*types.Func]token.Pos
}

// checkRecursion reports every group of functions that can call
// themselves, directly or through each other (rule 1), with one chain of
// calls for each group. Calls through interfaces and function values can't
// be resolved statically and are not followed.
func checkRecursion(pass *analysis.Pass, insp *inspector.Inspector) {
	g := buildCallGraph(pass, insp)
	for _, group := range cycles(g) {
		path := cyclePath(g, group)
		names := make([]string, len(path))
		for i, fn := range path {
			names[i] = displayName(fn)
		}
		pass.Reportf(g.decl[group[0]], "recursion: %s (JPL rule 1: no recursion)", strings.Join(names, " -> "))
	}
}

// cyclePath returns one chain of calls that leads from group[0] back to
// itself, such as [a, c, b, a], using only functions in group. A
// strongly connected group always has one. A breadth-first search finds a
// shortest chain; each function is queued at most once.
func cyclePath(g callGraph, group []*types.Func) []*types.Func {
	start := group[0]
	inGroup := make(map[*types.Func]bool, len(group))
	for _, fn := range group {
		inGroup[fn] = true
	}
	cameFrom := map[*types.Func]*types.Func{}
	queue := []*types.Func{start}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		for _, next := range callees(g, fn) {
			if next == start {
				return pathBack(cameFrom, start, fn)
			}
			if _, seen := cameFrom[next]; !seen && inGroup[next] {
				cameFrom[next] = fn
				queue = append(queue, next)
			}
		}
	}
	return append(group, start) // unreachable: group is strongly connected
}

// pathBack follows cameFrom links from last back to start, and returns the
// chain in call order, ending with the call back to start.
func pathBack(cameFrom map[*types.Func]*types.Func, start, last *types.Func) []*types.Func {
	path := []*types.Func{start}
	for fn := last; fn != start; fn = cameFrom[fn] {
		path = append(path, fn)
	}
	path = append(path, start)
	// The chain was collected backwards; reverse the part between the two starts.
	slices.Reverse(path[1 : len(path)-1])
	return path
}

func buildCallGraph(pass *analysis.Pass, insp *inspector.Inspector) callGraph {
	g := callGraph{edges: map[*types.Func]map[*types.Func]bool{}, decl: map[*types.Func]token.Pos{}}
	insp.WithStack([]ast.Node{(*ast.CallExpr)(nil), (*ast.FuncDecl)(nil)}, func(n ast.Node, push bool, stack []ast.Node) bool {
		if !push {
			return true
		}
		if fd, ok := n.(*ast.FuncDecl); ok {
			if fn, ok := pass.TypesInfo.Defs[fd.Name].(*types.Func); ok {
				g.decl[fn] = fd.Name.Pos()
			}
			return true
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		caller := enclosingFunc(pass, stack)
		// StaticCallee resolves calls to functions and to methods of
		// concrete types, however they are written: f(x), f[A, B](x),
		// t.m(x), T.m(t, x) or (*T).m(t, x). For a generic function, or a
		// method of a generic type, it returns the declared function, so
		// every instantiation is the same node in the graph. It returns nil
		// for calls through interfaces, function values and builtins.
		callee := typeutil.StaticCallee(pass.TypesInfo, call)
		if caller == nil || callee == nil || callee.Pkg() != pass.Pkg {
			return true
		}
		if g.edges[caller] == nil {
			g.edges[caller] = map[*types.Func]bool{}
		}
		g.edges[caller][callee] = true
		return true
	})
	return g
}

// enclosingFunc returns the declared function that contains the node at the
// top of the stack. Calls inside a function literal count as calls by the
// function that contains the literal.
func enclosingFunc(pass *analysis.Pass, stack []ast.Node) *types.Func {
	for i := len(stack) - 1; i >= 0; i-- {
		if fd, ok := stack[i].(*ast.FuncDecl); ok {
			fn, _ := pass.TypesInfo.Defs[fd.Name].(*types.Func)
			return fn
		}
	}
	return nil
}

// cycles returns the strongly connected components of the call graph that
// contain a cycle, using Tarjan's algorithm with an explicit stack so this
// checker itself has no recursion. Each cycle is sorted for stable output.
func cycles(g callGraph) [][]*types.Func {
	nodes := make([]*types.Func, 0, len(g.edges))
	for fn := range g.edges {
		nodes = append(nodes, fn)
	}
	slices.SortFunc(nodes, byName)

	t := tarjan{g: g, index: map[*types.Func]int{}, low: map[*types.Func]int{}, onStack: map[*types.Func]bool{}}
	for _, fn := range nodes {
		if _, seen := t.index[fn]; !seen {
			t.visit(fn)
		}
	}
	return t.found
}

type frame struct {
	fn    *types.Func
	succ  []*types.Func
	child int
}

type tarjan struct {
	g       callGraph
	next    int
	index   map[*types.Func]int
	low     map[*types.Func]int
	onStack map[*types.Func]bool
	stack   []*types.Func
	found   [][]*types.Func
}

// visit runs Tarjan's strongly-connected-components search from root. The
// work list is bounded: each function is pushed at most once.
func (t *tarjan) visit(root *types.Func) {
	work := []*frame{t.enter(root)}
	// jpl:loop ends when the work list is empty; every function is pushed at most once.
	for len(work) > 0 {
		top := work[len(work)-1]
		if top.child < len(top.succ) {
			next := top.succ[top.child]
			top.child++
			if _, seen := t.index[next]; !seen {
				work = append(work, t.enter(next))
			} else if t.onStack[next] {
				t.low[top.fn] = min(t.low[top.fn], t.index[next])
			}
			continue
		}
		work = work[:len(work)-1]
		if len(work) > 0 {
			parent := work[len(work)-1].fn
			t.low[parent] = min(t.low[parent], t.low[top.fn])
		}
		if t.low[top.fn] == t.index[top.fn] {
			t.popComponent(top.fn)
		}
	}
}

func (t *tarjan) enter(fn *types.Func) *frame {
	t.index[fn], t.low[fn] = t.next, t.next
	t.next++
	t.stack = append(t.stack, fn)
	t.onStack[fn] = true
	return &frame{fn: fn, succ: callees(t.g, fn)}
}

// callees returns the functions fn calls, sorted by name so that the
// output is the same on every run (Go randomizes map iteration order).
func callees(g callGraph, fn *types.Func) []*types.Func {
	succ := make([]*types.Func, 0, len(g.edges[fn]))
	for s := range g.edges[fn] {
		succ = append(succ, s)
	}
	slices.SortFunc(succ, byName)
	return succ
}

// byName orders functions by their displayName, and functions with the
// same name (several init functions, say) by where they are declared, so
// that sorting gives the same order on every run.
func byName(a, b *types.Func) int {
	return cmp.Or(strings.Compare(displayName(a), displayName(b)), cmp.Compare(a.Pos(), b.Pos()))
}

func (t *tarjan) popComponent(root *types.Func) {
	var comp []*types.Func
	for len(t.stack) > 0 {
		fn := t.stack[len(t.stack)-1]
		t.stack = t.stack[:len(t.stack)-1]
		t.onStack[fn] = false
		comp = append(comp, fn)
		if fn == root {
			break
		}
	}
	if len(comp) > 1 || t.g.edges[root][root] {
		slices.SortFunc(comp, byName)
		t.found = append(t.found, comp)
	}
}

// displayName is how a function appears in reports: "name" for a function,
// "Type.name" for a method, whatever alias the receiver was declared with.
func displayName(fn *types.Func) string {
	recv := fn.Signature().Recv()
	if recv == nil {
		return fn.Name()
	}
	t := types.Unalias(recv.Type())
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name() + "." + fn.Name()
	}
	return fn.Name()
}
