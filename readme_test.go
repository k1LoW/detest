package detest

import (
	"os"
	"strings"
	"testing"
)

// README's list of the functions detest runs is checked against knownFuncs,
// so that the list an agent reads before writing a simulation does not
// drift from what runs.
func TestREADMEListsKnownFuncs(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	const prefix = "- Functions: "
	var line string
	for l := range strings.SplitSeq(readme, "\n") {
		if strings.HasPrefix(l, prefix) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("README.md has no line starting with %q", prefix)
	}
	listed := map[string]bool{}
	for i, part := range strings.Split(line, "`") {
		if i%2 == 1 { // between backticks
			listed[part] = true
		}
	}
	for name := range knownFuncs {
		if name == "current_timestamp" || name == "current_date" {
			continue // the SQL value functions, which the README lists in capitals
		}
		if !listed[name] {
			t.Errorf("README's function list lacks %q", name)
		}
	}
	for name := range listed {
		if !knownFuncs[name] {
			t.Errorf("README lists %q, which detest does not run", name)
		}
	}
}
