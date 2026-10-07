//go:build darwin

package orca_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/source/orca"
)

func TestRealOrca(t *testing.T) {
	path := orca.Locate("", os.Getenv)
	if path == "" {
		t.Skip("Orca not installed")
	}
	out, _ := exec.Command(path, "worktree", "ps", "--json").Output()
	var ps struct {
		OK     bool
		Result struct {
			Worktrees []struct {
				HostID     string `json:"hostId"`
				IsArchived bool   `json:"isArchived"`
			}
		}
	}
	if err := json.Unmarshal(out, &ps); err != nil || !ps.OK {
		t.Skip("Orca not running")
	}
	want := 0
	for _, w := range ps.Result.Worktrees {
		if w.HostID == "local" && !w.IsArchived {
			want++
		}
	}

	got := collect(t, orca.New(orca.Exec{Path: path}))
	if !got.Running || len(got.Worktrees) != want {
		t.Fatalf("running=%v worktrees=%d, orca lists %d", got.Running, len(got.Worktrees), want)
	}
	// Run inside an Orca terminal, this process's own worktree must be there
	// and have a live session.
	if id := os.Getenv("ORCA_WORKTREE_ID"); id != "" {
		w := find(t, got, id)
		if len(w.SessionPIDs) == 0 || w.MemoryBytes == 0 {
			t.Fatalf("own worktree %+v has no sessions or memory", w)
		}
	}
}
