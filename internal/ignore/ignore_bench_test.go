package ignore

// Benchmarks for the compiled matcher (perf plan unit E, E2: "Walker with
// rules" harness row). The previous matcher allocated on every call
// (strings.Split/Join, filepath.Base) and tested every rule in order; the
// walker spent ~60% of a 61k-file walk in it (TestPerfWalkerDecomp: 1.1 s
// with rules versus 0.4 s without).

import (
	"strings"
	"testing"
)

// benchGitignore is a typical 48-rule root .gitignore (the corpus one).
const benchGitignore = `# Binaries
*.exe
*.exe~
*.dll
*.so
*.dylib
*.test
*.out
go.work
go.work.sum
.env
.env.*
!.env.example
*.log
logs/
tmp/
coverage/
*.swp
*.swo
*~
.DS_Store
Thumbs.db
.idea/
.vscode/
*.iml
/bin/
/out/
/target/
__pycache__/
*.py[cod]
*.egg-info/
.pytest_cache/
.mypy_cache/
.tox/
.cache/
*.tmp
*.bak
*.orig
*.rej
/secrets.json
**/testdata/fixtures/*.golden
docs/_build/
site/
.terraform/
*.tfstate
*.tfstate.*
npm-debug.log*
yarn-error.log*
`

var benchPaths = []string{
	"internal/agent/llm_runtime_search.go",
	"go126/src/net/http/server.go",
	"x/tools/gopls/internal/golang/completion/completion.go",
	"README.md",
	"a/b/c/d/e/f/g.txt",
	"web/node_modules",
	"build.log",
	"pkg/testdata/fixtures/out.golden",
}

func BenchmarkMatch(b *testing.B) {
	m := Parse(strings.NewReader(benchGitignore))
	b.ReportAllocs()
	for b.Loop() {
		for _, p := range benchPaths {
			m.Match(p, false)
		}
	}
}

func BenchmarkMatchOracle(b *testing.B) {
	m := oracleParse(benchGitignore)
	b.ReportAllocs()
	for b.Loop() {
		for _, p := range benchPaths {
			m.match(p, false)
		}
	}
}
