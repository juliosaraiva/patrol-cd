package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// askpassPayload is the content of the per-call credentials file read by the
// askpass helper.
type askpassPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// maxAskpassFile bounds the credentials file the helper is willing to read.
const maxAskpassFile = 64 << 10

var (
	askpassMu       sync.RWMutex
	askpassOverride string
	executablePath  = sync.OnceValues(os.Executable)
)

// SetAskpassPath overrides the program used as GIT_ASKPASS. An empty path restores
// the default, os.Executable(). The program must call MaybeAskpassMain at the start
// of main (or TestMain).
func SetAskpassPath(path string) {
	askpassMu.Lock()
	defer askpassMu.Unlock()
	askpassOverride = path
}

func askpassExecutable() (string, error) {
	askpassMu.RLock()
	p := askpassOverride
	askpassMu.RUnlock()
	if p == "" {
		var err error
		if p, err = executablePath(); err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("askpass helper path %q is not absolute", p)
	}
	return p, nil
}

// MaybeAskpassMain must be called first thing in main of every binary that uses
// this package with HTTPS credentials (cmd/patrolcd), and in TestMain of test
// binaries that do. When the process was started by git as GIT_ASKPASS (the
// credentials-file variable is set and exactly one argument, the prompt, is
// given) it answers the prompt from the per-call credentials file and exits.
// Otherwise it returns immediately without side effects.
//
// The helper answers only "Username for '<url>'" and "Password for '<url>'"
// prompts whose URL has the same origin (scheme, host, port) as the repository
// the client was created for; any other prompt fails, so credentials are never
// sent to another host (for example a submodule or redirect target).
func MaybeAskpassMain() {
	file := os.Getenv(askpassFileEnv)
	if file == "" || len(os.Args) != 2 {
		return
	}
	os.Exit(askpassMain(os.Args[1], file, os.Getenv(askpassOriginEnv), os.Stdout, os.Stderr))
}

var promptRe = regexp.MustCompile(`^(Username|Password) for ['"]([^'"]+)['"]:?\s*$`)

func askpassMain(prompt, file, origin string, stdout, stderr io.Writer) int {
	m := promptRe.FindStringSubmatch(strings.TrimSpace(prompt))
	if m == nil {
		_, _ = fmt.Fprintln(stderr, "patrol-git-askpass: unsupported prompt")
		return 1
	}
	kind, promptURL := m[1], m[2]
	if origin == "" || !sameOrigin(httpOrigin(promptURL), origin) {
		_, _ = fmt.Fprintln(stderr, "patrol-git-askpass: credentials are only offered to the repository origin")
		return 1
	}
	payload, err := readAskpassFile(file)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "patrol-git-askpass: %v\n", err)
		return 1
	}
	answer := payload.Username
	if kind == "Password" {
		answer = payload.Password
	}
	if strings.ContainsAny(answer, "\r\n") {
		_, _ = fmt.Fprintln(stderr, "patrol-git-askpass: credential contains a line break")
		return 1
	}
	if _, err := fmt.Fprintln(stdout, answer); err != nil {
		return 1
	}
	return 0
}

func readAskpassFile(path string) (askpassPayload, error) {
	var p askpassPayload
	if !filepath.IsAbs(path) {
		return p, errors.New("credentials file path is not absolute")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return p, fmt.Errorf("stat credentials file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return p, errors.New("credentials file is not a regular file")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return p, errors.New("credentials file is accessible by other users")
	}
	f, err := os.Open(path)
	if err != nil {
		return p, fmt.Errorf("open credentials file: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxAskpassFile+1))
	if err != nil {
		return p, fmt.Errorf("read credentials file: %w", err)
	}
	if len(data) > maxAskpassFile {
		return p, errors.New("credentials file too large")
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, errors.New("credentials file is malformed")
	}
	return p, nil
}
