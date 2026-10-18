package setup

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ankayma/tknowledge-personal/internal/setup/setuptest"
)

func TestEmbedded_BlankAndJobSearchComplete(t *testing.T) {
	r, err := Load(Embedded)
	if err != nil {
		t.Fatal(err)
	}
	blank, ok := r.Get("blank")
	if !ok || len(blank.Missing) != 0 || len(blank.Docs()) != 0 || blank.Basis() != "template:blank@1" {
		t.Fatalf("blank: %+v", blank)
	}
	js, ok := r.Get("job-search")
	if !ok || len(js.Namespaces) != 7 || js.PrinciplesCount() != 7 || len(js.Missing) != 0 || len(js.Docs()) != 3 {
		t.Fatalf("job-search: %+v (principles %d)", js, js.PrinciplesCount())
	}
	titles := []string{}
	for _, d := range js.Docs() {
		titles = append(titles, d.Title)
	}
	if !slices.Equal(titles, []string{"rules-comment-connect-filter", "instructions-job-search", "principle-list"}) ||
		!strings.Contains(js.Docs()[0].Body, "# B. Publishing on LinkedIn") || !strings.Contains(js.Docs()[1].Body, "Do not export delta files.") {
		t.Fatalf("job-search documents: %v", titles)
	}
}

func TestLoad_OverrideAndValidation(t *testing.T) {
	r := MustLoad(Embedded, setuptest.Demo)
	if inc, _ := r.Get("incomplete"); !slices.Equal(inc.Missing, []string{"incomplete/missing-rules.md"}) {
		t.Fatalf("incomplete: %v", inc.Missing)
	}
	demo, ok := r.Get("demo")
	if !ok || len(demo.Missing) != 0 || len(demo.Docs()) != 2 || demo.PrinciplesCount() != 2 || demo.Docs()[0].Body == "" {
		t.Fatalf("demo: %+v", demo)
	}
	ids := []string{}
	for _, tpl := range r.All() {
		ids = append(ids, tpl.ID)
	}
	if !slices.Equal(ids, []string{"blank", "demo", "incomplete", "job-search"}) {
		t.Fatalf("ids: %v", ids)
	}
	if _, err := Load(fstest.MapFS{"x/template.json": {Data: []byte(`{"id": "y", "version": 1}`)}}); err == nil {
		t.Fatal("id must match directory")
	}
	for ref, want := range map[string]bool{"template:demo@1": true, "template:demo@0": false, "template:@1": false, "template:demo": false, "doc_1": false} {
		if _, _, ok := ParseBasis(ref); ok != want {
			t.Errorf("ParseBasis(%q) = %v", ref, ok)
		}
	}
}
