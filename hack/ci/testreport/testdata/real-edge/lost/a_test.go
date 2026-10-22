package lost

import (
	"fmt"
	"strings"
	"testing"
)

// Each test logs a message that the allowlist matches, prints output without a newline, then
// skips for a reason the allowlist does not match: go puts the t.Skip header on that line.

// More than 8 KiB of carriage-return progress: the header lies past the part testreport keeps.
func TestLongLine(t *testing.T) {
	t.Log(`capability "x" not declared`)
	fmt.Print(strings.Repeat("\rprogress 50%", 800))
	t.Skip("git-lfs not found")
}

// A lone tab before the header.
func TestTabFragment(t *testing.T) {
	t.Log(`capability "x" not declared`)
	fmt.Print("columns:\n\t")
	t.Skip("git-lfs not found")
}

// A lone carriage return before the header.
func TestBareCR(t *testing.T) {
	t.Log(`capability "x" not declared`)
	fmt.Print("\r")
	t.Skip("git-lfs not found")
}

// Short carriage-return progress before the header.
func TestShortCR(t *testing.T) {
	t.Log(`capability "x" not declared`)
	fmt.Print("\rprogress 10%\rprogress 50%")
	t.Skip("git-lfs not found")
}
