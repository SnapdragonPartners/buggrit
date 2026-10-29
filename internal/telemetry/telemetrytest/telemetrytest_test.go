package telemetrytest

import (
	"fmt"
	"strings"
	"testing"
)

// failures records what a check reports instead of failing the test.
type failures struct {
	testing.TB
	errs []string
}

func (f *failures) Errorf(format string, args ...any) {
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
}
func (f *failures) Helper() {}

type named struct {
	Path string `json:"path"`
}

// Tagged reaches fields through named and inline structs, in slices and
// maps, and reports each one without a JSON key.
func TestTaggedWalksEveryStruct(t *testing.T) {
	var rec struct {
		Named  []named                          `json:"named"`
		Inline map[string]struct{ Path string } `json:"inline"`
		Deep   *struct {
			Items []struct{ ID string } `json:"items"`
		} `json:"deep"`
		Skip string `json:"-"`
	}
	f := &failures{TB: t}
	Tagged(f, rec)
	got := strings.Join(f.errs, "\n")
	if len(f.errs) != 2 || !strings.Contains(got, `encodes as "Path"`) || !strings.Contains(got, `encodes as "ID"`) {
		t.Errorf("reported:\n%s", got)
	}
}
