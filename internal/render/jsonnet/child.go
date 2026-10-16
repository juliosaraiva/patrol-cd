package jsonnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// EvalArg is the os.Args[1] value that switches a binary into evaluator mode
// (ADR-0007 `patrolcd jsonnet-eval`).
const EvalArg = "jsonnet-eval"

// MaxRequestBytes bounds the request the child reads from stdin.
const MaxRequestBytes = 16 << 20

// Exit codes of the evaluator child.
const (
	exitOK       = 0
	exitProtocol = 2
)

// Default child limits applied by Runner when the corresponding field is zero.
const (
	DefaultTimeout                  = 90 * time.Second
	DefaultCPUSeconds        uint64 = 60
	DefaultAddressSpaceBytes uint64 = 4 << 30
)

var evalMainCalled atomic.Bool

// MaybeJsonnetMain must be called at the start of main of every binary used as the
// evaluator child (cmd/patrolcd, and TestMain of test binaries that use Runner),
// right after sandbox.MaybeHelperMain. When os.Args[1] is EvalArg it reads a Request
// from stdin, evaluates it, writes the Response to stdout and exits; otherwise it
// records that the hook is installed and returns.
func MaybeJsonnetMain() {
	if len(os.Args) < 2 || os.Args[1] != EvalArg {
		evalMainCalled.Store(true)
		return
	}
	os.Exit(childMain(os.Stdin, os.Stdout, os.Stderr))
}

func childMain(stdin io.Reader, stdout, stderr io.Writer) int {
	data, err := io.ReadAll(io.LimitReader(stdin, MaxRequestBytes+1))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "patrol-jsonnet: read request: %v\n", err)
		return exitProtocol
	}
	if len(data) > MaxRequestBytes {
		_, _ = fmt.Fprintln(stderr, "patrol-jsonnet: request too large")
		return exitProtocol
	}
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		_, _ = fmt.Fprintf(stderr, "patrol-jsonnet: decode request: %v\n", err)
		return exitProtocol
	}
	resp, err := Evaluate(req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "patrol-jsonnet: %v\n", err)
		return exitProtocol
	}
	if err := json.NewEncoder(stdout).Encode(resp); err != nil {
		_, _ = fmt.Fprintf(stderr, "patrol-jsonnet: write response: %v\n", err)
		return exitProtocol
	}
	return exitOK
}

// ErrChildFailed reports an evaluator child that did not produce a response
// (crash, resource limit, timeout, missing MaybeJsonnetMain).
var ErrChildFailed = errors.New("jsonnet evaluator failed")

// Runner evaluates Requests in a sandboxed child process.
type Runner struct {
	// Path is the evaluator binary. Empty means os.Executable(), which must call
	// MaybeJsonnetMain (Run refuses otherwise, so a binary without the hook is never
	// re-executed with unexpected arguments).
	Path string
	// Limits bound the child. Zero Timeout, CPUSeconds and AddressSpaceBytes take
	// DefaultTimeout, DefaultCPUSeconds and DefaultAddressSpaceBytes; a negative
	// Timeout disables the wall-clock limit (the context still applies). MaxStdout
	// defaults to the request's output cap plus framing. CPU and address-space limits
	// need the sandbox helper (sandbox.MaybeHelperMain in the same binary).
	Limits sandbox.Limits
	// NoRlimits disables CPU/address-space/open-file limits (for binaries that do
	// not run the sandbox helper).
	NoRlimits bool
}

var executable = sync.OnceValues(os.Executable)

// Run evaluates req in a child process and returns its Response. Evaluation errors
// are reported in Response.Error; err is non-nil only when the child could not be
// run or did not answer (ErrChildFailed wraps the cause; context errors and
// sandbox.ErrTimeout / sandbox.ErrOutputLimit are preserved).
func (r Runner) Run(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	if len(req.Files) == 0 {
		return Response{Outputs: []json.RawMessage{}}, nil
	}
	path := r.Path
	if path == "" {
		if !evalMainCalled.Load() {
			return Response{}, fmt.Errorf("%w: jsonnet.MaybeJsonnetMain was not called in this binary", ErrChildFailed)
		}
		p, err := executable()
		if err != nil {
			return Response{}, fmt.Errorf("%w: locate executable: %w", ErrChildFailed, err)
		}
		path = p
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("jsonnet: encode request: %w", err)
	}
	if len(body) > MaxRequestBytes {
		return Response{}, fmt.Errorf("%w: request too large", ErrInvalidRequest)
	}
	lim := r.Limits
	if lim.Timeout == 0 {
		lim.Timeout = DefaultTimeout
	}
	if r.NoRlimits {
		lim.CPUSeconds, lim.AddressSpaceBytes, lim.MaxOpenFiles = 0, 0, 0
	} else {
		if lim.CPUSeconds == 0 {
			lim.CPUSeconds = DefaultCPUSeconds
		}
		if lim.AddressSpaceBytes == 0 {
			lim.AddressSpaceBytes = DefaultAddressSpaceBytes
		}
	}
	if lim.MaxStdout == 0 {
		maxOut := req.MaxOutputBytes
		if maxOut <= 0 {
			maxOut = DefaultMaxOutputBytes
		}
		// JSON re-encoding of the outputs plus framing; outputs are compacted, so
		// they never grow beyond their size, and escaping only applies to the
		// error message.
		lim.MaxStdout = maxOut + 1<<20
	}
	if lim.MaxStderr == 0 {
		lim.MaxStderr = 1 << 20
	}
	res, err := sandbox.Run(ctx, sandbox.Cmd{
		Path:   path,
		Args:   []string{EvalArg},
		Env:    sandbox.BaseEnv(),
		Stdin:  bytes.NewReader(body),
		Limits: lim,
	})
	if err != nil {
		var ee *sandbox.ExitError
		if errors.As(err, &ee) {
			msg := strings.TrimSpace(ee.StderrTail)
			if msg == "" {
				msg = ee.Error()
			}
			return Response{}, fmt.Errorf("%w: %s", ErrChildFailed, msg)
		}
		return Response{}, fmt.Errorf("%w: %w", ErrChildFailed, err)
	}
	var resp Response
	dec := json.NewDecoder(bytes.NewReader(res.Stdout))
	if err := dec.Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("%w: decode response: %w", ErrChildFailed, err)
	}
	if resp.Error == nil && len(resp.Outputs) != len(req.Files) {
		return Response{}, fmt.Errorf("%w: response has %d outputs for %d files", ErrChildFailed, len(resp.Outputs), len(req.Files))
	}
	if resp.Error != nil && (resp.Error.Index < 0 || resp.Error.Index >= len(req.Files) || resp.Error.Index != len(resp.Outputs)) {
		return Response{}, fmt.Errorf("%w: inconsistent error index", ErrChildFailed)
	}
	return resp, nil
}
