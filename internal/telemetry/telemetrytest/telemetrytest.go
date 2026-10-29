// Package telemetrytest checks event payloads against docs/telemetry.md.
package telemetrytest

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// Doc is docs/telemetry.md, relative to a test's package directory. It
// works for any package exactly two levels below the module root
// (internal/x, cmd/x), since tests run in their package's directory.
const Doc = "../../docs/telemetry.md"

// module is buggrit's module path. Its types encode as the doc names them;
// maestro-llms' encode with their Go field names, as the doc says.
const module = "github.com/SnapdragonPartners/buggrit/"

// shapes heads the doc's section on the nested shapes several payloads
// share, such as the diff and a finding.
const shapes = "### Shared shapes"

// Documented fails t unless docs/telemetry.md names every JSON key record
// carries: its top-level keys in the payload table's row for kind, and the
// keys of every buggrit struct nested in it, at any depth, in that row or
// in the shared shapes section. Each of those structs must give every
// field a JSON key (see Tagged). So a key can't be added or renamed
// without the reference following.
func Documented(t testing.TB, kind string, record any) {
	t.Helper()
	data, err := os.ReadFile(Doc)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	var row string
	for line := range strings.SplitSeq(doc, "\n") {
		if strings.HasPrefix(line, "| `"+kind+"` |") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("%s has no payload row for %q", Doc, kind)
	}
	_, shared, ok := strings.Cut(doc, shapes+"\n")
	if !ok {
		t.Fatalf("%s has no %q section", Doc, shapes)
	}
	if end := strings.Index(shared, "\n#"); end >= 0 {
		shared = shared[:end]
	}
	walk(t, reflect.TypeOf(record), func(typ reflect.Type, f *reflect.StructField, name string, top bool) {
		where := row
		if !top {
			where += shared
		}
		if !strings.Contains(where, "`"+name+"`") {
			t.Errorf("%s doesn't name %q (%s.%s) in the %s row%s", Doc, name, typ, f.Name, kind, map[bool]string{false: " or " + shapes}[top])
		}
	})
}

// Tagged fails t if a buggrit struct reachable from v, through fields,
// pointers, slices and maps, has an exported field without a JSON key: it
// would encode under its Go name, which the doc reserves for maestro-llms
// types.
func Tagged(t testing.TB, v any) {
	t.Helper()
	walk(t, reflect.TypeOf(v), func(reflect.Type, *reflect.StructField, string, bool) {})
}

// walk calls key for each exported field's JSON key in every buggrit
// struct reachable from root; top is set for root's own fields. A field
// without a key fails t.
func walk(t testing.TB, root reflect.Type, key func(typ reflect.Type, f *reflect.StructField, name string, top bool)) {
	t.Helper()
	seen := map[reflect.Type]bool{}
	var visit func(typ reflect.Type, top bool)
	visit = func(typ reflect.Type, top bool) {
		for {
			switch typ.Kind() {
			case reflect.Pointer, reflect.Slice, reflect.Array:
				typ = typ.Elem()
				continue
			case reflect.Map:
				visit(typ.Key(), false)
				typ = typ.Elem()
				continue
			}
			break
		}
		// An inline struct has no package, and is the record's own.
		pkg := typ.PkgPath()
		if typ.Kind() != reflect.Struct || seen[typ] || pkg != "" && !strings.HasPrefix(pkg+"/", module) {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			switch name {
			case "-":
				continue
			case "":
				t.Errorf("%s.%s has no JSON key, so it encodes as %q", typ, f.Name, f.Name)
			default:
				key(typ, &f, name, top)
			}
			visit(f.Type, false)
		}
	}
	visit(root, true)
}
