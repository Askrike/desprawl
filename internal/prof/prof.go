// Package prof records where desprawl's wall time goes. When the
// environment variable DESPRAWL_PROFILE names a file, every span (a command,
// a terraform or claude process, one type's listing, a fix round) is
// appended to it as one JSON line:
//
//	{"name":"terraform","start":1730000000.12,"seconds":3.4,"pid":123,"attrs":{"args":"plan"}}
//
// Spans may overlap (types list in parallel); several processes may append
// to the same file. Unset, a span costs a nil check.
package prof

import (
	"encoding/json"
	"os"
	"slices"
	"sync"
	"time"
)

// Env names the file spans are appended to.
const Env = "DESPRAWL_PROFILE"

var (
	once sync.Once
	mu   sync.Mutex
	out  *os.File
)

func file() *os.File {
	once.Do(func() {
		if path := os.Getenv(Env); path != "" {
			out, _ = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		}
	})
	return out
}

// Span starts a span; call the returned function when it ends. Attributes
// are key, value pairs; more can be passed at the end (results, errors).
//
//	end := prof.Span("list", "type", typ)
//	...
//	end("found", strconv.Itoa(n))
func Span(name string, kv ...string) func(more ...string) {
	if file() == nil {
		return func(...string) {}
	}
	start := time.Now()
	return func(more ...string) {
		attrs := map[string]string{}
		all := slices.Concat(kv, more)
		for i := 0; i+1 < len(all); i += 2 {
			attrs[all[i]] = all[i+1]
		}
		line, _ := json.Marshal(struct {
			Name    string            `json:"name"`
			Start   float64           `json:"start"`
			Seconds float64           `json:"seconds"`
			PID     int               `json:"pid"`
			Attrs   map[string]string `json:"attrs,omitempty"`
		}{name, float64(start.UnixMicro()) / 1e6, time.Since(start).Seconds(), os.Getpid(), attrs})
		mu.Lock()
		defer mu.Unlock()
		out.Write(append(line, '\n')) // one write per line, so lines from several processes don't interleave
	}
}

// Err is the attributes for an error, or none.
func Err(err error) []string {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return []string{"error", msg}
}
