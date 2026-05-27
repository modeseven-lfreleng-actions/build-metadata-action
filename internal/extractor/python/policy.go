// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Linux Foundation

package python

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lfreleng-actions/build-metadata-action/internal/pyversions"
)

// Policy controls how the Python extractor resolves the supported-Python
// set. It carries information ABOUT EOL versions (so the extractor can
// surface them via outputs) but never imposes a behaviour: the action
// always exits cleanly and downstream consumers decide what to do about
// EOL hits via the `python_eol_versions_present` /
// `python_eol_versions` outputs.
//
// The policy is package-scoped (see `activePolicy`) because the
// `extractor.Extractor` interface has a fixed `Extract(path string)`
// signature that we cannot widen. `cmd/build-metadata/main.go`
// configures the policy from action inputs before invoking Extract;
// tests set it directly for deterministic mocking of the live EOL API.
type Policy struct {
	Offline    bool
	Timeout    time.Duration
	MaxRetries int

	// SupportedSet is the canonical list of versions the matrix
	// generator is allowed to emit. Defaults to
	// `supportedPythonVersions` in offline mode, or to every cycle
	// from the live API at or above the baseline floor (EOL or not)
	// when online. EOL membership is tracked separately in
	// `EOLVersions` so the extractor can surface it without removing
	// anything. The baseline floor itself is `pyversions.Baseline()`;
	// see the constants in `internal/pyversions/eol.go`.
	SupportedSet []string

	// EOLVersions is the set of versions that the live API marked
	// end-of-life. Populated in online mode; empty otherwise.
	EOLVersions map[string]bool

	// LiveFallbackUsed records that the live API was consulted but
	// failed (or returned nothing usable), so the static set was used.
	LiveFallbackUsed bool
}

// defaultPolicy returns an offline policy with the static supported
// set. This is the policy the extractor uses when no caller has
// overridden it (e.g. `go test`).
func defaultPolicy() *Policy {
	return &Policy{
		Offline:      true,
		SupportedSet: append([]string(nil), supportedPythonVersions...),
		EOLVersions:  map[string]bool{},
	}
}

// activePolicy holds the policy the extractor will consult during
// matrix resolution. It is mutable so the CLI entry point (and tests)
// can swap in a configured policy before invoking Extract().
var activePolicy = defaultPolicy()

// SetActivePolicy replaces the active extractor policy. Passing nil
// resets to the offline default.
func SetActivePolicy(p *Policy) {
	if p == nil {
		activePolicy = defaultPolicy()
		return
	}
	if p.EOLVersions == nil {
		p.EOLVersions = map[string]bool{}
	}
	if len(p.SupportedSet) == 0 {
		p.SupportedSet = append([]string(nil), supportedPythonVersions...)
	}
	activePolicy = p
}

// ActivePolicy returns the current extractor policy (never nil).
func ActivePolicy() *Policy {
	if activePolicy == nil {
		activePolicy = defaultPolicy()
	}
	return activePolicy
}

// ResolvePolicy builds a Policy from CLI inputs, consulting the live
// EOL API when offline is false. Returns the resolved policy ready to
// be passed to SetActivePolicy.
func ResolvePolicy(offline bool, timeout time.Duration, maxRetries int) *Policy {
	p := &Policy{
		Offline:     offline,
		Timeout:     timeout,
		MaxRetries:  maxRetries,
		EOLVersions: map[string]bool{},
	}

	if offline {
		p.SupportedSet = append([]string(nil), supportedPythonVersions...)
		return p
	}

	client := pyversions.NewEOLClient(timeout, maxRetries)
	data, err := client.FetchEOLData()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"[WARNING] Failed to fetch live Python EOL data (%v); using static supported set\n", err)
		p.SupportedSet = append([]string(nil), supportedPythonVersions...)
		p.LiveFallbackUsed = true
		return p
	}

	// Walk the entire API response, recording every EOL cycle so the
	// matrix-emission step can surface the EOL membership via the
	// `python_eol_versions` / `python_eol_versions_present` outputs.
	for _, entry := range data {
		if eol, _ := client.IsVersionEOL(entry.Cycle, data); eol {
			p.EOLVersions[entry.Cycle] = true
		}
	}

	// Build the supported set from every cycle in the live response
	// that is between the baseline floor and the latest ceiling --
	// INCLUDING EOL cycles. The constraint solver therefore sees every
	// conceivable Python version in the supported range; EOL ones
	// still appear in the resolved matrix because reporting is the
	// extractor's only job here.
	//
	// Both bounds are sourced from `pyversions.Baseline()` and
	// `pyversions.Latest()` so a range bump (an EOL drop or a freshly
	// released minor) remains a one-line change in
	// `internal/pyversions/eol.go`. Without the upper bound the action
	// would automatically start emitting any newer minor (or a future
	// major like 4.0) as soon as endoflife.date listed it, which can
	// break downstream workflows whose runners/tooling do not yet
	// support that interpreter.
	floor := pyversions.Baseline()
	ceiling := pyversions.Latest()
	cycles := make([]string, 0, len(data))
	for _, entry := range data {
		cycle := entry.Cycle
		if compareVersionStrings(cycle, floor) < 0 {
			continue
		}
		if compareVersionStrings(cycle, ceiling) > 0 {
			continue
		}
		cycles = append(cycles, cycle)
	}
	if len(cycles) == 0 {
		fmt.Fprintf(os.Stderr,
			"[WARNING] Live EOL data yielded no Python versions in the %s..%s range; using static supported set\n",
			floor, ceiling)
		p.SupportedSet = append([]string(nil), supportedPythonVersions...)
		p.LiveFallbackUsed = true
		return p
	}
	// endoflife.date returns cycles in newest-first order. Downstream
	// consumers (in particular `resolveAndEmitMatrix`, which picks the
	// last element as `build_version`) assume ascending order; sort
	// here so the live and static paths produce equivalent matrices.
	sort.Slice(cycles, func(i, j int) bool {
		return compareVersionStrings(cycles[i], cycles[j]) < 0
	})
	p.SupportedSet = cycles
	return p
}

// detectEOLInMatrix returns the subset of `matrix` that the policy
// marks as end-of-life, in the matrix's original order. The matrix
// itself is never modified: build-metadata-action surfaces the EOL
// membership via dedicated outputs and leaves any policy decisions
// (warn, strip, fail) to downstream consumers such as
// python-build-action.
func detectEOLInMatrix(matrix []string, p *Policy) []string {
	if p == nil || len(p.EOLVersions) == 0 || len(matrix) == 0 {
		return nil
	}
	var hits []string
	for _, v := range matrix {
		if p.EOLVersions[v] {
			hits = append(hits, v)
		}
	}
	return hits
}

// debugf writes a debug log line to stderr only when INPUT_VERBOSE is
// set to "true" (matching the convention used by other extractors and
// documented by the action's `verbose` input). Without this gate, the
// extractor would spam logs even on quiet runs and bury more important
// `::warning::` / `::error::` lines.
func debugf(format string, args ...interface{}) {
	if os.Getenv("INPUT_VERBOSE") != "true" {
		return
	}
	fmt.Fprintf(os.Stderr, format, args...)
}

// writeOutOfRangeStepSummary appends a notice to the GitHub Actions
// step summary when the project's requires-python constraint matches
// no supported Python and the matrix had to widen as a fallback. The
// step summary is the user-visible surface; failures to write it are
// silently swallowed because the notice is purely advisory.
func writeOutOfRangeStepSummary(requiresPython string, matrix []string) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "### Python requires-python out of range\n\n")
	_, _ = fmt.Fprintf(f, "Project declared `requires-python = %q`, which matches no "+
		"Python version supported by this action. The build matrix was widened to "+
		"the supported set so the workflow can proceed:\n\n%s\n\n",
		requiresPython, strings.Join(matrix, ", "))
}

// compareVersionStrings is a local copy of the major.minor comparator;
// duplicated here so policy.go does not depend on the lower-cased
// compareVersions defined in python.go (and so this file can be lifted
// out into its own package later without churn).
func compareVersionStrings(v1, v2 string) int {
	var maj1, min1, maj2, min2 int
	_, _ = fmt.Sscanf(v1, "%d.%d", &maj1, &min1)
	_, _ = fmt.Sscanf(v2, "%d.%d", &maj2, &min2)
	switch {
	case maj1 != maj2:
		if maj1 < maj2 {
			return -1
		}
		return 1
	case min1 != min2:
		if min1 < min2 {
			return -1
		}
		return 1
	}
	return 0
}
