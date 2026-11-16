package main

import (
	"strings"
	"testing"
)

func TestNotesBetween(t *testing.T) {
	rs := []release{{Tag: "v0.1.12"}, {Tag: "v0.1.10"}, {Tag: "v0.1.9"}, {Tag: "v0.2.0", Draft: true}, {Tag: "v0.1.11"}}
	var got []string
	for _, r := range notesBetween(rs, "v0.1.9", "v0.1.11") {
		got = append(got, r.Tag)
	}
	if strings.Join(got, " ") != "v0.1.10 v0.1.11" {
		t.Errorf("got %v", got)
	}
	if n := notesBetween(rs, "v0.1.12", "v0.1.10"); len(n) != 0 {
		t.Errorf("going back: %v", n)
	}
	if s := notesText("- Faster discover\r\n\r\nCo-authored-by: A <a@x>\r\n"); s != "- Faster discover" {
		t.Errorf("notesText: %q", s)
	}
}

func TestYes(t *testing.T) {
	for in, want := range map[string]bool{"\n": true, "y\n": true, "Y\n": true, "n\n": false, " No \n": false, "": false, "y": false} {
		if got := yes(strings.NewReader(in)); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
}
