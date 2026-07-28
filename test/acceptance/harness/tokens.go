package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// tokensDir is computed from this source file's own location via
// runtime.Caller, not the test binary's working directory - go test sets
// CWD to whichever package is under test, which is often NOT this
// package, so a hardcoded "../../testdata/tokens" would resolve
// differently (and wrongly) depending on who calls Token().
var tokensDir = func() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "tokens")
}()

var personaFile = map[string]string{
	"support": "dana.jwt", // hostile on purpose: asserts tenant_id t_evilcorp
	"admin":   "root.jwt",
}

// tokenFor reads a real fixture token - raw unverified claims JSON, not a
// signed JWT (see identity package doc) - for one of the named personas.
func tokenFor(persona string) string {
	file, ok := personaFile[persona]
	if !ok {
		file = personaFile["support"]
	}
	data, err := os.ReadFile(filepath.Join(tokensDir, file))
	if err != nil {
		panic(fmt.Sprintf("harness: read token fixture %s: %v", file, err))
	}
	return strings.TrimSpace(string(data))
}
