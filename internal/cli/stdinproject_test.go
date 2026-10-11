package cli

import "github.com/cybagard/cyba-headroom/internal/shim"

// composeStdinProject is composeStdinKey's project alone, which the tests of
// a stdin name check: an unknown one is the directory's.
func composeStdinProject(c shim.Call, lookupEnv func(string) (string, bool), getwd func() (string, error)) string {
	project, _ := composeStdinKey(c, lookupEnv, getwd)
	return project
}
