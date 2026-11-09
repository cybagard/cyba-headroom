package daemon

import (
	"io/fs"
	"testing"
)

// SetStat replaces how Listen and CheckDir stat the socket's dir until t ends.
func SetStat(t *testing.T, f func(string) (fs.FileInfo, error)) {
	old := stat
	stat = f
	t.Cleanup(func() { stat = old })
}
