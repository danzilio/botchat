// Package speccheck enforces the link between SPEC.md and the test suite:
// every requirement must be covered by at least one test (via a
// "// spec: ID" comment in a Go test or an e2e/*.spec.js browser test), and
// tests must not cite requirements that don't exist. See CONTRIBUTING.md.
package speccheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	// A requirement: "- **MSG-4** ...". Manual ones contain "*(manual)*".
	reqRE = regexp.MustCompile(`(?m)^\s*-\s+\*\*([A-Z]+-\d+)\*\*(.*)$`)
	// A test reference: "// spec: MSG-4" (several IDs may be comma-separated).
	refRE = regexp.MustCompile(`//\s*spec:\s*([A-Z]+-\d+(?:\s*,\s*[A-Z]+-\d+)*)`)
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

type requirement struct {
	id     string
	manual bool
}

func requirements(t *testing.T, root string) []requirement {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "SPEC.md"))
	if err != nil {
		t.Fatal(err)
	}
	var out []requirement
	seen := map[string]bool{}
	for _, m := range reqRE.FindAllStringSubmatch(string(b), -1) {
		id := m[1]
		if seen[id] {
			t.Errorf("SPEC.md defines %s more than once", id)
		}
		seen[id] = true
		out = append(out, requirement{id: id, manual: strings.Contains(m[2], "*(manual)*")})
	}
	if len(out) == 0 {
		t.Fatal("no requirements found in SPEC.md")
	}
	return out
}

func references(t *testing.T, root string) map[string][]string {
	t.Helper()
	refs := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "dist" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		isTest := strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".spec.js")
		if d.IsDir() || !isTest || strings.Contains(path, "speccheck") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range refRE.FindAllStringSubmatch(string(b), -1) {
			for _, id := range strings.Split(m[1], ",") {
				id = strings.TrimSpace(id)
				refs[id] = append(refs[id], rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestEveryRequirementIsTested(t *testing.T) {
	root := repoRoot(t)
	refs := references(t, root)
	for _, r := range requirements(t, root) {
		if r.manual {
			continue
		}
		if len(refs[r.id]) == 0 {
			t.Errorf("%s has no test: add a test with a `// spec: %s` comment", r.id, r.id)
		}
	}
}

func TestNoUnknownReferences(t *testing.T) {
	root := repoRoot(t)
	var ids []string
	for _, r := range requirements(t, root) {
		ids = append(ids, r.id)
	}
	for id, files := range references(t, root) {
		if !slices.Contains(ids, id) {
			t.Errorf("%v reference %s, which is not in SPEC.md", files, id)
		}
	}
}
