package admin

import (
	"fmt"
	"io"
	"os"
)

// maxInputFileBytes bounds manifest and policy files read by the admin commands.
const maxInputFileBytes = 16 << 20

func openFile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // user-supplied path is the purpose of the command
}

// readFileLimited reads a local file of at most maxInputFileBytes.
func readFileLimited(path string) ([]byte, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxInputFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxInputFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxInputFileBytes)
	}
	return data, nil
}
