package tka

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// ownerStore is the owner's own store as it stood on 14/10/2026: its 42 active
// instructions (testdata/owner-instructions.json, read from the store) with
// the D-116 classification applied the way `tk instructions tag
// testdata/instruction-tags.json` applies it — default lines keep the default list's
// tags, the lines the owner added take D-116's.
func ownerStore(t *testing.T) []Instruction {
	t.Helper()
	var list []Instruction
	raw, err := os.ReadFile("testdata/owner-instructions.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	var tags []InstructionTagInput
	raw, err = os.ReadFile("testdata/instruction-tags.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &tags); err != nil {
		t.Fatal(err)
	}
	byID := map[string]InstructionTags{}
	for _, tg := range tags {
		if err := validTags(tg.InstructionTags); err != nil {
			t.Fatalf("%s: %v", tg.ID, err)
		}
		tg.Source = "tagged"
		byID[tg.ID] = tg.InstructionTags
	}
	untagged := []string{}
	for i := range list {
		if tg, ok := byID[list[i].ID]; ok {
			list[i].Tags = tg
		} else if tg, ok := defaultTagsFor(list[i]); ok {
			list[i].Tags = tg
		} else {
			untagged = append(untagged, list[i].Label)
		}
	}
	if len(list) != 42 || len(untagged) != 0 {
		t.Fatalf("every one of the 42 lines carries tags: %d lines, untagged %v", len(list), untagged)
	}
	return list
}

func labels(lines []HelpLine) []string {
	out := []string{}
	for _, l := range lines {
		out = append(out, l.Label)
	}
	return out
}

// The owner's acceptance (13/10/2026): help("draft linkedin message") returns
// directives 14, 16, 17, 19, 26 in full and nothing unrelated — and 28, added
// on 14/10 for drafts that run long; help("record a decision") returns 1, 2,
// 4, 5, 20, 24.
func TestHelp_OwnerAcceptance(t *testing.T) {
	list := ownerStore(t)
	for _, c := range []struct {
		situation string
		want      []string
	}{
		{"draft linkedin message", []string{"14", "16", "17", "19", "26", "28"}},
		{"record a decision", []string{"1", "2", "4", "5", "20", "24"}},
		{"close session", []string{"10", "11"}},
		{"analyse strategy", []string{"9", "22", "23", "25"}},
		{"Kết thúc phiên làm việc", []string{"10", "11"}},
		{"soạn tin nhắn linkedin", []string{"14", "16", "17", "19", "26", "28"}},
		{"record the status of work already done", []string{"1", "2", "3", "4", "5", "20", "24"}},
		{"record a screenshot with no link as evidence", []string{"12", "20"}},
		{"comment on a draft standard in the consultation form", []string{"14", "15", "26", "28"}},
		{"the person is tired of correcting you, recording costs too much", []string{"27", "29"}},
	} {
		got := Help(list, c.situation)
		if !slices.Equal(labels(got.Directives), c.want) || len(got.Definitions) != 0 {
			t.Errorf("help(%q):\n got %v + definitions %v (read as %v)\nwant %v", c.situation, labels(got.Directives), labels(got.Definitions), got.ReadAs, c.want)
		}
	}

	// In full, with the incident the line cites.
	got := Help(list, "draft linkedin message")
	for _, l := range got.Directives {
		var in Instruction
		for _, x := range list {
			if x.ID == l.ID {
				in = x
			}
		}
		if l.Text != in.Text {
			t.Fatalf("%s comes back in full", l.Label)
		}
	}
	if d := got.Directives[4]; d.Label != "26" || !strings.HasPrefix(d.Incident, "13/10/2026: four drafts") || !slices.Equal(d.Cites, []string{"D-114"}) {
		t.Fatalf("26 cites its incident: %q %v", d.Incident, d.Cites)
	}

	rec := Help(list, "record a decision")
	if d := rec.Directives[4]; d.Label != "20" || !strings.HasPrefix(d.Incident, "On 13/10/2026 a session wrote eight claims") {
		t.Fatalf("20 gives its incident in running text: %q", d.Incident)
	}

	// A term: its definition, nothing else.
	if got := Help(list, "what is a T"); len(got.Directives) != 0 || !slices.Equal(labels(got.Definitions), []string{"def:truth [T]"}) || got.ReadAs["unread"] != nil {
		t.Fatalf("what is a T: %v %v", labels(got.Directives), labels(got.Definitions))
	}
	if got := Help(list, "what is an unbound document"); !slices.Equal(labels(got.Definitions), []string{"def:document/bound-vs-unbound"}) {
		t.Fatalf("unbound: %v", labels(got.Definitions))
	}
	// Nothing to read: nothing returned, and the way to ask.
	if got := Help(list, "weather in Dubai"); got.Count != 0 || got.Next == "" {
		t.Fatalf("unrelated: %+v", got)
	}
}

// The default set every new store gets carries tags of its own, so help works
// in a store nobody has tagged; directive 13 (the owner's 27) answers on-cost.
func TestHelp_DefaultStore(t *testing.T) {
	list := startingLines(catalog{})
	for i := range list {
		tg, ok := defaultTagsFor(list[i])
		if !ok {
			t.Fatalf("%s has no default tags", list[i].Label)
		}
		if err := validTags(tg); err != nil {
			t.Fatalf("%s: %v", list[i].Label, err)
		}
		list[i].Tags = tg
	}
	if got := Help(list, "recording costs the person more than it keeps"); !slices.Equal(labels(got.Directives), []string{"13"}) {
		t.Fatalf("on-cost: %v (%v)", labels(got.Directives), got.ReadAs)
	}
	if got := Help(list, "record a decision"); !slices.Equal(labels(got.Directives), []string{"1", "2", "4", "5"}) {
		t.Fatalf("record a decision in a new store: %v", labels(got.Directives))
	}
	// A line changed by its store does not borrow the default's tags.
	changed := list[2]
	changed.Text = "Something else."
	if _, ok := defaultTagsFor(changed); ok {
		t.Fatal("a default label with other text keeps no default tags")
	}
}

func TestValidTags(t *testing.T) {
	for _, bad := range []InstructionTags{
		{When: []string{"someday"}},
		{What: []string{"person"}},
		{Where: []string{"linkedin:post"}},
		{When: []string{"before-write:Work Status"}},
	} {
		if err := validTags(bad); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
	if err := validTags(InstructionTags{Who: []string{"agent"}, When: []string{"before-write:work-status"}, Where: []string{"web-form"}}); err != nil {
		t.Fatal(err)
	}
}
