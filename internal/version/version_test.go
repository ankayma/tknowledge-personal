package version

import "testing"

func TestFormat(t *testing.T) {
	for _, c := range []struct {
		rev, at string
		dirty   bool
		want    string
	}{
		{"5ee8d5f0123456789", "2026-10-01T11:20:31Z", false, "5ee8d5f · 2026-10-01 11:20 UTC"},
		{"5ee8d5f0123456789", "2026-10-01T18:20:31+07:00", true, "5ee8d5f · 2026-10-01 11:20 UTC · dirty"},
		{"abc", "", false, "abc"},
		{"", "2026-10-01T11:20:31Z", true, "dev"},
	} {
		if got := format(c.rev, c.at, c.dirty); got != c.want {
			t.Fatalf("format(%q, %q, %v) = %q, want %q", c.rev, c.at, c.dirty, got, c.want)
		}
	}
	if String() == "" {
		t.Fatal("String must never be empty")
	}
}
