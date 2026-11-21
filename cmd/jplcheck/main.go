// Command jplcheck checks NASA/JPL "Power of Ten" rules 1 and 2 that no
// standard Go linter covers. Usage: go run ./cmd/jplcheck ./...
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/MITRAKER/realtime-chat/internal/jplcheck"
)

func main() { singlechecker.Main(jplcheck.Analyzer) }
