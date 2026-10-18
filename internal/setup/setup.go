// Package setup is the template registry behind setup.templates / setup.apply.
// A template is a directory with
// template.json plus the markdown files it names. A template need not be
// complete: a file that is absent or empty is listed in Missing, apply writes
// the rest, and the user supplies the gap during setup (store kind=rule, or
// setup.principles.set). A store built by hand exports back into a template
// (Files) for other users.
package setup

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed templates
var embedded embed.FS

// Embedded is the templates shipped in the binary (internal/setup/templates).
var Embedded fs.FS = func() fs.FS {
	sub, err := fs.Sub(embedded, "templates")
	if err != nil {
		panic(err)
	}
	return sub
}()

type Namespace struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type Doc struct {
	Title string `json:"title"`
	File  string `json:"file"`
	Body  string `json:"-"`
}

type Template struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Version     int         `json:"version"`
	Namespaces  []Namespace `json:"namespaces"`
	Rules       []Doc       `json:"rules"`
	Principles  *Doc        `json:"principles,omitempty"`
	// Missing lists template files that are absent or empty.
	Missing []string `json:"-"`
}

// Basis is what everything a template writes rests on: template:<id>@<version>.
func (t Template) Basis() string { return "template:" + t.ID + "@" + strconv.Itoa(t.Version) }

func (t Template) all() []Doc {
	docs := append([]Doc(nil), t.Rules...)
	if t.Principles != nil {
		docs = append(docs, *t.Principles)
	}
	return docs
}

// Docs are the rule documents then the principle list that have a body, in
// apply order.
func (t Template) Docs() []Doc {
	docs := []Doc{}
	for _, d := range t.all() {
		if d.Body != "" {
			docs = append(docs, d)
		}
	}
	return docs
}

// MissingTitles are the titles of the documents whose file is absent or
// empty — what the user still has to write.
func (t Template) MissingTitles() []string {
	titles := []string{}
	for _, d := range t.all() {
		if d.Body == "" {
			titles = append(titles, d.Title)
		}
	}
	return titles
}

// Files lays the template out as a directory: template.json and one markdown
// file per document (named from its title when File is empty). Load reads it
// back to the same template.
func (t Template) Files() (map[string]string, error) {
	files := map[string]string{}
	name := func(d *Doc) {
		if d.File == "" {
			base := docFileKey(d.Title)
			d.File = base + ".md"
			for i := 2; files[d.File] != ""; i++ {
				d.File = base + "-" + strconv.Itoa(i) + ".md"
			}
		}
		files[d.File] = d.Body
	}
	t.Rules = append([]Doc(nil), t.Rules...)
	for i := range t.Rules {
		name(&t.Rules[i])
	}
	if t.Principles != nil {
		p := *t.Principles
		name(&p)
		t.Principles = &p
	}
	if t.Namespaces == nil {
		t.Namespaces = []Namespace{}
	}
	if t.Rules == nil {
		t.Rules = []Doc{}
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, err
	}
	files["template.json"] = string(raw) + "\n"
	return files, nil
}

var nonKey = regexp.MustCompile(`[^a-z0-9]+`)

func docFileKey(title string) string {
	k := strings.Trim(nonKey.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if k == "" {
		k = "doc"
	}
	return k
}

// IDPattern is what a template id (and its directory name) may be.
var IDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

var principleLine = regexp.MustCompile(`(?m)^(\| \*\*P\.\d+\*\*|\d+\. )`)

// PrinciplesCount counts table rows "| **P.n**" or numbered lines "n. ".
func (t Template) PrinciplesCount() int {
	if t.Principles == nil {
		return 0
	}
	return len(principleLine.FindAllString(t.Principles.Body, -1))
}

type Registry struct {
	byID map[string]Template
}

// Load reads every <dir>/template.json in each file system; a later file
// system overrides an earlier one with the same id.
func Load(fsyss ...fs.FS) (*Registry, error) {
	r := &Registry{byID: map[string]Template{}}
	for _, fsys := range fsyss {
		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			raw, err := fs.ReadFile(fsys, path.Join(e.Name(), "template.json"))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			var t Template
			if err := json.Unmarshal(raw, &t); err != nil {
				return nil, fmt.Errorf("template %s: %w", e.Name(), err)
			}
			if t.ID != e.Name() || t.Version < 1 {
				return nil, fmt.Errorf("template %s: id must equal its directory and version be ≥ 1", e.Name())
			}
			load := func(d *Doc) {
				b, err := fs.ReadFile(fsys, path.Join(e.Name(), d.File))
				if err != nil || strings.TrimSpace(string(b)) == "" {
					t.Missing = append(t.Missing, path.Join(e.Name(), d.File))
					return
				}
				d.Body = string(b)
			}
			for i := range t.Rules {
				load(&t.Rules[i])
			}
			if t.Principles != nil {
				load(t.Principles)
			}
			r.byID[t.ID] = t
		}
	}
	return r, nil
}

func MustLoad(fsyss ...fs.FS) *Registry {
	r, err := Load(fsyss...)
	if err != nil {
		panic(err)
	}
	return r
}

// All templates, by id.
func (r *Registry) All() []Template {
	out := make([]Template, 0, len(r.byID))
	for _, t := range r.byID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// NextVersion is 1 for a new id, else the loaded version plus one — so a
// re-exported template's basis tells the two apart.
func (r *Registry) NextVersion(id string) int {
	return r.byID[id].Version + 1
}

func (r *Registry) Get(id string) (Template, bool) {
	t, ok := r.byID[strings.TrimSpace(id)]
	return t, ok
}

// ParseBasis reads template:<id>@<version>.
func ParseBasis(ref string) (string, int, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(ref), "template:")
	if !ok {
		return "", 0, false
	}
	id, v, ok := strings.Cut(rest, "@")
	version, err := strconv.Atoi(v)
	return id, version, ok && id != "" && err == nil && version >= 1
}
