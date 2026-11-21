package jplcheck_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/MITRAKER/realtime-chat/internal/jplcheck"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), jplcheck.Analyzer, "a")
}
