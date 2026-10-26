package splittest

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// Site is the production call site a split violation is charged to: the first
// frame on the caller's stack that is neither test code nor store plumbing.
type Site struct {
	// File is the repo-relative, slash-separated source file.
	File string
	// Line is the line within File.
	Line int
	// Func is the short function name (package-qualified, import path
	// dropped), e.g. "api.(*Server).resolveBeadOwner".
	Func string
}

// String renders the site as file:line.
func (s Site) String() string {
	if s.File == "" {
		return "?"
	}
	return fmt.Sprintf("%s:%d", s.File, s.Line)
}

// stackFacts is what one stack walk learns about a store call.
type stackFacts struct {
	site Site
	// via is the next production frame outside the site's own file: who
	// called the site. A domain front door (a session or mail store) built over
	// the wrong store is charged to the front door; via names who built it.
	via Site
	// test is the top-level test function the call ran under, or "" for a
	// goroutine whose stack does not reach one.
	test string
	// federated reports a beads.FederatedRead / FederatedGet frame: the call
	// site declared that a miss here is expected.
	federated bool
}

var (
	// sourceRoot is the source-path prefix every frame of this build shares,
	// derived from this file's own path. go test records absolute paths and
	// Bazel's rules_go records execroot-relative ones; either way stripping
	// the prefix this file carries yields a repo-relative path.
	sourceRoot = func() string {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			return ""
		}
		const self = "internal/beads/splittest/callsite.go"
		file = filepath.ToSlash(file)
		if strings.HasSuffix(file, self) {
			return strings.TrimSuffix(file, self)
		}
		return ""
	}()

	transparentMu sync.RWMutex
	// transparentFiles are repo-relative production files whose frames are
	// pass-through store plumbing (a policy wrapper, a gate seam) rather than
	// a call site: a violation is charged to whoever called INTO them.
	transparentFiles = map[string]bool{}
)

// TreatFilesAsPlumbing registers repo-relative production files whose frames a
// split violation skips over when it looks for the responsible call site, the
// way it already skips internal/beads and this package. A package registers its
// own pass-through wrappers (cmd/gc's bead policy store, its gate seam) before
// any violation is recorded.
func TreatFilesAsPlumbing(files ...string) {
	transparentMu.Lock()
	defer transparentMu.Unlock()
	for _, f := range files {
		transparentFiles[filepath.ToSlash(f)] = true
	}
}

// frameInfo is the part of a stack frame the site search reads.
type frameInfo struct {
	file     string // repo-relative when the frame is this repository's
	line     int
	function string
}

// captureStack walks the stack of its caller and reports the call site, the
// test, and whether the read was marked federated. The walk starts at the
// caller; store and gate frames are skipped by kind, not by count.
func captureStack() stackFacts {
	pcs := make([]uintptr, 96)
	n := runtime.Callers(2, pcs)
	pcs = pcs[:n]
	var stack []frameInfo
	frames := runtime.CallersFrames(pcs)
	for {
		frame, more := frames.Next()
		stack = append(stack, frameInfo{file: relativeSourcePath(frame.File), line: frame.Line, function: frame.Function})
		if !more {
			break
		}
	}
	facts := factsFromFrames(stack)
	facts.federated = beads.FederatedReadOnStack(pcs)
	return facts
}

// factsFromFrames picks the responsible call site (the innermost frame that is
// neither test code nor plumbing) and the outermost top-level test from a
// stack, innermost frame first.
func factsFromFrames(stack []frameInfo) stackFacts {
	var facts stackFacts
	for _, frame := range stack {
		if strings.HasSuffix(frame.file, "_test.go") {
			if name := topLevelTestName(frame.function); name != "" {
				facts.test = name
			}
			continue
		}
		if isPlumbingFrame(frame.file, frame.function) {
			continue
		}
		switch {
		case facts.site.File == "":
			facts.site = Site{File: frame.file, Line: frame.line, Func: shortFuncName(frame.function)}
		case facts.via.File == "" && frame.file != facts.site.File:
			facts.via = Site{File: frame.file, Line: frame.line, Func: shortFuncName(frame.function)}
		}
	}
	return facts
}

// relativeSourcePath strips the build's repo-root prefix from a frame's file.
func relativeSourcePath(file string) string {
	file = filepath.ToSlash(file)
	if sourceRoot != "" && strings.HasPrefix(file, sourceRoot) {
		return strings.TrimPrefix(file, sourceRoot)
	}
	return file
}

// isPlumbingFrame reports frames that are never the responsible call site: the
// Go runtime and testing harness, compiler-generated method wrappers (promoted
// methods of the typed class wrappers), internal/beads itself (stores and
// their wrappers), this package, and registered pass-through files.
func isPlumbingFrame(file, function string) bool {
	if file == "<autogenerated>" || file == "" {
		return true
	}
	for _, prefix := range []string{"runtime.", "testing.", "reflect.", "sync."} {
		if strings.HasPrefix(function, prefix) {
			return true
		}
	}
	if strings.HasPrefix(file, "internal/beads/") && !strings.Contains(strings.TrimPrefix(file, "internal/beads/"), "/") {
		return true
	}
	if strings.HasPrefix(file, "internal/beads/splittest/") {
		return true
	}
	if !isRepoSource(file) {
		// The standard library or a module dependency.
		return true
	}
	transparentMu.RLock()
	defer transparentMu.RUnlock()
	return transparentFiles[file]
}

// repoSourceDirs are the repository's top-level Go source trees. A frame
// outside them is the standard library or a dependency, whatever form the build
// recorded its path in.
var repoSourceDirs = []string{"cmd/", "internal/", "pkg/", "test/", "examples/", "contrib/", "tools/", "scripts/"}

// isRepoSource reports whether a repo-relative path names this repository's
// own source.
func isRepoSource(file string) bool {
	for _, dir := range repoSourceDirs {
		if strings.HasPrefix(file, dir) {
			return true
		}
	}
	return false
}

// topLevelTestName extracts "TestFoo" from a test-file frame's function name
// ("pkg.TestFoo", "pkg.TestFoo.func1.2"), or "" when the frame is a helper.
func topLevelTestName(function string) string {
	short := shortFuncName(function)
	dot := strings.Index(short, ".")
	if dot < 0 {
		return ""
	}
	rest := short[dot+1:]
	if !strings.HasPrefix(rest, "Test") {
		return ""
	}
	if end := strings.Index(rest, "."); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// shortFuncName drops the import path from a frame's function name.
func shortFuncName(function string) string {
	if slash := strings.LastIndex(function, "/"); slash >= 0 {
		return function[slash+1:]
	}
	return function
}
