package prof

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSpans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.jsonl")
	t.Setenv(Env, path)
	once, out = sync.Once{}, nil
	t.Cleanup(func() { // the file stays open for the process; Windows can't remove it open
		if out != nil {
			out.Close()
		}
		once, out = sync.Once{}, nil
	})

	end := Span("list", "type", "a_thing")
	time.Sleep(10 * time.Millisecond)
	end("found", "3")
	Span("terraform", "args", "plan")(Err(errors.New("boom"))...)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var spans []struct {
		Name    string
		Seconds float64
		Attrs   map[string]string
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s struct {
			Name    string
			Seconds float64
			Attrs   map[string]string
		}
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		spans = append(spans, s)
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	if s := spans[0]; s.Name != "list" || s.Attrs["type"] != "a_thing" || s.Attrs["found"] != "3" || s.Seconds < 0.01 {
		t.Errorf("first span = %+v", s)
	}
	if s := spans[1]; s.Attrs["error"] != "boom" {
		t.Errorf("second span = %+v", s)
	}
}

func TestOff(t *testing.T) {
	t.Setenv(Env, "")
	once, out = sync.Once{}, nil
	Span("x")() // must not panic or write anywhere
}
