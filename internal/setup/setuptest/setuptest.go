// Package setuptest holds templates for tests: "demo" is complete and small,
// "incomplete" names a rule file it does not have.
package setuptest

import "testing/fstest"

// Demo is a complete template "demo": one new namespace, one rule document,
// a two-item principle list. "incomplete" lacks its rule file.
var Demo = fstest.MapFS{
	"demo/template.json": {Data: []byte(`{
  "id": "demo", "name": "Demo", "description": "complete template for tests", "version": 1,
  "namespaces": [{"name": "demo-ns", "description": "namespace of the demo"}, {"name": "tracker", "description": "dòng ứng tuyển"}],
  "rules": [{"title": "demo-rules", "file": "demo-rules.md"}],
  "principles": {"title": "principle-list", "file": "principle-list.md"}
}`)},
	"demo/demo-rules.md":     {Data: []byte("# Demo rules\n\nA1. Không comment quá hai lần.\n")},
	"demo/principle-list.md": {Data: []byte("# Principle List\n\n1. Strong architecture absorbs weak execution.\n2. Honest gaps over false promises.\n")},

	// "incomplete" names a rule file that is not there.
	"incomplete/template.json": {Data: []byte(`{
  "id": "incomplete", "name": "Incomplete", "description": "names a missing file", "version": 1,
  "namespaces": [{"name": "never-added", "description": "added even though a file is missing"}],
  "rules": [{"title": "missing-rules", "file": "missing-rules.md"}]
}`)},
}
