// Package tf wraps the terraform CLI.
package tf

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/askrike/desprawl/internal/logs"
	"github.com/askrike/desprawl/internal/prof"
	"github.com/askrike/desprawl/internal/progress"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Runner struct {
	Dir string
	Env map[string]string // extra environment, e.g. cloud credentials profile
	Log logs.Logger
	// CLIConfig is the TF_CLI_CONFIG_FILE of every command but init (see
	// DevOverrides); init installs providers with the user's own config.
	CLIConfig string
	// ProviderCache, if set, is where init keeps providers, their lock-file
	// checksums and schemas across workspaces (see cache.go).
	ProviderCache string
	// Progress, if set, counts the resources each plan has read.
	Progress *progress.Reporter
}

// In returns a runner for another directory with the same environment.
func (r Runner) In(dir string) Runner {
	r.Dir = dir
	return r
}

// span profiles one terraform process; dir is the last element of the
// runner's directory (in discover, the type being listed).
func (r Runner) span(command string) func(...string) {
	return prof.Span("terraform", "command", command, "dir", filepath.Base(r.Dir))
}

func (r Runner) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "terraform", append([]string{"-chdir=" + r.Dir}, args...)...)
	c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	for k, v := range r.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	// After Env: CLIConfig includes the user's config, wherever it is.
	if r.CLIConfig != "" && args[0] != "init" {
		c.Env = append(c.Env, "TF_CLI_CONFIG_FILE="+r.CLIConfig)
	}
	return c
}

func (r Runner) Init(ctx context.Context) error {
	c := r.cmd(ctx, "init", "-no-color", "-input=false")
	if r.ProviderCache != "" {
		c.Env = append(c.Env, r.pluginCacheEnv()...)
	}
	end := r.span("init")
	out, err := c.CombinedOutput()
	end(prof.Err(err)...)
	if err != nil {
		return fmt.Errorf("terraform init: %w\n%s", err, out)
	}
	if r.ProviderCache != "" {
		r.keepLockHashes()
	}
	return nil
}

type Pos struct {
	Line int `json:"line"`
}

type Range struct {
	Filename string `json:"filename"`
	Start    Pos    `json:"start"`
	End      Pos    `json:"end"`
}

type Diagnostic struct {
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail"`
	Address  string `json:"address"`
	Range    *Range `json:"range"`
}

func (d Diagnostic) String() string {
	var b strings.Builder
	b.WriteString(d.Summary)
	if d.Address != "" {
		b.WriteString(" [" + d.Address + "]")
	}
	if d.Range != nil {
		fmt.Fprintf(&b, " (%s:%d)", d.Range.Filename, d.Range.Start.Line)
	}
	if d.Detail != "" {
		b.WriteString(": " + strings.ReplaceAll(d.Detail, "\n", " "))
	}
	return b.String()
}

// stream runs terraform and calls onLine for each line of its stdout.
// Stdout goes through a writer rather than StdoutPipe so that Wait owns the
// OS pipe: provider plugins can outlive an interrupted terraform and hold
// the pipe open, and Wait closes it after WaitDelay. On cancellation
// terraform is interrupted, not killed, so it shuts its plugins down.
func (r Runner) stream(ctx context.Context, args []string, onLine func([]byte)) (stderr string, err error) {
	c := r.cmd(ctx, args...)
	c.Cancel = func() error { return c.Process.Signal(os.Interrupt) }
	c.WaitDelay = 15 * time.Second
	var errBuf bytes.Buffer
	c.Stderr = &errBuf
	pr, pw := io.Pipe()
	c.Stdout = pw
	end := r.span(args[0])
	if err := c.Start(); err != nil {
		end(prof.Err(err)...)
		return "", err
	}
	defer func() { end(prof.Err(err)...) }()
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 256<<20)
		for sc.Scan() {
			onLine(sc.Bytes())
		}
		io.Copy(io.Discard, pr) // keep draining if a line was too long
	}()
	err = c.Wait()
	pw.Close()
	<-scanned
	if ctx.Err() != nil {
		return errBuf.String(), ctx.Err()
	}
	return errBuf.String(), err
}

// planTimeout bounds a single plan; a plan that runs longer is stuck on
// something (credentials, throttling) rather than working.
const planTimeout = 30 * time.Minute

// planParallelism is how many resources a plan reads at once. Reads are
// mostly waiting on cloud APIs, so terraform's default of 10 leaves plans
// of a whole account idle; providers retry throttled calls.
const planParallelism = 50

// Plan runs `terraform plan -json` and returns its error diagnostics.
// A non-nil error means terraform could not be run at all; a failed plan
// is reported through the diagnostics.
func (r Runner) Plan(ctx context.Context, extra ...string) ([]Diagnostic, error) {
	diags, _, err := r.PlanIDs(ctx, extra...)
	return diags, err
}

// PlanIDs is Plan that also returns the ID of each resource the plan read
// (imported or refreshed), by address, as terraform reports it. Reads
// happen before generated config is validated, so a plan that fails on
// config still reports them.
func (r Runner) PlanIDs(ctx context.Context, extra ...string) ([]Diagnostic, map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	var diags []Diagnostic
	ids := map[string]string{}
	r.Progress.Reset()
	args := []string{"plan", "-json", "-input=false", "-lock=false", fmt.Sprintf("-parallelism=%d", planParallelism)}
	stderr, err := r.stream(ctx, append(args, extra...), func(line []byte) {
		var msg struct {
			Type       string      `json:"type"`
			Message    string      `json:"@message"`
			Diagnostic *Diagnostic `json:"diagnostic"`
			Hook       struct {
				Resource struct {
					Addr string `json:"addr"`
				} `json:"resource"`
				IDKey   string `json:"id_key"`
				IDValue string `json:"id_value"`
			} `json:"hook"`
		}
		if json.Unmarshal(line, &msg) != nil {
			return
		}
		switch {
		case msg.Type == "diagnostic" && msg.Diagnostic != nil && msg.Diagnostic.Severity == "error":
			diags = append(diags, *msg.Diagnostic)
		case msg.Type == "refresh_complete":
			r.Progress.Add(1) // one resource read from the cloud, the slow part
			if h := msg.Hook; h.IDKey == "id" && h.IDValue != "" && h.Resource.Addr != "" {
				ids[h.Resource.Addr] = h.IDValue
			}
		}
	})
	if ctx.Err() != nil {
		return nil, nil, fmt.Errorf("terraform plan: %w", ctx.Err())
	}
	if err != nil && len(diags) == 0 {
		return nil, nil, fmt.Errorf("terraform plan: %w\n%s", err, stderr)
	}
	return diags, ids, nil
}

// ShowJSON returns `terraform show -json <planfile>`.
func (r Runner) ShowJSON(ctx context.Context, planFile string) ([]byte, error) {
	c := r.cmd(ctx, "show", "-json", planFile)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	end := r.span("show")
	out, err := c.Output()
	end(prof.Err(err)...)
	if err != nil {
		return nil, fmt.Errorf("terraform show: %w\n%s", err, stderr.String())
	}
	return out, nil
}

// PlanOutputs plans the configuration in the runner's directory and
// returns its output values; config there should only read (data sources
// and outputs). The plan file is deleted.
func (r Runner) PlanOutputs(ctx context.Context) (map[string]json.RawMessage, []Diagnostic, error) {
	const planFile = "outputs.tfplan"
	defer os.Remove(r.Dir + "/" + planFile)
	diags, err := r.Plan(ctx, "-out="+planFile)
	if err != nil || len(diags) > 0 {
		return nil, diags, err
	}
	b, err := r.ShowJSON(ctx, planFile)
	if err != nil {
		return nil, nil, err
	}
	var plan struct {
		PlannedValues struct {
			Outputs map[string]struct {
				Value json.RawMessage `json:"value"`
			} `json:"outputs"`
		} `json:"planned_values"`
	}
	if err := json.Unmarshal(b, &plan); err != nil {
		return nil, nil, err
	}
	out := map[string]json.RawMessage{}
	for k, o := range plan.PlannedValues.Outputs {
		out[k] = o.Value
	}
	return out, nil, nil
}
