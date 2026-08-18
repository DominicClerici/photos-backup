package video

import (
	"bytes"
	"os/exec"
)

// execOutput runs a helper command in tests, where the point is to verify the
// output with a tool other than the one that produced it.
func execOutput(binary string, args ...string) (string, error) {
	cmd := exec.Command(binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}
