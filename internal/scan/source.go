// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scan

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/vuln/internal"
	"golang.org/x/vuln/internal/client"
	"golang.org/x/vuln/internal/govulncheck"
	"golang.org/x/vuln/internal/osv"
	"golang.org/x/vuln/internal/vulncheck"
)

// runSource reports vulnerabilities that affect the analyzed packages.
//
// Vulnerabilities can be called (affecting the package, because a vulnerable
// symbol is actually exercised) or just imported by the package
// (likely having a non-affecting outcome).
func runSource(ctx context.Context, handler govulncheck.Handler, cfg *config, client *client.Client, dir string) error {
	var pkgs []*packages.Package
	pkgs, err := loadPackages(cfg, dir)
	if err != nil {
		// Try to provide a meaningful and actionable error message.
		if !fileExists(filepath.Join(dir, "go.mod")) {
			return fmt.Errorf("govulncheck: %v", errNoGoMod)
		}
		if isGoVersionMismatchError(err) {
			return fmt.Errorf("govulncheck: %v\n\n%v", errGoVersionMismatch, err)
		}
		return err
	}
	if err := handler.Progress(sourceProgressMessage(pkgs)); err != nil {
		return err
	}
	vr, err := vulncheck.Source(ctx, pkgs, &cfg.Config, client)
	if err != nil {
		return err
	}
	callStacks := vulncheck.CallStacks(vr)
	filterCallStacks(callStacks)
	return emitResult(handler, vr, callStacks)
}

// loadPackages loads the packages matching patterns at dir using build tags
// provided by tagsFlag. Uses load mode needed for vulncheck analysis. If the
// packages contain errors, a packageError is returned containing a list of
// the errors, along with the packages themselves.
func loadPackages(c *config, dir string) ([]*packages.Package, error) {
	graph := vulncheck.NewPackageGraph(c.GoVersion)
	cfg := &packages.Config{Dir: dir, Tests: c.test}
	return graph.LoadPackages(cfg, c.tags, c.patterns)
}

func filterCallStacks(callstacks map[*vulncheck.Vuln][]vulncheck.CallStack) {
	type key struct {
		id  string
		pkg string
		mod string
	}
	// Collect all called symbols for a package.
	// Needed for creating unique call stacks.
	vulnsPerPkg := make(map[key][]*vulncheck.Vuln)
	for vv := range callstacks {
		if vv.CallSink != nil {
			k := key{id: vv.OSV.ID, pkg: vv.ImportSink.PkgPath, mod: vv.ImportSink.Module.Path}
			vulnsPerPkg[k] = append(vulnsPerPkg[k], vv)
		}
	}
	for vv, stacks := range callstacks {
		var filtered []vulncheck.CallStack
		if vv.CallSink != nil {
			k := key{id: vv.OSV.ID, pkg: vv.ImportSink.PkgPath, mod: vv.ImportSink.Module.Path}
			vcs := uniqueCallStack(vv, stacks, vulnsPerPkg[k])
			if vcs != nil {
				filtered = []vulncheck.CallStack{vcs}
			}
		}
		callstacks[vv] = filtered
	}
}

func emitResult(handler govulncheck.Handler, vr *vulncheck.Result, callstacks map[*vulncheck.Vuln][]vulncheck.CallStack) error {
	osvs := map[string]*osv.Entry{}

	// Create Result where each vulncheck.Vuln{OSV, ModPath, PkgPath} becomes
	// a separate Vuln{OSV, Modules{Packages{PkgPath}}} entry. We merge the
	// results later.
	var findings []*govulncheck.Finding
	for _, vv := range vr.Vulns {
		p := &govulncheck.Package{Path: vv.ImportSink.PkgPath}
		m := &govulncheck.Module{
			Path:         vv.ImportSink.Module.Path,
			FoundVersion: foundVersion(vv),
			FixedVersion: fixedVersion(vv.ImportSink.Module.Path, vv.OSV.Affected),
			Packages:     []*govulncheck.Package{p},
		}
		v := &govulncheck.Finding{OSV: vv.OSV.ID, Modules: []*govulncheck.Module{m}}
		stacks := callstacks[vv]
		for _, stack := range stacks {
			p.CallStacks = append(p.CallStacks, govulncheck.CallStack{
				Frames: stackFramesfromEntries(stack),
			})
		}
		findings = append(findings, v)
		osvs[vv.OSV.ID] = vv.OSV
	}

	findings = merge(findings)
	sortResult(findings)
	// For each vulnerability, queue it to be written to the output.
	for _, f := range findings {
		if err := handler.OSV(osvs[f.OSV]); err != nil {
			return err
		}
		if err := handler.Finding(f); err != nil {
			return err
		}
	}
	return nil
}

// stackFramesFromEntries creates a sequence of stack
// frames from vcs. Position of a StackFrame is the
// call position of the corresponding stack entry.
func stackFramesfromEntries(vcs vulncheck.CallStack) []*govulncheck.StackFrame {
	var frames []*govulncheck.StackFrame
	for _, e := range vcs {
		fr := &govulncheck.StackFrame{
			Function: e.Function.Name,
			Package:  e.Function.PkgPath,
			Receiver: e.Function.Receiver(),
		}
		if e.Call == nil || e.Call.Pos == nil {
			fr.Position = nil
		} else {
			fr.Position = &govulncheck.Position{
				Filename: e.Call.Pos.Filename,
				Offset:   e.Call.Pos.Offset,
				Line:     e.Call.Pos.Line,
				Column:   e.Call.Pos.Column,
			}
		}
		frames = append(frames, fr)
	}
	return frames
}

// sourceProgressMessage returns a string of the form
//
//	"Scanning your code and P packages across M dependent modules for known vulnerabilities..."
//
// P is the number of strictly dependent packages of
// topPkgs and Y is the number of their modules.
func sourceProgressMessage(topPkgs []*packages.Package) *govulncheck.Progress {
	pkgs, mods := depPkgsAndMods(topPkgs)

	pkgsPhrase := fmt.Sprintf("%d package", pkgs)
	if pkgs != 1 {
		pkgsPhrase += "s"
	}

	modsPhrase := fmt.Sprintf("%d dependent module", mods)
	if mods != 1 {
		modsPhrase += "s"
	}

	msg := fmt.Sprintf("Scanning your code and %s across %s for known vulnerabilities...", pkgsPhrase, modsPhrase)
	return &govulncheck.Progress{Message: msg}
}

// depPkgsAndMods returns the number of packages that
// topPkgs depend on and the number of their modules.
func depPkgsAndMods(topPkgs []*packages.Package) (int, int) {
	tops := make(map[string]bool)
	depPkgs := make(map[string]bool)
	depMods := make(map[string]bool)

	for _, t := range topPkgs {
		tops[t.PkgPath] = true
	}

	var visit func(*packages.Package, bool)
	visit = func(p *packages.Package, top bool) {
		path := p.PkgPath
		if depPkgs[path] {
			return
		}
		if tops[path] && !top {
			// A top package that is a dependency
			// will not be in depPkgs, so we skip
			// reiterating on it here.
			return
		}

		// We don't count a top-level package as
		// a dependency even when they are used
		// as a dependent package.
		if !tops[path] {
			depPkgs[path] = true
			if p.Module != nil &&
				p.Module.Path != internal.GoStdModulePath && // no module for stdlib
				p.Module.Path != internal.UnknownModulePath { // no module for unknown
				depMods[p.Module.Path] = true
			}
		}

		for _, d := range p.Imports {
			visit(d, false)
		}
	}

	for _, t := range topPkgs {
		visit(t, true)
	}

	return len(depPkgs), len(depMods)
}

// summarizeCallStack returns a short description of the call stack.
// It uses one of four forms, depending on what the lowest function F
// in topPkgs calls and what is the highest function V of vulnPkg:
//   - If F calls V directly and F as well as V are not anonymous functions:
//     "F calls V"
//   - The same case as above except F calls function G in some other package:
//     "F calls G, which eventually calls V"
//   - If F is an anonymous function, created by function G, and H is the
//     lowest non-anonymous function in topPkgs:
//     "H calls G, which eventually calls V"
//   - If V is an anonymous function, created by function W:
//     "F calls W, which eventually calls V"
//
// If it can't find any of these functions, summarizeCallStack returns the empty string.
func summarizeCallStack(cs govulncheck.CallStack, topPkgs map[string]bool) string {
	if len(cs.Frames) == 0 {
		return ""
	}
	iTop, iTopEnd, topFunc, topEndFunc := summarizeTop(cs.Frames, topPkgs)
	if iTop < 0 {
		return ""
	}

	vulnPkg := cs.Frames[len(cs.Frames)-1].Package
	iVulnStart, vulnStartFunc, vulnFunc := summarizeVuln(cs.Frames, iTopEnd, vulnPkg)
	if iVulnStart < 0 {
		return ""
	}

	topPos := posToString(cs.Frames[iTop].Position)
	if topPos != "" {
		topPos += ": "
	}

	// The invariant is that the summary will always mention at most three functions
	// and never mention an anonymous function. It prioritizes summarizing top of the
	// stack as that is what the user has the most control of. For instance, if both
	// the top and vuln portions of the stack are each summarized with two functions,
	// then the final summary will mention the two functions of the top segment and
	// only one from the vuln segment.
	if topFunc != topEndFunc {
		// The last function of the top segment is anonymous.
		return fmt.Sprintf("%s%s calls %s, which eventually calls %s", topPos, topFunc, topEndFunc, vulnFunc)
	}
	if iVulnStart != iTopEnd+1 {
		// If there is something in between top and vuln segments of
		// the stack, then also summarize that intermediate segment.
		return fmt.Sprintf("%s%s calls %s, which eventually calls %s", topPos, topFunc, FuncName(cs.Frames[iTopEnd+1]), vulnFunc)
	}
	if vulnStartFunc != vulnFunc {
		// The first function of the vuln segment is anonymous.
		return fmt.Sprintf("%s%s calls %s, which eventually calls %s", topPos, topFunc, vulnStartFunc, vulnFunc)
	}
	return fmt.Sprintf("%s%s calls %s", topPos, topFunc, vulnFunc)
}

// summarizeTop returns summary information for the beginning segment
// of call stack frames that belong to topPkgs. It returns the latest,
// e.g., lowest function in this segment and its index in frames. If
// that function is anonymous, then summarizeTop also returns the
// lowest non-anonymous function and its index in frames. In that case,
// the anonymous function is replaced by the function that created it.
//
//	[p.V p.W q.Q ...]        -> (1, 1, p.W, p.W)
//	[p.V p.W p.Z$1 q.Q ...]  -> (1, 2, p.W, p.Z)
func summarizeTop(frames []*govulncheck.StackFrame, topPkgs map[string]bool) (iTop, iTopEnd int, topFunc, topEndFunc string) {
	iTopEnd = lowest(frames, func(e *govulncheck.StackFrame) bool {
		_, found := topPkgs[e.Package]
		return found
	})
	if iTopEnd < 0 {
		return -1, -1, "", ""
	}

	topEndFunc = FuncName(frames[iTopEnd])
	if !isAnonymousFunction(topEndFunc) {
		iTop = iTopEnd
		topFunc = topEndFunc
		return
	}

	topEndFunc = creatorName(topEndFunc)

	iTop = lowest(frames, func(e *govulncheck.StackFrame) bool {
		_, found := topPkgs[e.Package]
		return found && !isAnonymousFunction(e.Function)
	})
	if iTop < 0 {
		iTop = iTopEnd
		topFunc = topEndFunc // for sanity
		return
	}

	topFunc = FuncName(frames[iTop])
	return
}

// summarizeVuln returns summary information for the final segment
// of call stack frames that belong to vulnPkg. It returns the earliest,
// e.g., highest function in this segment and its index in frames. If
// that function is anonymous, then summarizeVuln also returns the
// highest non-anonymous function. In that case, the anonymous function
// is replaced by the function that created it.
//
//	[x x q.Q v.V v.W]   -> (3, v.V, v.V)
//	[x x q.Q v.V$1 v.W] -> (3, v.V, v.W)
func summarizeVuln(frames []*govulncheck.StackFrame, iTop int, vulnPkg string) (iVulnStart int, vulnStartFunc, vulnFunc string) {
	iVulnStart = highest(frames[iTop+1:], func(e *govulncheck.StackFrame) bool {
		return e.Package == vulnPkg
	})
	if iVulnStart < 0 {
		return -1, "", ""
	}
	iVulnStart += iTop + 1 // adjust for slice in call to highest.

	vulnStartFunc = FuncName(frames[iVulnStart])
	if !isAnonymousFunction(vulnStartFunc) {
		vulnFunc = vulnStartFunc
		return
	}

	vulnStartFunc = creatorName(vulnStartFunc)

	iVuln := highest(frames[iVulnStart:], func(e *govulncheck.StackFrame) bool {
		return e.Package == vulnPkg && !isAnonymousFunction(e.Function)
	})
	if iVuln < 0 {
		vulnFunc = vulnStartFunc // for sanity
		return
	}

	vulnFunc = FuncName(frames[iVuln+iVulnStart])
	return
}

// updateInitPositions populates non-existing positions of init functions
// and their respective calls in callStacks (see #51575).
func updateInitPositions(callStacks map[*vulncheck.Vuln][]vulncheck.CallStack, pkgs []*packages.Package) {
	pMap := pkgMap(pkgs)
	for _, css := range callStacks {
		for _, cs := range css {
			for i := range cs {
				updateInitPosition(&cs[i], pMap)
				if i != len(cs)-1 {
					updateInitCallPosition(&cs[i], cs[i+1], pMap)
				}
			}
		}
	}
}

// updateInitCallPosition updates the position of a call to init in a stack frame, if
// one already does not exist:
//
//	P1.init -> P2.init: position of call to P2.init is the position of "import P2"
//	statement in P1
//
//	P.init -> P.init#d: P.init is an implicit init. We say it calls the explicit
//	P.init#d at the place of "package P" statement.
func updateInitCallPosition(curr *vulncheck.StackEntry, next vulncheck.StackEntry, pkgs map[string]*packages.Package) {
	call := curr.Call
	if !isInit(next.Function) || (call.Pos != nil && call.Pos.IsValid()) {
		// Skip non-init functions and inits whose call site position is available.
		return
	}

	pkg := pkgs[curr.Function.PkgPath]
	var pos token.Position
	if curr.Function.Name == "init" && curr.Function.PkgPath == next.Function.PkgPath {
		// We have implicit P.init calling P.init#d. Set the call position to
		// be at "package P" statement position.
		pos = packageStatementPos(pkg)
	} else {
		// Choose the beginning of the import statement as the position.
		pos = importStatementPos(pkg, next.Function.PkgPath)
	}

	call.Pos = &pos
}

func importStatementPos(pkg *packages.Package, importPath string) token.Position {
	var importSpec *ast.ImportSpec
spec:
	for _, f := range pkg.Syntax {
		for _, impSpec := range f.Imports {
			// Import spec paths have quotation marks.
			impSpecPath, err := strconv.Unquote(impSpec.Path.Value)
			if err != nil {
				panic(fmt.Sprintf("import specification: package path has no quotation marks: %v", err))
			}
			if impSpecPath == importPath {
				importSpec = impSpec
				break spec
			}
		}
	}

	if importSpec == nil {
		// for sanity, in case of a wild call graph imprecision
		return token.Position{}
	}

	// Choose the beginning of the import statement as the position.
	return pkg.Fset.Position(importSpec.Pos())
}

func packageStatementPos(pkg *packages.Package) token.Position {
	if len(pkg.Syntax) == 0 {
		return token.Position{}
	}
	// Choose beginning of the package statement as the position. Pick
	// the first file since it is as good as any.
	return pkg.Fset.Position(pkg.Syntax[0].Package)
}

// updateInitPosition updates the position of P.init function in a stack frame if one
// is not available. The new position is the position of the "package P" statement.
func updateInitPosition(se *vulncheck.StackEntry, pkgs map[string]*packages.Package) {
	fun := se.Function
	if !isInit(fun) || (fun.Pos != nil && fun.Pos.IsValid()) {
		// Skip non-init functions and inits whose position is available.
		return
	}

	pos := packageStatementPos(pkgs[fun.PkgPath])
	fun.Pos = &pos
}

func isInit(f *vulncheck.FuncNode) bool {
	// A source init function, or anonymous functions used in inits, will
	// be named "init#x" by vulncheck (more precisely, ssa), where x is a
	// positive integer. Implicit inits are named simply "init".
	return f.Name == "init" || strings.HasPrefix(f.Name, "init#")
}

// creatorName returns the name of the function that created
// the anonymous function anonFuncName. Assumes anonFuncName
// is of the form <name>$1...
func creatorName(anonFuncName string) string {
	vs := strings.Split(anonFuncName, "$")
	if len(vs) == 1 {
		return anonFuncName
	}
	return vs[0]
}

func isAnonymousFunction(funcName string) bool {
	// anonymous functions have $ sign in their name (naming done by ssa)
	return strings.ContainsRune(funcName, '$')
}

// uniqueCallStack returns the first unique call stack among css, if any.
// Unique means that the call stack does not go through symbols of vg.
func uniqueCallStack(v *vulncheck.Vuln, css []vulncheck.CallStack, vg []*vulncheck.Vuln) vulncheck.CallStack {
	vulnFuncs := make(map[*vulncheck.FuncNode]bool)
	for _, v := range vg {
		vulnFuncs[v.CallSink] = true
	}

callstack:
	for _, cs := range css {
		for _, e := range cs {
			if e.Function != v.CallSink && vulnFuncs[e.Function] {
				continue callstack
			}
		}
		return cs
	}
	return nil
}
