package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// commit is one non-merge commit of the release range, parsed as a conventional commit.
type commit struct {
	SHA      string
	Subject  string
	Body     string
	Type     string // empty when the subject is not a conventional commit
	Scope    string
	Desc     string
	Breaking bool
	// BreakingNote is the text of a BREAKING CHANGE footer (empty for a bare "!").
	BreakingNote string
	// PR is the pull request number the commit arrived with (0 when unknown).
	PR int
}

var (
	// type(scope)!: description
	subjectRe = regexp.MustCompile(`^([a-z]+)(?:\(([^()\r\n]+)\))?(!)?: (\S.*)$`)
	// BREAKING CHANGE: text (or BREAKING-CHANGE), a git trailer; the note runs to the next trailer
	// or the end of the body.
	breakingRe = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE: ?`)
	trailerRe  = regexp.MustCompile(`(?m)^[A-Za-z][A-Za-z-]*: `)
	// "Merge pull request #12 from owner/branch" (merge commits) and "subject (#12)" (squash merges).
	mergePRRe  = regexp.MustCompile(`^Merge pull request #([0-9]+) `)
	squashPRRe = regexp.MustCompile(`\s\(#([0-9]+)\)$`)
)

// parse fills Type, Scope, Desc, Breaking and BreakingNote from the subject and body, and PR
// from a squash merge's "(#N)" suffix.
func (c *commit) parse() {
	m := subjectRe.FindStringSubmatch(c.Subject)
	if m == nil {
		return
	}
	c.Type, c.Scope, c.Breaking, c.Desc = m[1], m[2], m[3] == "!", m[4]
	if c.PR == 0 {
		if pm := squashPRRe.FindStringSubmatch(c.Desc); pm != nil {
			c.PR, _ = strconv.Atoi(pm[1])
			c.Desc = strings.TrimSpace(strings.TrimSuffix(c.Desc, pm[0]))
		}
	}
	if loc := breakingRe.FindStringIndex(c.Body); loc != nil {
		c.Breaking = true
		note := c.Body[loc[1]:]
		if next := trailerRe.FindStringIndex(note); next != nil {
			note = note[:next[0]]
		}
		c.BreakingNote = strings.Join(strings.Fields(note), " ")
	}
}

// git runs git in dir and returns its stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

const (
	fieldSep  = "\x1f"
	recordSep = "\x1e"
)

// logEntry is one record of `git log` (merges included).
type logEntry struct {
	SHA, Subject, Body string
	Parents            []string
}

func gitLog(ctx context.Context, dir string, revs ...string) ([]logEntry, error) {
	args := append([]string{"log", "--format=%H" + fieldSep + "%P" + fieldSep + "%s" + fieldSep + "%b" + recordSep}, revs...)
	out, err := git(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var entries []logEntry
	for _, rec := range strings.Split(out, recordSep) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, fieldSep, 4)
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected git log record %q", rec)
		}
		entries = append(entries, logEntry{SHA: f[0], Parents: strings.Fields(f[1]), Subject: f[2], Body: strings.TrimSpace(f[3])})
	}
	return entries, nil
}

// collectCommits returns the non-merge commits of HEAD that are not reachable from since (all of
// HEAD's history when since is empty), newest first. A commit that arrived through a "Merge pull
// request #N" merge commit gets PR N.
func collectCommits(ctx context.Context, dir, since string) ([]commit, error) {
	rng := "HEAD"
	if since != "" {
		rng = since + "..HEAD"
	}
	entries, err := gitLog(ctx, dir, rng)
	if err != nil {
		return nil, err
	}
	prOf := map[string]int{}
	for _, e := range entries {
		m := mergePRRe.FindStringSubmatch(e.Subject)
		if m == nil || len(e.Parents) != 2 {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		shas, err := git(ctx, dir, "rev-list", e.Parents[0]+".."+e.Parents[1])
		if err != nil {
			return nil, err
		}
		for _, sha := range strings.Fields(shas) {
			if _, ok := prOf[sha]; !ok {
				prOf[sha] = n
			}
		}
	}
	var commits []commit
	for _, e := range entries {
		if len(e.Parents) > 1 {
			continue
		}
		c := commit{SHA: e.SHA, Subject: e.Subject, Body: e.Body, PR: prOf[e.SHA]}
		c.parse()
		commits = append(commits, c)
	}
	return commits, nil
}

// latestTag returns the newest release tag (vX.Y.Z) reachable from HEAD and its version, or ""
// when there is none.
func latestTag(ctx context.Context, dir string) (string, version, error) {
	out, err := git(ctx, dir, "tag", "--merged", "HEAD", "--list", "v[0-9]*")
	if err != nil {
		return "", version{}, err
	}
	var best string
	var bestV version
	for _, t := range strings.Fields(out) {
		v, err := parseVersion(t)
		if err != nil {
			continue
		}
		if best == "" || bestV.less(v) {
			best, bestV = t, v
		}
	}
	return best, bestV, nil
}

// tagExists reports whether the tag exists locally.
func tagExists(ctx context.Context, dir, tag string) (bool, error) {
	out, err := git(ctx, dir, "tag", "--list", tag)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == tag, nil
}
