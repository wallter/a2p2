package localapi

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The chaos checker (scripts/chaos/chaos.py) compares what each member
// holds by the entry fields that are the same on every machine
// (ENTRY_SHARED) and leaves out those that describe the reading machine
// (ENTRY_LOCAL). A field added to Entry and placed in neither list would
// stop a chaos run, which is not part of make check; this test says so
// first. A field in both, or a list naming a field Entry no longer has,
// fails too.
func TestChaosCheckerKnowsEveryEntryField(t *testing.T) {
	src, err := os.ReadFile("../../scripts/chaos/chaos.py")
	if err != nil {
		t.Fatal(err)
	}
	list := func(name string) map[string]bool {
		m := regexp.MustCompile(`(?s)\n` + name + ` = \((.*?)\n\)`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("chaos.py has no %s = ( ... ) tuple", name)
		}
		out := map[string]bool{}
		for _, line := range strings.Split(string(m[1]), "\n") {
			line, _, _ = strings.Cut(line, "#")
			for _, q := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(line, -1) {
				out[q[1]] = true
			}
		}
		return out
	}
	shared, local := list("ENTRY_SHARED"), list("ENTRY_LOCAL")
	fields := map[string]bool{}
	ty := reflect.TypeOf(Entry{})
	for i := 0; i < ty.NumField(); i++ {
		name, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			fields[name] = true
		}
	}
	var problems []string
	for f := range fields {
		switch {
		case shared[f] && local[f]:
			problems = append(problems, f+" is in both ENTRY_SHARED and ENTRY_LOCAL")
		case !shared[f] && !local[f]:
			problems = append(problems, f+" is in neither ENTRY_SHARED nor ENTRY_LOCAL: decide whether every member holds it alike")
		}
	}
	for _, l := range []map[string]bool{shared, local} {
		for f := range l {
			if !fields[f] {
				problems = append(problems, f+" is listed in chaos.py but is not a field of localapi.Entry")
			}
		}
	}
	// The checker compares files without each file's state here.
	if _, ok := reflect.TypeOf(EntryFile{}).FieldByName("State"); !ok {
		problems = append(problems, "EntryFile has no State, which chaos.py's record_key leaves out")
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}
