package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The docs have gone stale four times, and every time it was a number in prose
// that nothing checked: "world is the only account allowed negative", "the six
// checks" when there were seven, "six tests skip" when nine did, a route list
// carrying a line twice. Prose cannot be compiled, but a claim stated as a
// number can be pinned to the thing it counts, and that is all this file does.
//
// It deliberately does not check wording. A test that asserts a sentence is a
// test that fails every time a sentence improves, and it would be deleted
// within a month.

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	// this package is internal/api, so the repository root is two up
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// A route list that has drifted is worse than no route list: a reader trusts
// it and writes a client against a path that does not exist.
func TestTheDocumentedRoutesAreTheRoutesThatExist(t *testing.T) {
	want := map[string]bool{}
	for _, op := range specOperations(t) {
		want[op.method+" "+op.path] = true
	}
	if len(want) == 0 {
		t.Fatal("found no operations in the contract, so this test proves nothing")
	}

	// Only the route lists, not every mention of a path in prose: a sentence
	// naming an endpoint is not a claim to document all of them, and treating
	// it as one makes this test fail on ordinary writing.
	lists := map[string]func(string) string{
		// the fenced block under "### The api"
		"README.md": func(body string) string {
			return between(body, "### The api", "Outside the versioned API")
		},
		// the <td> cells of the route table
		"internal/api/demo.html": func(body string) string {
			return between(body, `<table class="surface">`, "</table>")
		},
	}
	route := regexp.MustCompile(`(GET|POST|PUT|PATCH|DELETE)(?:&nbsp;)?\s+(/v1/[^\s<]*)`)

	for doc, region := range lists {
		t.Run(doc, func(t *testing.T) {
			body := region(repoFile(t, doc))

			seen := map[string]int{}
			for _, m := range route.FindAllStringSubmatch(body, -1) {
				seen[m[1]+" "+m[2]]++
			}
			if len(seen) == 0 {
				t.Fatal("no routes found, so the extraction is broken rather than the doc being clean")
			}

			for op, n := range seen {
				if !want[op] {
					t.Errorf("documents %q, which the contract does not have", op)
				}
				if n > 1 {
					t.Errorf("lists %q %d times", op, n)
				}
			}
			for op := range want {
				if seen[op] == 0 {
					t.Errorf("does not document %q", op)
				}
			}
		})
	}
}

// between returns the text after the first start and before the next end, or
// the empty string, which the callers treat as a broken extraction rather than
// a clean document.
func between(body, start, end string) string {
	i := strings.Index(body, start)
	if i < 0 {
		return ""
	}
	rest := body[i+len(start):]
	if j := strings.Index(rest, end); j >= 0 {
		return rest[:j]
	}
	return rest
}

// "Eighteen routes" in prose, next to a list of eighteen routes, is the kind of
// pair that stays consistent right up until someone adds a nineteenth.
func TestTheRouteCountInProseMatchesTheContract(t *testing.T) {
	n := len(specOperations(t))

	words := map[int]string{
		15: "fifteen", 16: "sixteen", 17: "seventeen", 18: "eighteen",
		19: "nineteen", 20: "twenty", 21: "twenty-one", 22: "twenty-two",
	}
	word, ok := words[n]
	if !ok {
		t.Skipf("no spelling for %d routes; add one to this table", n)
	}

	page := strings.ToLower(repoFile(t, "internal/api/demo.html"))
	if !strings.Contains(page, word+" routes") {
		t.Errorf("the page does not say %q, but the contract has %d operations", word+" routes", n)
	}
}

// The checks are named in output an operator greps and in a schedule that
// alerts on them, so a name is an interface. This pins the list itself; the
// count in prose follows from it.
func TestTheDocumentedChecksAreTheChecksThatRun(t *testing.T) {
	// the six that always run, plus the three the composition root and the
	// stale window add. changing this list is a deliberate act, which is the
	// point of writing it out.
	want := []string{
		"conservation", "log", "projection", "effective_volumes",
		"balance_permissions", "closed_accounts",
		"stale_balances", "conversions", "reconciliation",
	}

	// the names as the code spells them, so a rename here fails loudly there
	source := repoFile(t, "storage/verify_runs.go") +
		repoFile(t, "cmd/giro/verify.go")
	for _, name := range want {
		if !strings.Contains(source, `"`+name+`"`) {
			t.Errorf("check %q is documented but no longer registered in the code", name)
		}
	}

	for _, doc := range []string{"README.md", "internal/api/demo.html", "deploy/README.md"} {
		body := repoFile(t, doc)
		for _, name := range want {
			if !strings.Contains(body, name) {
				t.Errorf("%s never mentions the %q check", doc, name)
			}
		}
	}
}

// Every migration is a file, and a doc that names a count is naming how many
// files there are.
func TestTheMigrationCountOnThePageIsReal(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations found, so this test proves nothing")
	}

	page := repoFile(t, "internal/api/demo.html")
	claim := regexp.MustCompile(`applied (\d+) migration`).FindStringSubmatch(page)
	if claim == nil {
		return // the page no longer shows the runner's output, which is fine
	}
	if claim[1] != strconv.Itoa(len(files)) {
		t.Errorf("the page shows %s migrations applied, but there are %d files",
			claim[1], len(files))
	}
}
