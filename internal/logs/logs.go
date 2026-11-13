// Package logs is what desprawl's stages say while they run, besides
// their results: at the default level what a command did, with -verbose
// what it's doing and why it takes time, with -debug the details of each
// resource and retry.
package logs

import "fmt"

// Level says who a line is for.
type Level int

const (
	// Info is what a command did, and warnings its problems don't list.
	Info Level = iota
	// Verbose is what it's doing now and why it takes time.
	Verbose
	// Debug is each resource's and retry's details, and lines the
	// problems repeat.
	Debug
)

// Logger prints a line at a level; a nil Logger prints nothing.
type Logger func(Level, string)

func (l Logger) Info(format string, a ...any)    { l.at(Info, format, a) }
func (l Logger) Verbose(format string, a ...any) { l.at(Verbose, format, a) }
func (l Logger) Debug(format string, a ...any)   { l.at(Debug, format, a) }

func (l Logger) at(level Level, format string, a []any) {
	if l != nil {
		l(level, fmt.Sprintf(format, a...))
	}
}
