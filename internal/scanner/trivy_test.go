package scanner

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTrivySummary(t *testing.T) {
	raw := []byte(`{
		"Results": [
			{"Vulnerabilities": [
				{"Severity": "CRITICAL"},
				{"Severity": "HIGH"},
				{"Severity": "high"},
				{"Severity": "MEDIUM"},
				{"Severity": "LOW"},
				{"Severity": "WEIRD"}
			]},
			{"Vulnerabilities": [
				{"Severity": "CRITICAL"}
			]}
		]
	}`)

	s, err := parseTrivySummary(raw)
	require.NoError(t, err)
	require.Equal(t, 2, s.Critical)
	require.Equal(t, 2, s.High)
	require.Equal(t, 1, s.Medium)
	require.Equal(t, 1, s.Low)
	require.Equal(t, 1, s.Unknown)
}

func TestParseTrivySummary_NoResults(t *testing.T) {
	s, err := parseTrivySummary([]byte(`{"Results": []}`))
	require.NoError(t, err)
	require.Equal(t, Summary{}, s)
}

func TestParseTrivySummary_Invalid(t *testing.T) {
	_, err := parseTrivySummary([]byte(`not json`))
	require.Error(t, err)
}
