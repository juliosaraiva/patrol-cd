// Command imagelayout checks the filesystem of a built container image against
// hack/images/rootfs-expected.txt, the layout the Dockerfile promises (users, modes, mount
// points, multi-call links, files that must be absent). hack/images/rootfs-check.sh checks the
// same file against a root filesystem assembled without Docker; this command checks what
// BuildKit actually produced.
//
// Usage:
//
//	go run ./hack/imagelayout [-expected hack/images/rootfs-expected.txt] [-docker docker] IMAGE
//
// The image is exported through a throwaway container (docker create / export / rm); nothing is
// started. Exit status 0 when every expectation holds, 1 when one fails, 2 on usage or runtime
// errors.
package main

import (
	"archive/tar"
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// expectation is one line of rootfs-expected.txt: TYPE MODE OWNER PATH [TARGET].
type expectation struct {
	kind   string // d, f, x, l or absent
	mode   string // octal, or "-" when not checked
	owner  string // uid:gid, or "-"
	path   string
	target string // link target (kind l)
	line   int
}

// entry is what the image holds at a path.
type entry struct {
	typeflag byte
	mode     int64
	uid, gid int
	link     string
}

func main() {
	expected := flag.String("expected", "hack/images/rootfs-expected.txt", "expected layout file")
	docker := flag.String("docker", "docker", "docker executable")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: imagelayout [-expected FILE] [-docker CMD] IMAGE")
		os.Exit(2)
	}
	f, err := os.Open(*expected)
	if err != nil {
		fmt.Fprintln(os.Stderr, "imagelayout:", err)
		os.Exit(2)
	}
	exps, err := parseExpected(f)
	_ = f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "imagelayout:", err)
		os.Exit(2)
	}
	entries, err := exportImage(*docker, flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "imagelayout:", err)
		os.Exit(2)
	}
	failures := check(exps, entries)
	for _, m := range failures {
		fmt.Println("FAIL", m)
	}
	fmt.Printf("imagelayout: %d expectations checked against %d paths of %s; %d failed\n",
		len(exps), len(entries), flag.Arg(0), len(failures))
	if len(failures) > 0 {
		os.Exit(1)
	}
}

// parseExpected reads the expectation lines, skipping blank lines and comments.
func parseExpected(r io.Reader) ([]expectation, error) {
	var out []expectation
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) < 4 {
			return nil, fmt.Errorf("line %d: want TYPE MODE OWNER PATH [TARGET], got %q", n, line)
		}
		e := expectation{kind: fs[0], mode: fs[1], owner: fs[2], path: fs[3], line: n}
		switch e.kind {
		case "d", "f", "x", "absent":
			if len(fs) != 4 {
				return nil, fmt.Errorf("line %d: unexpected fields after the path in %q", n, line)
			}
		case "l":
			if len(fs) != 5 {
				return nil, fmt.Errorf("line %d: a link needs a target: %q", n, line)
			}
			e.target = fs[4]
		default:
			return nil, fmt.Errorf("line %d: unknown type %q", n, e.kind)
		}
		if e.mode != "-" {
			if _, err := strconv.ParseInt(e.mode, 8, 32); err != nil {
				return nil, fmt.Errorf("line %d: bad mode %q", n, e.mode)
			}
		}
		if !strings.HasPrefix(e.path, "/") {
			return nil, fmt.Errorf("line %d: path %q is not absolute", n, e.path)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// readTar indexes a docker export stream by absolute path.
func readTar(r io.Reader) (map[string]entry, error) {
	entries := map[string]entry{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		p := "/" + strings.Trim(strings.TrimPrefix(h.Name, "./"), "/")
		entries[p] = entry{typeflag: h.Typeflag, mode: h.Mode & 0o7777, uid: h.Uid, gid: h.Gid, link: h.Linkname}
	}
}

// exportImage creates a container from image (never started), reads its filesystem and removes it.
func exportImage(docker, image string) (map[string]entry, error) {
	out, err := exec.Command(docker, "create", image, "true").Output()
	if err != nil {
		return nil, fmt.Errorf("docker create %s: %w", image, err)
	}
	id := strings.TrimSpace(string(out))
	defer exec.Command(docker, "rm", "-f", id).Run() //nolint:errcheck // best-effort cleanup of the throwaway container
	cmd := exec.Command(docker, "export", id)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	entries, rerr := readTar(pipe)
	if _, derr := io.Copy(io.Discard, pipe); derr != nil && rerr == nil {
		rerr = derr
	}
	if werr := cmd.Wait(); werr != nil {
		return nil, fmt.Errorf("docker export: %w", werr)
	}
	return entries, rerr
}

// check returns one message per expectation that does not hold, in file order.
func check(exps []expectation, entries map[string]entry) []string {
	var fails []string
	fail := func(e expectation, format string, a ...any) {
		fails = append(fails, fmt.Sprintf("line %d %s: ", e.line, e.path)+fmt.Sprintf(format, a...))
	}
	for _, e := range exps {
		got, present := entries[e.path]
		if e.kind == "absent" {
			if present {
				fail(e, "present but must be absent")
			}
			continue
		}
		if !present {
			fail(e, "missing")
			continue
		}
		var typeOK bool
		switch e.kind {
		case "d":
			typeOK = got.typeflag == tar.TypeDir
		case "f", "x":
			typeOK = got.typeflag == tar.TypeReg || got.typeflag == tar.TypeLink
		case "l":
			typeOK = got.typeflag == tar.TypeSymlink
		}
		if !typeOK {
			fail(e, "type %q, want %q", string(rune(got.typeflag)), e.kind)
			continue
		}
		if e.kind == "l" {
			if got.link != e.target {
				fail(e, "link target %q, want %q", got.link, e.target)
			}
		} else if e.mode != "-" {
			if want, _ := strconv.ParseInt(e.mode, 8, 32); got.mode != want {
				fail(e, "mode %04o, want %s", got.mode, e.mode)
			}
		}
		if e.owner != "-" {
			if have := fmt.Sprintf("%d:%d", got.uid, got.gid); have != e.owner {
				fail(e, "owner %s, want %s", have, e.owner)
			}
		}
	}
	return fails
}
