package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// connTimeout bounds one client exchange so a stuck client cannot pin a goroutine.
const connTimeout = 2 * time.Second

// staleProbe is how long Listen waits for an existing daemon to answer.
const staleProbe = 200 * time.Millisecond

// Listen binds the daemon socket at path. It refuses if another daemon is
// answering there, removes a stale socket left by a crash, and restricts the
// socket to the current user.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("daemon: %s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, staleProbe); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("daemon: another daemon is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("daemon: removing stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return ln, nil
}

// Serve answers clients on ln until ctx ends, then closes ln (which removes
// the socket file) and waits for in-flight replies.
func (d *Daemon) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("daemon: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.handle(c)
		}()
	}
}

func (d *Daemon) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(connTimeout))
	rep := d.answer(c)
	rep.V = protocol.Version
	if err := json.NewEncoder(c).Encode(rep); err != nil {
		d.log.Debug("reply failed", "err", err)
	}
}

func (d *Daemon) answer(c net.Conn) protocol.Reply {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 4096), protocol.MaxLine)
	if !sc.Scan() {
		err := sc.Err()
		if err == nil {
			err = errors.New("empty request")
		}
		return protocol.Reply{Error: "read request: " + err.Error()}
	}
	var req protocol.Request
	if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
		return protocol.Reply{Error: "bad request: " + err.Error()}
	}
	if req.V != protocol.Version {
		return protocol.Reply{Error: fmt.Sprintf("unsupported protocol version %d, daemon speaks %d", req.V, protocol.Version)}
	}
	switch req.Op {
	case protocol.OpPing:
		return protocol.Reply{OK: true}
	case protocol.OpStatus:
		return protocol.Reply{OK: true, Snapshot: d.Snapshot()}
	default:
		return protocol.Reply{Error: fmt.Sprintf("unknown op %q", req.Op)}
	}
}
