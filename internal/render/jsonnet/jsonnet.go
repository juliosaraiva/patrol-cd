// Package jsonnet evaluates the Jsonnet files of Directory sources (ADR-0007).
//
// Evaluation runs in a child process so it can be time-, CPU- and memory-limited and
// killed: Runner re-executes the current binary (os.Executable by default) with
// EvalArg as its first argument through internal/sandbox, writes a JSON Request on its
// stdin and reads a JSON Response from its stdout. The binary's main (cmd/patrolcd,
// and TestMain of test binaries that use Runner) must call MaybeJsonnetMain first
// thing, after sandbox.MaybeHelperMain.
//
// Evaluate is the in-process evaluator the child runs. It applies the baseline import
// confinement: every import/importstr/importbin is located with stat calls only, its
// symlink-resolved path must lie inside RepoRoot and be a regular file, and only then
// is it read (with a size cap). A fresh VM is used per file, as upstream.
package jsonnet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	gojsonnet "github.com/google/go-jsonnet"
)

// Defaults applied when the corresponding Request field is zero.
const (
	// DefaultMaxImportBytes caps one imported (or evaluated) file.
	DefaultMaxImportBytes int64 = 64 << 20
	// DefaultMaxOutputBytes caps the combined JSON output of one Request.
	DefaultMaxOutputBytes int64 = 256 << 20
	// DefaultMaxStack is go-jsonnet's default stack depth.
	DefaultMaxStack = 500
	// MaxFiles bounds the number of files in one Request.
	MaxFiles = 10000
)

// Var is an external variable or top-level argument.
type Var struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// Code evaluates Value as Jsonnet code instead of binding it as a string.
	Code bool `json:"code,omitempty"`
}

// Request describes one evaluation batch. Paths are absolute.
type Request struct {
	// RepoRoot is the confinement root for every file read.
	RepoRoot string `json:"repoRoot"`
	// JPaths are the library search directories in declaration order (upstream: the
	// application directory first, then the resolved libs). Lookup tries the
	// importing file's directory, then JPaths from last to first.
	JPaths []string `json:"jpaths,omitempty"`
	// ExtVars and TLAs are bound on every VM (already environment-substituted).
	ExtVars []Var `json:"extVars,omitempty"`
	TLAs    []Var `json:"tlas,omitempty"`
	// Files are evaluated in order, each with a fresh VM. Evaluation stops at the
	// first error.
	Files []string `json:"files"`
	// MaxImportBytes caps each file read; zero means DefaultMaxImportBytes.
	MaxImportBytes int64 `json:"maxImportBytes,omitempty"`
	// MaxOutputBytes caps the combined output; zero means DefaultMaxOutputBytes.
	MaxOutputBytes int64 `json:"maxOutputBytes,omitempty"`
	// MaxStack is the VM stack depth; zero means DefaultMaxStack.
	MaxStack int `json:"maxStack,omitempty"`
}

// Response is the result of a Request: one output per evaluated file (the JSON text
// produced by Jsonnet), and the first evaluation error, if any.
type Response struct {
	Outputs []json.RawMessage `json:"outputs"`
	Error   *EvalError        `json:"error,omitempty"`
}

// EvalError reports the failure of Files[Index].
type EvalError struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}

func (e *EvalError) Error() string { return e.Message }

// ErrInvalidRequest reports a malformed Request.
var ErrInvalidRequest = errors.New("jsonnet: invalid request")

// Validate checks the request shape.
func (r Request) Validate() error {
	if !filepath.IsAbs(r.RepoRoot) {
		return fmt.Errorf("%w: repository root must be absolute", ErrInvalidRequest)
	}
	if len(r.Files) > MaxFiles {
		return fmt.Errorf("%w: too many files", ErrInvalidRequest)
	}
	for _, f := range r.Files {
		if !filepath.IsAbs(f) {
			return fmt.Errorf("%w: file paths must be absolute", ErrInvalidRequest)
		}
	}
	for _, p := range r.JPaths {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("%w: library paths must be absolute", ErrInvalidRequest)
		}
	}
	return nil
}

// Evaluate evaluates req.Files in process with import confinement. Setup problems
// (an invalid request, an unresolvable repository root) are returned as errors;
// evaluation problems are reported in Response.Error.
func Evaluate(req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	root, err := filepath.EvalSymlinks(req.RepoRoot)
	if err != nil {
		return Response{}, fmt.Errorf("jsonnet: resolve repository root: %w", err)
	}
	maxImport := req.MaxImportBytes
	if maxImport <= 0 {
		maxImport = DefaultMaxImportBytes
	}
	maxOut := req.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultMaxOutputBytes
	}
	stack := req.MaxStack
	if stack <= 0 {
		stack = DefaultMaxStack
	}
	resp := Response{Outputs: make([]json.RawMessage, 0, len(req.Files))}
	var total int64
	for i, file := range req.Files {
		vm := gojsonnet.MakeVM()
		vm.MaxStack = stack
		for _, v := range req.TLAs {
			if v.Code {
				vm.TLACode(v.Name, v.Value)
			} else {
				vm.TLAVar(v.Name, v.Value)
			}
		}
		for _, v := range req.ExtVars {
			if v.Code {
				vm.ExtCode(v.Name, v.Value)
			} else {
				vm.ExtVar(v.Name, v.Value)
			}
		}
		vm.Importer(&confinedImporter{
			jpaths:   slices.Clone(req.JPaths),
			root:     root,
			maxBytes: maxImport,
			cache:    map[string]gojsonnet.Contents{},
		})
		out, err := vm.EvaluateFile(file)
		if err != nil {
			resp.Error = &EvalError{Index: i, Message: err.Error()}
			return resp, nil
		}
		total += int64(len(out))
		if total > maxOut {
			resp.Error = &EvalError{Index: i, Message: fmt.Sprintf("jsonnet output exceeds %d bytes", maxOut)}
			return resp, nil
		}
		resp.Outputs = append(resp.Outputs, json.RawMessage(out))
	}
	return resp, nil
}

// confinedImporter locates imports like go-jsonnet's FileImporter (the importing
// file's directory, then the library paths from last to first) using stat calls
// only, validates that the symlink-resolved location is a regular file inside the
// repository root, and only then reads it, with a size cap. Contents are cached per
// location, as the go-jsonnet Importer contract requires.
type confinedImporter struct {
	jpaths   []string
	root     string
	maxBytes int64
	cache    map[string]gojsonnet.Contents
}

func (i *confinedImporter) Import(importedFrom, importedPath string) (gojsonnet.Contents, string, error) {
	dir, _ := filepath.Split(importedFrom)
	foundAt, err := i.locate(dir, importedPath)
	if err != nil {
		return gojsonnet.Contents{}, "", err
	}
	if c, ok := i.cache[foundAt]; ok {
		return c, foundAt, nil
	}
	resolved, err := i.validate(foundAt, importedPath)
	if err != nil {
		return gojsonnet.Contents{}, "", err
	}
	data, err := readBounded(resolved, i.maxBytes)
	if err != nil {
		return gojsonnet.Contents{}, "", fmt.Errorf("failed to read jsonnet import %q: %w", importedPath, err)
	}
	c := gojsonnet.MakeContentsRaw(data)
	i.cache[foundAt] = c
	return c, foundAt, nil
}

func (i *confinedImporter) locate(dir, importedPath string) (string, error) {
	notFound := fmt.Errorf("couldn't open import %q: no match locally or in the Jsonnet library paths", importedPath)
	if strings.ContainsRune(importedPath, 0) {
		return "", notFound
	}
	if filepath.IsAbs(importedPath) {
		if _, err := os.Lstat(importedPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", notFound
			}
			return "", fmt.Errorf("failed to stat jsonnet import %q: %w", importedPath, err)
		}
		return importedPath, nil
	}
	bases := make([]string, 0, len(i.jpaths)+1)
	bases = append(bases, dir)
	for _, p := range slices.Backward(i.jpaths) {
		bases = append(bases, p)
	}
	for _, base := range bases {
		candidate := filepath.Join(base, importedPath)
		if _, err := os.Lstat(candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("failed to stat jsonnet import %q: %w", importedPath, err)
		}
	}
	return "", notFound
}

func (i *confinedImporter) validate(foundAt, importedPath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(foundAt)
	if err != nil {
		return "", fmt.Errorf("failed to resolve jsonnet import %q: %w", importedPath, err)
	}
	if !within(i.root, resolved) {
		return "", fmt.Errorf("jsonnet import %q resolves outside repository root", importedPath)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("failed to stat jsonnet import %q: %w", importedPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("jsonnet import %q is not a regular file", importedPath)
	}
	return resolved, nil
}

// within reports whether p is root or below it (both already symlink-resolved).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// readBounded reads a regular file of at most limit bytes.
func readBounded(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}
