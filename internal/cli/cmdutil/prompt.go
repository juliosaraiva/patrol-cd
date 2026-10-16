package cmdutil

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// maxAnswerBytes bounds one line read from the terminal.
const maxAnswerBytes = 4096

// ErrNoInput is returned when a prompt needs an answer and the input is exhausted.
var ErrNoInput = errors.New("no input available to answer the prompt")

// Prompter asks questions on the command's input. Prompts are written to the error
// stream so that the standard output stays machine readable. Passwords are read without
// echo when the input is a terminal.
type Prompter struct {
	in  io.Reader
	r   *bufio.Reader
	out io.Writer
	// ReadPassword reads a line without echo from a terminal file descriptor
	// (term.ReadPassword); tests replace it.
	ReadPassword func(fd int) ([]byte, error)
}

// NewPrompter returns a prompter reading in and writing prompts to out.
func NewPrompter(in io.Reader, out io.Writer) *Prompter {
	return &Prompter{in: in, r: bufio.NewReader(in), out: out, ReadPassword: term.ReadPassword}
}

// line reads one line (without the newline); EOF after partial input returns the input.
func (p *Prompter) line() (string, error) {
	var b strings.Builder
	for {
		c, err := p.r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && b.Len() > 0 {
				return b.String(), nil
			}
			if errors.Is(err, io.EOF) {
				return "", ErrNoInput
			}
			return "", err
		}
		if c == '\n' {
			return strings.TrimSuffix(b.String(), "\r"), nil
		}
		if b.Len() >= maxAnswerBytes {
			return "", fmt.Errorf("answer exceeds %d bytes", maxAnswerBytes)
		}
		b.WriteByte(c)
	}
}

// Confirm asks msg until the answer is y/yes or n/no.
func (p *Prompter) Confirm(msg string) (bool, error) {
	for {
		_, _ = fmt.Fprint(p.out, msg)
		a, err := p.line()
		if err != nil {
			_, _ = fmt.Fprintln(p.out)
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(a)) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}

// ConfirmAll asks msg until the answer is y/yes, n/no or a/all (yes to every remaining
// item).
func (p *Prompter) ConfirmAll(msg string) (yes, all bool, err error) {
	for {
		_, _ = fmt.Fprint(p.out, msg)
		a, err := p.line()
		if err != nil {
			_, _ = fmt.Fprintln(p.out)
			return false, false, err
		}
		switch strings.ToLower(strings.TrimSpace(a)) {
		case "y", "yes":
			return true, false, nil
		case "n", "no":
			return false, false, nil
		case "a", "all":
			return true, true, nil
		}
	}
}

// Input asks for a non-empty value (echoed).
func (p *Prompter) Input(msg string) (string, error) {
	for {
		_, _ = fmt.Fprint(p.out, msg+": ")
		a, err := p.line()
		if err != nil {
			_, _ = fmt.Fprintln(p.out)
			return "", err
		}
		if v := strings.TrimSpace(a); v != "" {
			return v, nil
		}
	}
}

// Password asks for a non-empty secret without echo (when the input is a terminal).
func (p *Prompter) Password(msg string) (string, error) {
	for {
		_, _ = fmt.Fprint(p.out, msg+": ")
		v, err := p.secret()
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
	}
}

func (p *Prompter) secret() (string, error) {
	if f, ok := p.in.(*os.File); ok && term.IsTerminal(int(f.Fd())) && p.ReadPassword != nil {
		b, err := p.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(p.out)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	a, err := p.line()
	if err != nil {
		_, _ = fmt.Fprintln(p.out)
		return "", err
	}
	return strings.TrimSpace(a), nil
}

// NewPassword asks for a password twice until both entries match.
func (p *Prompter) NewPassword(user string) (string, error) {
	for range 5 {
		first, err := p.Password(fmt.Sprintf("*** Enter new password for user %s", user))
		if err != nil {
			return "", err
		}
		second, err := p.Password(fmt.Sprintf("*** Confirm new password for user %s", user))
		if err != nil {
			return "", err
		}
		if first == second {
			return first, nil
		}
		_, _ = fmt.Fprintln(p.out, "Passwords do not match")
	}
	return "", errors.New("passwords do not match")
}

// OptionalPrompt asks confirmation questions only when prompts are enabled (upstream
// utils.Prompt); disabled prompts answer yes.
type OptionalPrompt struct {
	Enabled bool
	P       *Prompter
}

// Confirm asks msg when enabled.
func (o OptionalPrompt) Confirm(msg string) (bool, error) {
	if !o.Enabled {
		return true, nil
	}
	return o.P.Confirm(msg)
}

// ConfirmBaseOnCount asks single for one item and multi (y/n/a) for several.
func (o OptionalPrompt) ConfirmBaseOnCount(single, multi string, count int) (yes, all bool, err error) {
	if !o.Enabled || count == 0 {
		return true, true, nil
	}
	if count == 1 {
		yes, err := o.P.Confirm(single)
		return yes, false, err
	}
	return o.P.ConfirmAll(multi)
}
