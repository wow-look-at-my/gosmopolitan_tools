// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

// Bisect finds changes responsible for causing a failure. A typical use is to
// identify the source locations in a program that are miscompiled by a given
// compiler optimization.
//
// Usage:
//
//	bisect [flags] [var=value...] command [arguments...]
//
// Bisect operates on a target command line – the target – that can be run
// with various changes individually enabled or disabled. With none of the
// changes enabled, the target is known to succeed (exit with exit code zero).
// With all the changes enabled, the target is known to fail (exit any other
// way).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/bits"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/tools/internal/bisect"
)

// Preserve import of bisect, to allow [bisect.Match] in the doc comment.
var _ bisect.Matcher

func usage() {
	fmt.Fprintf(os.Stderr, "usage: bisect [flags] [var=value...] command [arguments...]\n")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("bisect: ")

	var b Bisect
	b.Stdout = os.Stdout
	b.Stderr = os.Stderr
	flag.IntVar(&b.Max, "max", 0, "stop after finding `m` failing change sets")
	flag.IntVar(&b.MaxSet, "maxset", 0, "do not search for change sets larger than `s` elements")
	flag.DurationVar(&b.Timeout, "timeout", 0, "stop target and consider failed after duration `d`")
	flag.IntVar(&b.Count, "count", 2, "run target `n` times for each trial")
	flag.BoolVar(&b.Verbose, "v", false, "enable verbose output")

	env := ""
	envFlag := ""
	flag.Func("compile", "bisect source locations affected by Go compiler `rewrite` (fma, loopvar, ...)", func(value string) error {
		if envFlag != "" {
			return fmt.Errorf("cannot use -%s and -compile", envFlag)
		}
		envFlag = "compile"
		env = "GOCOMPILEDEBUG=" + value + "hash=PATTERN"
		return nil
	})
	flag.Func("godebug", "bisect call stacks affected by GODEBUG setting `name=value`", func(value string) error {
		if envFlag != "" {
			return fmt.Errorf("cannot use -%s and -godebug", envFlag)
		}
		envFlag = "godebug"
		env = "GODEBUG=" + value + "#PATTERN"
		return nil
	})

	flag.Usage = usage
	flag.Parse()
	args := flag.Args()

	// Split command line into env settings, command name, args.
	i := 0
	for i < len(args) && strings.Contains(args[i], "=") {
		i++
	}
	if i == len(args) {
		usage()
	}
	b.Env, b.Cmd, b.Args = args[:i], args[i], args[i+1:]
	if env != "" {
		b.Env = append([]string{env}, b.Env...)
	}

	// Check that PATTERN is available for us to vary.
	found := false
	for _, e := range b.Env {
		if _, v, _ := strings.Cut(e, "="); strings.Contains(v, "PATTERN") {
			found = true
		}
	}
	for _, a := range b.Args {
		if strings.Contains(a, "PATTERN") {
			found = true
		}
	}
	if !found {
		log.Fatalf("no PATTERN in target environment or args")
	}

	if !b.Search() {
		os.Exit(1)
	}
}

// A Bisect holds the state for a bisect invocation.
type Bisect struct {
	// Env is the additional environment variables for the command.
	Env []string

	// Cmd is the command (program name) to run. PATTERN and RANDOM are not substituted.
	Cmd string

	// Args is the command arguments. PATTERN and RANDOM are substituted anywhere they appear.
	Args []string

	// Command-line flags controlling bisect behavior.
	Max     int
	MaxSet  int
	Timeout time.Duration
	Count   int  // run target this many times for each trial and give up if flaky (one min assumed; a later default on command line set in main)
	Verbose bool // print long output about each trial (only useful for debugging bisect itself)

	// State for running bisect, replaced during testing.
	Stdout  io.Writer                                                             // where to write standard output (usually os.Stdout)
	Stderr  io.Writer                                                             // where to write standard error (usually os.Stderr)
	TestRun func(env []string, cmd string, args []string) (out []byte, err error) // if non-nil, used instead of exec.Command

	// State maintained by Search.

	// By default, Search looks for a minimal set of changes that cause a failure when enabled.
	Disable bool

	// SkipHexDigits is the number of hex digits to use in skip messages.
	SkipHexDigits int

	// Add is a list of suffixes to add to every trial.
	Add []string

	// Skip is a list of suffixes that uniquely identify changes to exclude from every trial.
	Skip []string
}

// A Result holds the result of a single target trial.
type Result struct {
	Success bool   // whether the target succeeded (exited with zero status)
	Cmd     string // full target command line
	Out     string // full target output (stdout and stderr combined)

	Suffix    string   // the suffix used for collecting MatchIDs, MatchText, and MatchFull
	MatchIDs  []uint64 // match IDs enabled during this trial
	MatchText []string // match reports for the IDs, with match markers removed
	MatchFull []string // full match lines for the IDs, with match markers kept
}

// &searchFatal is a special panic value to signal that Search failed.
var searchFatal int

// Search runs a bisect search according to the configuration in b.
// It reports whether any failing change sets were found.
func (b *Bisect) Search() bool {
	defer func() {
		// Recover from panic(&searchFatal), implicitly returning false from Search.
		// Re-panic on any other panic.
		if e := recover(); e != nil && e != &searchFatal {
			panic(e)
		}
	}()

	// Run with no changes and all changes, to figure out which direction we're searching.

	b.Logf("checking target with all changes disabled")
	runN := b.Run("n")

	b.Logf("checking target with all changes enabled")
	runY := b.Run("y")

	var broken *Result
	switch {
	case runN.Success && !runY.Success:
		b.Logf("target succeeds with no changes, fails with all changes")
		b.Logf("searching for minimal set of enabled changes causing failure")
		broken = runY
		b.Disable = false

	case !runN.Success && runY.Success:
		b.Logf("target fails with no changes, succeeds with all changes")
		b.Logf("searching for minimal set of disabled changes causing failure")
		broken = runN
		b.Disable = true

	case runN.Success && runY.Success:
		b.Fatalf("target succeeds with no changes and all changes")

	case !runN.Success && !runY.Success:
		b.Fatalf("target fails with no changes and all changes")
	}

	// Compute minimum number of bits needed to distinguish all the changes we saw during N and all the changes we saw during Y.
	b.SkipHexDigits = skipHexDigits(runN.MatchIDs, runY.MatchIDs)

	// Loop finding and printing change sets, until none remain.
	found := 0
	for {
		// Find set.
		bad := b.search(broken)
		if bad == nil {
			if found == 0 {
				b.Fatalf("cannot find any failing change sets of size ≤ %d", b.MaxSet)
			}
			break
		}

		// Confirm that set does fail, to avoid false accusations. Also asking for user-visible output; earlier runs did not.
		b.Logf("confirming failing change set")
		b.Add = append(b.Add[:0], bad...)
		broken = b.Run("v")
		if broken.Success {
			b.Logf("confirmation run succeeded unexpectedly")
		}
		b.Add = b.Add[:0]

		// Print confirmed change set.
		found++
		b.Logf("FOUND failing change set")
		desc := "(enabling changes causes failure)"
		if b.Disable {
			desc = "(disabling changes causes failure)"
		}
		fmt.Fprintf(b.Stdout, "--- change set #%d %s\n%s\n---\n", found, desc, strings.Join(broken.MatchText, "\n"))

		// Stop if we've found enough change sets.
		if b.Max > 0 && found >= b.Max {
			break
		}

		// If running bisect target | tee bad.txt, prints to stdout and stderr both appear on the terminal, but the ones to stdout go through tee.
		time.Sleep(1 * time.Millisecond)

		// Disable the now-known-bad changes and see if any failures remain.
		b.Logf("checking for more failures")
		b.Skip = append(bad, b.Skip...)
		broken = b.Run("")
		if broken.Success {
			what := "enabled"
			if b.Disable {
				what = "disabled"
			}
			b.Logf("target succeeds with all remaining changes %s", what)
			break
		}
		b.Logf("target still fails; searching for more bad changes")
	}
	return true
}

// Fatalf prints a message to standard error and then panics,
// causing Search to return false.
func (b *Bisect) Fatalf(format string, args ...any) {
	s := fmt.Sprintf("bisect: fatal error: "+format, args...)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	b.Stderr.Write([]byte(s))
	panic(&searchFatal)
}

// Logf prints a message to standard error.
func (b *Bisect) Logf(format string, args ...any) {
	s := fmt.Sprintf("bisect: "+format, args...)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	b.Stderr.Write([]byte(s))
}

func skipHexDigits(idY, idN []uint64) int {
	var all []uint64
	seen := make(map[uint64]bool)
	for _, x := range idY {
		seen[x] = true
		all = append(all, x)
	}
	for _, x := range idN {
		if !seen[x] {
			seen[x] = true
			all = append(all, x)
		}
	}
	sort.Slice(all, func(i, j int) bool { return bits.Reverse64(all[i]) < bits.Reverse64(all[j]) })
	digits := sort.Search(64/4, func(digits int) bool {
		mask := uint64(1)<<(4*digits) - 1
		for i := 0; i+1 < len(all); i++ {
			if all[i]&mask == all[i+1]&mask {
				return false
			}
		}
		return true
	})
	if digits < 64/4 {
		digits++
	}
	return digits
}

// search searches for a single locally minimal change set.
//
// Invariant: r describes the result of r.Suffix + b.Add, which failed.
// (There's an implicit -b.Skip everywhere here. b.Skip does not change.)
// We want to extend r.Suffix to preserve the failure, working toward
// a suffix that identifies a single change.
func (b *Bisect) search(r *Result) []string {
	// The caller should be passing in a failure result that we diagnose.
	if r.Success {
		b.Fatalf("internal error: unexpected success") // mistake by caller
	}

	// If the failure reported no changes, the target is misbehaving.
	if len(r.MatchIDs) == 0 {
		b.Fatalf("failure with no reported changes:\n\n$ %s\n%s\n", r.Cmd, r.Out)
	}

	// If there's one matching change, that's the one we're looking for.
	if len(r.MatchIDs) == 1 {
		return []string{fmt.Sprintf("x%0*x", b.SkipHexDigits, r.MatchIDs[0]&(1<<(4*b.SkipHexDigits)-1))}
	}

	// If the suffix we were tracking in the trial is already 64 bits,
	// either the target is bad or bisect itself is buggy.
	if len(r.Suffix) >= 64 {
		b.Fatalf("failed to isolate a single change with very long suffix")
	}

	suffix := commonSuffix(r.MatchIDs)
	if !strings.HasSuffix(suffix, r.Suffix) {
		b.Fatalf("internal error: invalid common suffix") // bug in commonSuffix
	}

	// Run 0suffix and 1suffix. If one fails, chase down the failure in that half.
	r0 := b.Run("0" + suffix)
	if !r0.Success {
		return b.search(r0)
	}
	r1 := b.Run("1" + suffix)
	if !r1.Success {
		return b.search(r1)
	}

	// suffix failed, but 0suffix and 1suffix succeeded.
	if b.MaxSet > 0 && 2+len(b.Add) > b.MaxSet {
		return nil
	}

	// Adding all matches for 1suffix, recurse to narrow down 0suffix.
	old := len(b.Add)
	b.Add = append(b.Add, "1"+suffix)
	r0 = b.Run("0" + suffix)
	if r0.Success {
		// 0suffix + b.Add + 1suffix = suffix + b.Add is what r describes, and it failed.
		b.Fatalf("target fails inconsistently")
	}
	bad0 := b.search(r0)
	if bad0 == nil {
		// Search failed due to MaxSet limit.
		return nil
	}
	b.Add = b.Add[:old]

	// Adding the specific match we found in 0suffix, recurse to narrow down 1suffix.
	b.Add = append(b.Add[:old], bad0...)
	r1 = b.Run("1" + suffix)
	if r1.Success {
		// 1suffix + b.Add + bad0 = bad0 + b.Add + 1suffix is what b.search(r0) reported as a failure.
		b.Fatalf("target fails inconsistently")
	}
	bad1 := b.search(r1)
	if bad1 == nil {
		// Search failed due to MaxSet limit.
		return nil
	}
	b.Add = b.Add[:old]

	// bad0 and bad1 together provoke the failure.
	return append(bad0, bad1...)
}

// Run runs a set of trials selecting changes with the given suffix, plus the
// ones in b.Add and not the ones in b.Skip.
func (b *Bisect) Run(suffix string) *Result {
	out := b.run(suffix)
	for i := 1; i < b.Count; i++ {
		r := b.run(suffix)
		if r.Success != out.Success {
			b.Fatalf("target fails inconsistently")
		}
	}
	return out
}

// run runs a single trial for Run.
func (b *Bisect) run(suffix string) *Result {
	random := fmt.Sprint(rand.Uint64())

	// Accept suffix == "v" to mean we need user-visible output.
	visible := ""
	if suffix == "v" {
		visible = "v"
		suffix = ""
	}

	// Construct change ID pattern.
	var pattern string
	if suffix == "y" || suffix == "n" {
		pattern = suffix
		suffix = ""
	} else {
		var elem []string
		if suffix != "" {
			elem = append(elem, "+", suffix)
		}
		for _, x := range b.Add {
			elem = append(elem, "+", x)
		}
		for _, x := range b.Skip {
			elem = append(elem, "-", x)
		}
		pattern = strings.Join(elem, "")
		if pattern == "" {
			pattern = "y"
		}
	}
	if b.Disable {
		pattern = "!" + pattern
	}
	pattern = visible + pattern

	// Construct substituted env and args.
	env := make([]string, len(b.Env))
	for i, x := range b.Env {
		k, v, _ := strings.Cut(x, "=")
		env[i] = k + "=" + replace(v, pattern, random)
	}
	args := make([]string, len(b.Args))
	for i, x := range b.Args {
		args[i] = replace(x, pattern, random)
	}

	// Construct and log command line. There is no newline in the log print.
	cmdText := strings.Join(append(append(env, b.Cmd), args...), " ")
	fmt.Fprintf(b.Stderr, "bisect: run: %s...", cmdText)

	// Run command with args and env.
	var out []byte
	var err error
	if b.TestRun != nil {
		out, err = b.TestRun(env, b.Cmd, args)
	} else {
		ctx := context.Background()
		if b.Timeout != 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, b.Timeout)
			defer cancel()
		}
		cmd := exec.CommandContext(ctx, b.Cmd, args...)
		cmd.Env = append(os.Environ(), env...)
		cmdInterrupt(cmd)
		out, err = cmd.CombinedOutput()
	}

	// Parse output to construct result.
	r := &Result{
		Suffix:  suffix,
		Success: err == nil,
		Cmd:     cmdText,
		Out:     string(out),
	}

	// Calculate bits, mask to identify suffix matches.
	var bits, mask uint64
	if suffix != "" && suffix != "y" && suffix != "n" && suffix != "v" {
		var err error
		bits, err = strconv.ParseUint(suffix, 2, 64)
		if err != nil {
			b.Fatalf("internal error: bad suffix")
		}
		mask = uint64(1<<len(suffix)) - 1
	}

	// Process output, collecting match reports for suffix.
	have := make(map[uint64]bool)
	all := r.Out
	for all != "" {
		var line string
		line, all, _ = strings.Cut(all, "\n")
		short, id, ok := bisect.CutMarker(line)
		if !ok || (id&mask) != bits {
			continue
		}

		if !have[id] {
			have[id] = true
			r.MatchIDs = append(r.MatchIDs, id)
		}
		r.MatchText = append(r.MatchText, short)
		r.MatchFull = append(r.MatchFull, line)
	}

	// Finish log print from above, describing the command's completion.
	if err == nil {
		fmt.Fprintf(b.Stderr, " ok (%d matches)\n", len(r.MatchIDs))
	} else {
		fmt.Fprintf(b.Stderr, " FAIL (%d matches)\n", len(r.MatchIDs))
	}

	if err != nil && len(r.MatchIDs) == 0 {
		b.Fatalf("target failed without printing any matches\n%s", r.Out)
	}

	// In verbose mode, print extra debugging: all the lines with match markers.
	if b.Verbose {
		b.Logf("matches:\n%s", strings.Join(r.MatchFull, "\n\t"))
	}

	return r
}

// replace returns x with literal text PATTERN and RANDOM replaced by pattern and random.
func replace(x, pattern, random string) string {
	x = strings.ReplaceAll(x, "PATTERN", pattern)
	x = strings.ReplaceAll(x, "RANDOM", random)
	return x
}

// commonSuffix returns the longest common binary suffix shared by all uint64s in list.
// If list is empty, commonSuffix returns an empty string.
func commonSuffix(list []uint64) string {
	if len(list) == 0 {
		return ""
	}
	b := list[0]
	n := 64
	for _, x := range list {
		for x&((1<<n)-1) != b {
			n--
			b &= (1 << n) - 1
		}
	}
	s := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		s[i] = '0' + byte(b&1)
		b >>= 1
	}
	return string(s[:])
}
