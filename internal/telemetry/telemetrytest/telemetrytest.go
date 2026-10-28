// Package telemetrytest checks event payloads against docs/telemetry.md.
package telemetrytest

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// Doc is docs/telemetry.md from a package two levels below the module
// root, which is where every package recording telemetry sits.
const Doc = "../../docs/telemetry.md"

// Documented fails t unless the payload table's row for kind names each of
// record's top-level JSON keys, so the reference can't fall behind what
// the events carry.
func Documented(t *testing.T, kind string, record any) {
	t.Helper()
	data, err := os.ReadFile(Doc)
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "| `"+kind+"` |") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("%s has no payload row for %q", Doc, kind)
	}
	typ := reflect.TypeOf(record)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if !strings.Contains(row, "`"+name+"`") {
			t.Errorf("%s: the %s row doesn't name %q (%s.%s)", Doc, kind, name, typ.Name(), f.Name)
		}
	}
}
