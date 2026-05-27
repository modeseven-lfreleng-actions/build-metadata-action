// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Linux Foundation

package python

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withPolicy installs the supplied policy as the active extractor
// policy for the duration of t and restores the previous one on
// cleanup. Tests use it to drive the EOL-detection paths
// deterministically without touching the live endoflife.date API.
func withPolicy(t *testing.T, p *Policy) {
	t.Helper()
	prev := ActivePolicy()
	SetActivePolicy(p)
	t.Cleanup(func() { SetActivePolicy(prev) })
}

// TestPolicy_EolVersionsSurfacedInOutput confirms that when the resolved
// matrix contains versions the policy marks as EOL, the extractor:
//
//   - leaves the matrix UNCHANGED (no stripping, no fallback);
//   - emits `eol_versions` as a space-separated list of the hits;
//   - emits `eol_versions_present` = true.
//
// The action takes no opinionated action (warn/strip/fail removed);
// downstream consumers such as python-build-action read these outputs
// and decide what to do.
func TestPolicy_EolVersionsSurfacedInOutput(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      false,
		SupportedSet: []string{"3.9", "3.10", "3.11", "3.12"},
		EOLVersions:  map[string]bool{"3.9": true},
	})

	setupCfg := "[metadata]\n" +
		"name = eol-detect-pkg\n" +
		"version = 1.0\n" +
		"python_requires = >=3.9\n"
	tmpDir := createTempProject(t, map[string]string{"setup.cfg": setupCfg})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	metadata, err := ex.Extract(tmpDir)
	require.NoError(t, err)

	matrix, _ := metadata.LanguageSpecific["version_matrix"].([]string)
	assert.Contains(t, matrix, "3.9",
		"matrix must retain EOL versions; downstream consumers decide what to do")

	assert.Equal(t, "3.9", metadata.LanguageSpecific["eol_versions"],
		"eol_versions must list the EOL hits as a space-separated string")
	assert.Equal(t, true, metadata.LanguageSpecific["eol_versions_present"],
		"eol_versions_present must be true when any EOL version is in the matrix")
}

// TestPolicy_NoEolHitsEmitsCleanOutputs confirms that when the matrix
// contains no EOL versions, the EOL outputs are emitted but empty/false
// (rather than absent) so downstream `if:` predicates have a stable
// value to read.
func TestPolicy_NoEolHitsEmitsCleanOutputs(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      true,
		SupportedSet: []string{"3.10", "3.11", "3.12", "3.13", "3.14"},
		EOLVersions:  map[string]bool{},
	})

	setupCfg := "[metadata]\n" +
		"name = clean-pkg\n" +
		"version = 1.0\n" +
		"python_requires = >=3.10\n"
	tmpDir := createTempProject(t, map[string]string{"setup.cfg": setupCfg})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	metadata, err := ex.Extract(tmpDir)
	require.NoError(t, err)

	assert.Equal(t, "", metadata.LanguageSpecific["eol_versions"],
		"eol_versions must be the empty string when no EOL versions are in the matrix")
	assert.Equal(t, false, metadata.LanguageSpecific["eol_versions_present"],
		"eol_versions_present must be false when no EOL versions are in the matrix")
}

// TestPolicy_MultipleEolHitsAreSpaceSeparated confirms the format of
// the `eol_versions` output when more than one EOL version is in the
// matrix. The shell action's convention was a space-separated list and
// downstream consumers expect that exact shape.
func TestPolicy_MultipleEolHitsAreSpaceSeparated(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      false,
		SupportedSet: []string{"3.8", "3.9", "3.10", "3.11"},
		EOLVersions:  map[string]bool{"3.8": true, "3.9": true},
	})

	setupCfg := "[metadata]\n" +
		"name = multi-eol\n" +
		"version = 1.0\n" +
		"python_requires = >=3.8\n"
	tmpDir := createTempProject(t, map[string]string{"setup.cfg": setupCfg})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	metadata, err := ex.Extract(tmpDir)
	require.NoError(t, err)

	assert.Equal(t, "3.8 3.9", metadata.LanguageSpecific["eol_versions"],
		"multiple EOL hits must be joined by a single space, in matrix order")
}

// TestPolicy_OfflineUsesStaticSupportedSet confirms that an offline
// policy skips the live API entirely and emits the static supported
// set as the matrix when no constraint is declared.
func TestPolicy_OfflineUsesStaticSupportedSet(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      true,
		SupportedSet: []string{"3.10", "3.11", "3.12", "3.13", "3.14"},
		EOLVersions:  map[string]bool{},
	})

	setupCfg := "[metadata]\n" +
		"name = offline-pkg\n" +
		"version = 1.0\n"
	tmpDir := createTempProject(t, map[string]string{"setup.cfg": setupCfg})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	metadata, err := ex.Extract(tmpDir)
	require.NoError(t, err)

	matrix, _ := metadata.LanguageSpecific["version_matrix"].([]string)
	assert.Equal(t, []string{"3.10", "3.11", "3.12", "3.13", "3.14"}, matrix)
	source, _ := metadata.LanguageSpecific["requires_python_source"].(string)
	assert.Equal(t, "static-fallback", source,
		"a project with no python_requires must report 'static-fallback', not the live source")
}

// TestDetectEOLInMatrix_PreservesMatrixOrder confirms that the helper
// returns hits in the matrix's original order so the resulting
// `eol_versions` output is stable across runs.
func TestDetectEOLInMatrix_PreservesMatrixOrder(t *testing.T) {
	policy := &Policy{
		EOLVersions: map[string]bool{"3.9": true, "3.7": true},
	}
	out := detectEOLInMatrix(
		[]string{"3.7", "3.8", "3.9", "3.10"}, policy)
	assert.Equal(t, []string{"3.7", "3.9"}, out,
		"detectEOLInMatrix must preserve the matrix order when reporting hits")
}

// TestDetectEOLInMatrix_NoOpsWhenNoEolVersionsConfigured confirms the
// short-circuit when the policy carries no EOL version map (typical
// offline case).
func TestDetectEOLInMatrix_NoOpsWhenNoEolVersionsConfigured(t *testing.T) {
	out := detectEOLInMatrix(
		[]string{"3.10", "3.11"}, &Policy{})
	assert.Nil(t, out)
}

// TestResolvePolicy_OfflineNeverHitsTheNetwork is a structural check:
// when offline=true the resolved policy carries the static supported
// set verbatim and no EOL versions (network was never consulted).
func TestResolvePolicy_OfflineNeverHitsTheNetwork(t *testing.T) {
	p := ResolvePolicy(true, 0, 0)
	require.NotNil(t, p)
	assert.True(t, p.Offline)
	assert.Empty(t, p.EOLVersions,
		"offline policy must not carry any EOL version data")
	assert.Equal(t, append([]string(nil), supportedPythonVersions...), p.SupportedSet)
}

// TestDefaultPolicyMatchesStaticSet covers the no-input baseline used
// by every test that does not call withPolicy.
func TestDefaultPolicyMatchesStaticSet(t *testing.T) {
	p := defaultPolicy()
	require.NotNil(t, p)
	assert.True(t, p.Offline)
	assert.Equal(t, append([]string(nil), supportedPythonVersions...), p.SupportedSet)
}

// TestPolicy_ClassifierMatrixEmitsEOLOutputs covers the bug where the
// setup.cfg/setup.py classifier-derived matrix branches previously
// did NOT emit `eol_versions` / `eol_versions_present`, leaving the
// outputs absent for classifier-only projects. The action now emits
// the keys at every matrix-emission site.
func TestPolicy_ClassifierMatrixEmitsEOLOutputs(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      false,
		SupportedSet: []string{"3.10", "3.11", "3.12", "3.13", "3.14"},
		// In a real online run the live API would also list, say, 3.10 as
		// EOL only after October 2026; for the test we just pretend.
		EOLVersions: map[string]bool{"3.10": true},
	})

	setupCfg := "[metadata]\n" +
		"name = classifier-eol\n" +
		"version = 1.0\n" +
		"classifiers =\n" +
		"    Programming Language :: Python :: 3.10\n" +
		"    Programming Language :: Python :: 3.11\n" +
		"    Programming Language :: Python :: 3.12\n"
	tmpDir := createTempProject(t, map[string]string{"setup.cfg": setupCfg})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	metadata, err := ex.Extract(tmpDir)
	require.NoError(t, err)

	source, _ := metadata.LanguageSpecific["requires_python_source"].(string)
	assert.Equal(t, "classifiers", source,
		"setup.cfg classifier-derived path must record its source")

	// The matrix-emission helper centralised in emitEOLOutputs() must
	// fire here even though the classifier path bypasses
	// resolveAndEmitMatrix.
	assert.Equal(t, "3.10", metadata.LanguageSpecific["eol_versions"],
		"classifier-derived matrices must surface EOL versions for downstream consumers")
	assert.Equal(t, true, metadata.LanguageSpecific["eol_versions_present"])
}

// TestPolicy_FallbackMatrixEmitsEOLOutputs covers the same gap on the
// static-fallback path. Because the fallback set is intentionally
// filtered to exclude EOL cycles, the outputs should be empty/false --
// but they must still be PRESENT so downstream `if:` predicates have a
// stable value to read.
// TestPolicy_OutOfRangeSetsRequiresPythonFallback covers the gap where
// the out-of-range / parse-error paths in resolveAndEmitMatrix widen
// the matrix to the supported set (a fallback from the consumer's
// perspective) but did not set `requires_python_fallback=true`. The
// boolean output must now fire for any *-fallback source so consumers
// can rely on a single signal regardless of which fallback path was
// taken.
func TestPolicy_OutOfRangeSetsRequiresPythonFallback(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      true,
		SupportedSet: []string{"3.10", "3.11", "3.12", "3.13", "3.14"},
		EOLVersions:  map[string]bool{},
	})

	pyproject := "[project]\n" +
		"name = \"out-of-range-fb\"\n" +
		"version = \"1.0\"\n" +
		"requires-python = \">=4.0\"\n"
	tmpDir := createTempProject(t, map[string]string{"pyproject.toml": pyproject})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	metadata, err := ex.Extract(tmpDir)
	require.NoError(t, err)

	assert.Equal(t, "out-of-range-fallback",
		metadata.LanguageSpecific["requires_python_source"])
	assert.Equal(t, true, metadata.LanguageSpecific["requires_python_fallback"],
		"requires_python_fallback must be true for any -fallback source path, "+
			"not only the static-fallback path")
}

func TestPolicy_FallbackMatrixEmitsEOLOutputs(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      true,
		SupportedSet: []string{"3.10", "3.11", "3.12", "3.13", "3.14"},
		EOLVersions:  map[string]bool{},
	})

	// A bare-minimum setup.cfg with no version signal whatsoever; the
	// extractor will fall back to the policy supported set.
	setupCfg := "[metadata]\nname = fallback-pkg\n"
	tmpDir := createTempProject(t, map[string]string{"setup.cfg": setupCfg})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	metadata, err := ex.Extract(tmpDir)
	require.NoError(t, err)

	assert.Equal(t, "static-fallback",
		metadata.LanguageSpecific["requires_python_source"],
		"the no-signal fallback path must record the static-fallback source")
	assert.Equal(t, "", metadata.LanguageSpecific["eol_versions"],
		"static-fallback path emits empty eol_versions for stable downstream reads")
	assert.Equal(t, false, metadata.LanguageSpecific["eol_versions_present"])
}

// TestPolicyEnsuresActiveIsNeverNil is the contract test for
// ActivePolicy(); package consumers rely on it never returning nil.
func TestPolicyEnsuresActiveIsNeverNil(t *testing.T) {
	withPolicy(t, nil) // SetActivePolicy(nil) installs the default
	assert.NotNil(t, ActivePolicy())
}
