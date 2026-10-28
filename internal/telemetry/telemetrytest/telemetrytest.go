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

// module is buggrit's module path. Its types encode as the doc names them;
// maestro-llms' encode with their Go field names, as the doc says.
const module = "github.com/SnapdragonPartners/buggrit/"

// Documented fails t unless the payload table's row for kind names each of
// record's top-level JSON keys, and every buggrit struct record carries
// gives each field a JSON key (see Tagged), so the reference can't fall
// behind what the events carry.
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
	Tagged(t, record)
}

// Tagged fails t if a buggrit struct reachable from v, through fields,
// pointers, slices and maps, has an exported field without a JSON key: it
// would encode under its Go name, which the doc reserves for maestro-llms
// types.
func Tagged(t *testing.T, v any) {
	t.Helper()
	seen := map[reflect.Type]bool{}
	var walk func(typ reflect.Type)
	walk = func(typ reflect.Type) {
		for {
			switch typ.Kind() {
			case reflect.Pointer, reflect.Slice, reflect.Array:
				typ = typ.Elem()
				continue
			case reflect.Map:
				walk(typ.Key())
				typ = typ.Elem()
				continue
			}
			break
		}
		if typ.Kind() != reflect.Struct || seen[typ] || !strings.HasPrefix(typ.PkgPath()+"/", module) {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name == "" {
				t.Errorf("%s.%s has no JSON key, so it encodes as %q", typ, f.Name, f.Name)
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeOf(v))
}
