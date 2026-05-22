// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 The Linux Foundation

package python

import (
	"os"
	"strings"
	"testing"

	"github.com/lfreleng-actions/build-metadata-action/internal/extractor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withPolicy installs the supplied policy as the active extractor
// policy for the duration of t and restores the previous one on
// cleanup. Tests use it to drive the EOL-behaviour branches
// deterministically without touching the live endoflife.date API.
func withPolicy(t *testing.T, p *Policy) {
	t.Helper()
	prev := ActivePolicy()
	SetActivePolicy(p)
	t.Cleanup(func() { SetActivePolicy(prev) })
}

// TestPolicy_WarnKeepsEolVersions confirms the default 'warn' behaviour
// keeps EOL versions in the constraint-derived matrix and records the
// fact that filtering did NOT trim anything (eol_filtered=false).
func TestPolicy_WarnKeepsEolVersions(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      false,
		Behaviour:    EOLBehaviourWarn,
		SupportedSet: []string{"3.9", "3.10", "3.11", "3.12"},
		EOLVersions:  map[string]bool{"3.9": true},
	})

	setupCfg := "[metadata]\n" +
		"name = warn-pkg\n" +
		"version = 1.0\n" +
		"python_requires = >=3.9\n"
	tmpDir := createTempProject(t, map[string]string{"setup.cfg": setupCfg})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	metadata, err := ex.Extract(tmpDir)
	require.NoError(t, err)

	matrix, _ := metadata.LanguageSpecific["version_matrix"].([]string)
	assert.Contains(t, matrix, "3.9",
		"warn behaviour must keep EOL versions in the matrix")
	assert.Equal(t, false, metadata.LanguageSpecific["eol_filtered"],
		"eol_filtered should be false when warn behaviour keeps the matrix intact")
}

// TestPolicy_StripRemovesEolVersions confirms 'strip' actually removes
// EOL versions and sets eol_filtered=true.
func TestPolicy_StripRemovesEolVersions(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      false,
		Behaviour:    EOLBehaviourStrip,
		SupportedSet: []string{"3.9", "3.10", "3.11", "3.12"},
		EOLVersions:  map[string]bool{"3.9": true},
	})

	setupCfg := "[metadata]\n" +
		"name = strip-pkg\n" +
		"version = 1.0\n" +
		"python_requires = >=3.9\n"
	tmpDir := createTempProject(t, map[string]string{"setup.cfg": setupCfg})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	metadata, err := ex.Extract(tmpDir)
	require.NoError(t, err)

	matrix, _ := metadata.LanguageSpecific["version_matrix"].([]string)
	assert.NotContains(t, matrix, "3.9",
		"strip behaviour must remove EOL versions from the matrix")
	assert.Equal(t, true, metadata.LanguageSpecific["eol_filtered"],
		"eol_filtered should be true when strip actually trimmed the matrix")
}

// TestPolicy_StripEmptiesMatrixFallsBackToEolFallback confirms that
// when EOL stripping leaves an empty matrix, the extractor falls back
// to the policy's non-EOL supported set AND overrides
// requires_python_source to 'eol-fallback' so downstream consumers can
// tell apart a clean constraint-match from a forced widening.
func TestPolicy_StripEmptiesMatrixFallsBackToEolFallback(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      false,
		Behaviour:    EOLBehaviourStrip,
		SupportedSet: []string{"3.9", "3.10"},
		EOLVersions:  map[string]bool{"3.9": true, "3.10": false},
	})

	// A constraint that resolves to {3.9} alone before EOL filtering.
	setupCfg := "[metadata]\n" +
		"name = eol-fallback-pkg\n" +
		"version = 1.0\n" +
		"python_requires = ==3.9\n"
	tmpDir := createTempProject(t, map[string]string{"setup.cfg": setupCfg})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	metadata, err := ex.Extract(tmpDir)
	require.NoError(t, err)

	source, _ := metadata.LanguageSpecific["requires_python_source"].(string)
	assert.Equal(t, "eol-fallback", source,
		"source should be 'eol-fallback' when strip empties the constraint-derived matrix")

	matrix, _ := metadata.LanguageSpecific["version_matrix"].([]string)
	assert.NotContains(t, matrix, "3.9")
	assert.Contains(t, matrix, "3.10")
}

// TestPolicy_FailReturnsErrorForEolVersionsInMatrix confirms that
// 'fail' behaviour produces an error from the extractor when any EOL
// version survives constraint resolution.
func TestPolicy_FailReturnsErrorForEolVersionsInMatrix(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      false,
		Behaviour:    EOLBehaviourFail,
		SupportedSet: []string{"3.9", "3.10", "3.11"},
		EOLVersions:  map[string]bool{"3.9": true},
	})

	setupCfg := "[metadata]\n" +
		"name = fail-pkg\n" +
		"version = 1.0\n" +
		"python_requires = >=3.9\n"
	tmpDir := createTempProject(t, map[string]string{"setup.cfg": setupCfg})
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	ex := NewExtractor()
	_, err := ex.Extract(tmpDir)
	require.Error(t, err,
		"fail behaviour must abort extraction when an EOL version remains in the matrix")
	assert.Contains(t, err.Error(), "end-of-life Python")
}

// TestPolicy_OfflineUsesStaticSupportedSet confirms that an offline
// policy skips the live API entirely and emits the static supported
// set as the matrix when no constraint is declared.
func TestPolicy_OfflineUsesStaticSupportedSet(t *testing.T) {
	withPolicy(t, &Policy{
		Offline:      true,
		Behaviour:    EOLBehaviourWarn,
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

// TestPolicy_StepSummaryWrittenForEolWarn confirms the writeEOLStepSummary
// helper appends to $GITHUB_STEP_SUMMARY when the file is configured.
// This is the user-visible surface for EOL warnings on the GitHub job page.
func TestPolicy_StepSummaryWrittenForEolWarn(t *testing.T) {
	summary, err := os.CreateTemp(t.TempDir(), "step-summary-*.md")
	require.NoError(t, err)
	require.NoError(t, summary.Close())

	t.Setenv("GITHUB_STEP_SUMMARY", summary.Name())

	writeEOLStepSummary([]string{"3.9", "2.7"}, "warned")

	body, err := os.ReadFile(summary.Name())
	require.NoError(t, err)
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "Python EOL versions warned",
		"step summary must include the action header")
	assert.Contains(t, bodyStr, "3.9",
		"step summary must list each EOL version")
	assert.Contains(t, bodyStr, "2.7")
}

// TestApplyEOLPolicy_NoOpsWhenNoEolVersionsConfigured confirms the
// short-circuit when the policy carries no EOL version map (typical
// offline + warn case).
func TestApplyEOLPolicy_NoOpsWhenNoEolVersionsConfigured(t *testing.T) {
	matrix := []string{"3.10", "3.11"}
	out, filtered, err := applyEOLPolicy(matrix, &Policy{Behaviour: EOLBehaviourStrip})
	require.NoError(t, err)
	assert.False(t, filtered)
	assert.Equal(t, matrix, out)
}

// TestApplyEOLPolicy_StripPreservesOrder confirms that strip behaviour
// keeps the surviving versions in their original order so the matrix
// reproducibility downstream consumers rely on is not violated.
func TestApplyEOLPolicy_StripPreservesOrder(t *testing.T) {
	policy := &Policy{
		Behaviour:   EOLBehaviourStrip,
		EOLVersions: map[string]bool{"3.10": true, "3.12": true},
	}
	out, filtered, err := applyEOLPolicy(
		[]string{"3.10", "3.11", "3.12", "3.13"}, policy)
	require.NoError(t, err)
	assert.True(t, filtered)
	assert.Equal(t, []string{"3.11", "3.13"}, out,
		"strip must preserve the original order of the surviving versions")
}

// TestApplyEOLPolicy_FailListsAllEolHitsInError ensures the error
// message returned for 'fail' behaviour names every EOL version so the
// user can fix their constraint without rerunning under 'warn' first.
func TestApplyEOLPolicy_FailListsAllEolHitsInError(t *testing.T) {
	policy := &Policy{
		Behaviour:   EOLBehaviourFail,
		EOLVersions: map[string]bool{"3.9": true, "3.10": true},
	}
	_, _, err := applyEOLPolicy([]string{"3.9", "3.10", "3.11"}, policy)
	require.Error(t, err)
	msg := err.Error()
	assert.True(t, strings.Contains(msg, "3.9") && strings.Contains(msg, "3.10"),
		"fail-mode error must enumerate every EOL hit: got %q", msg)
}

// TestResolvePolicy_NormalisesUnknownBehaviour confirms the policy
// builder treats an unknown behaviour string as 'warn' rather than
// silently dropping it.
func TestResolvePolicy_NormalisesUnknownBehaviour(t *testing.T) {
	// Offline=true so we don't reach the live API.
	p := ResolvePolicy(true, "rubbish", 0, 0)
	require.NotNil(t, p)
	assert.Equal(t, EOLBehaviourWarn, p.Behaviour)
	assert.True(t, p.Offline)
}

// TestResolvePolicy_OfflineNeverHitsTheNetwork is a structural check:
// when offline=true the resolved policy carries the static supported
// set verbatim and no EOL versions (network was never consulted).
func TestResolvePolicy_OfflineNeverHitsTheNetwork(t *testing.T) {
	p := ResolvePolicy(true, EOLBehaviourStrip, 0, 0)
	require.NotNil(t, p)
	assert.True(t, p.Offline)
	assert.Equal(t, EOLBehaviourStrip, p.Behaviour)
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
	assert.Equal(t, EOLBehaviourWarn, p.Behaviour)
	assert.Equal(t, append([]string(nil), supportedPythonVersions...), p.SupportedSet)

	// nonEOLSet on a clean policy returns the same set.
	assert.Equal(t, p.SupportedSet, p.nonEOLSet())
}

// TestPolicyEnsuresActiveIsNeverNil is the contract test for
// ActivePolicy(); package consumers rely on it never returning nil.
func TestPolicyEnsuresActiveIsNeverNil(t *testing.T) {
	withPolicy(t, nil) // SetActivePolicy(nil) installs the default
	assert.NotNil(t, ActivePolicy())
}

// Silence an unused-package warning in case the extractor stops using
// extractor.ProjectMetadata directly in some refactor.
var _ = extractor.ProjectMetadata{}
