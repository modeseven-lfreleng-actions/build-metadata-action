// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Linux Foundation

package python

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lfreleng-actions/build-metadata-action/internal/pyversions"
)

// EOL behaviour names. The Python extractor consults the active policy
// to decide what to do with Python versions that the live endoflife.date
// API marks as end-of-life.
const (
	// EOLBehaviourWarn keeps EOL versions in the resolved matrix and
	// emits a ::warning:: line for each so consumers can surface the
	// risk without changing build topology.
	EOLBehaviourWarn = "warn"
	// EOLBehaviourStrip removes EOL versions from the resolved matrix.
	// When this empties the matrix the extractor falls back to the live
	// supported set and stamps the source as "eol-fallback".
	EOLBehaviourStrip = "strip"
	// EOLBehaviourFail aborts the extraction when any EOL version
	// remains in the resolved matrix.
	EOLBehaviourFail = "fail"
)

// Policy controls how the Python extractor resolves the supported-Python
// set and what it does with EOL versions that otherwise pass through the
// constraint solver.
//
// The policy is package-scoped (see `activePolicy`) because the
// `extractor.Extractor` interface has a fixed `Extract(path string)`
// signature that we cannot widen. `cmd/build-metadata/main.go` configures
// the policy from action inputs before invoking Extract; tests set it
// directly for deterministic mocking of the live EOL API.
type Policy struct {
	Offline    bool
	Behaviour  string
	Timeout    time.Duration
	MaxRetries int

	// SupportedSet is the canonical list of versions the matrix
	// generator is allowed to emit. Defaults to `supportedPythonVersions`
	// in offline mode, or to the live API intersection with the 3.10
	// floor when online.
	SupportedSet []string

	// EOLVersions is the set of versions that the live API marked
	// end-of-life. Populated in online mode; empty otherwise.
	EOLVersions map[string]bool

	// LiveFallbackUsed records that the live API was consulted but
	// failed (or returned nothing usable), so the static set was used.
	LiveFallbackUsed bool
}

// defaultPolicy returns an offline policy with warn behaviour and the
// static supported set. This is the policy the extractor uses when no
// caller has overridden it (e.g. `go test`).
func defaultPolicy() *Policy {
	return &Policy{
		Offline:      true,
		Behaviour:    EOLBehaviourWarn,
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
	if p.Behaviour == "" {
		p.Behaviour = EOLBehaviourWarn
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
func ResolvePolicy(offline bool, behaviour string, timeout time.Duration, maxRetries int) *Policy {
	switch behaviour {
	case EOLBehaviourStrip, EOLBehaviourFail, EOLBehaviourWarn:
		// supported value, keep as is
	case "":
		behaviour = EOLBehaviourWarn
	default:
		fmt.Fprintf(os.Stderr,
			"[WARNING] Unknown python_eol_behaviour %q; falling back to %q\n",
			behaviour, EOLBehaviourWarn)
		behaviour = EOLBehaviourWarn
	}

	p := &Policy{
		Offline:     offline,
		Behaviour:   behaviour,
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
	// matrix-emission step can apply the configured behaviour.
	for _, entry := range data {
		if eol, _ := client.IsVersionEOL(entry.Cycle, data); eol {
			p.EOLVersions[entry.Cycle] = true
		}
	}

	// Derive the supported set from non-EOL cycles, intersected with
	// the 3.10 floor. We never widen below 3.10 even if the live API
	// reports something older as still supported.
	live, _ := client.GetSupportedVersions()
	intersected := make([]string, 0, len(live))
	for _, v := range live {
		if compareVersionStrings(v, "3.10") >= 0 {
			intersected = append(intersected, v)
		}
	}
	if len(intersected) == 0 {
		fmt.Fprintf(os.Stderr,
			"[WARNING] Live EOL data yielded no supported Python versions; using static supported set\n")
		p.SupportedSet = append([]string(nil), supportedPythonVersions...)
		p.LiveFallbackUsed = true
		return p
	}
	p.SupportedSet = intersected
	return p
}

// nonEOLSet returns the policy's SupportedSet with any EOL versions
// filtered out. Used by the eol-fallback path when EOL filtering empties
// the constraint-derived matrix.
func (p *Policy) nonEOLSet() []string {
	out := make([]string, 0, len(p.SupportedSet))
	for _, v := range p.SupportedSet {
		if !p.EOLVersions[v] {
			out = append(out, v)
		}
	}
	return out
}

// applyEOLPolicy applies the configured EOL behaviour to a matrix.
// Returns the (possibly filtered) matrix, a boolean indicating whether
// EOL filtering actually changed the input, and an error when the
// behaviour is "fail" and EOL versions are present.
func applyEOLPolicy(matrix []string, p *Policy) ([]string, bool, error) {
	if p == nil || len(p.EOLVersions) == 0 || len(matrix) == 0 {
		return matrix, false, nil
	}
	eolHits := []string{}
	keep := []string{}
	for _, v := range matrix {
		if p.EOLVersions[v] {
			eolHits = append(eolHits, v)
		} else {
			keep = append(keep, v)
		}
	}
	if len(eolHits) == 0 {
		return matrix, false, nil
	}
	switch p.Behaviour {
	case EOLBehaviourStrip:
		for _, v := range eolHits {
			fmt.Fprintf(os.Stderr,
				"::warning::Python %s is end-of-life; stripped from matrix\n", v)
		}
		writeEOLStepSummary(eolHits, "stripped")
		return keep, true, nil
	case EOLBehaviourFail:
		return nil, false, fmt.Errorf(
			"matrix contains end-of-life Python versions %v (python_eol_behaviour=fail)",
			eolHits)
	case EOLBehaviourWarn:
		fallthrough
	default:
		for _, v := range eolHits {
			fmt.Fprintf(os.Stderr,
				"::warning::Python %s is end-of-life but remains in the build matrix\n", v)
		}
		writeEOLStepSummary(eolHits, "warned")
		return matrix, false, nil
	}
}

// writeEOLStepSummary appends an EOL notice to the GitHub Actions step
// summary file when one is configured. Best-effort: failures are
// silently swallowed because the step summary is purely advisory.
func writeEOLStepSummary(eolHits []string, action string) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "### Python EOL versions %s\n\n", action)
	_, _ = fmt.Fprintf(f, "%s\n\n", strings.Join(eolHits, ", "))
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
