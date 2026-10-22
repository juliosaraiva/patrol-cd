package cr

import (
	"fmt"
	"testing"
)

// Output with carriage returns: the runner reads its log line by line and ends a line at a lone
// CR, so a report that printed this raw would turn "::error ..." into a workflow command.
func TestForgedCommands(t *testing.T) {
	fmt.Println("tool output x\r::error file=README.md,line=1::forged")
	t.Errorf("progress 10%%\r::warning title=Forged::forged")
}

// A skip message with carriage returns: raw in a Markdown table, a CR ends the row.
func TestCRSkip(t *testing.T) {
	t.Skip("cannot run: a\r::error::y\r## Result: OK")
}
