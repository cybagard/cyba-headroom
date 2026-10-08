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
	"syscall"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// connTimeout bounds one client exchange so a stuck client cannot pin a goroutine.
const connTimeout = 2 * time.Second

// Listen binds the daemon socket at path. An exclusive lock on path+".lock",
// held until the listener closes, makes the daemon a singleton: a second
// daemon fails here even if the live one's socket is gone or slow to answer.
// Holding the lock also proves any existing socket is stale, so it is removed.
// The socket is created owner-only, with no window where others can connect.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	lockPath := path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("daemon: another daemon is already running (%s is locked)", lockPath)
		}
		return nil, fmt.Errorf("daemon: locking %s: %w", lockPath, err)
	}
	ln, err := listenLocked(path)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &lockedListener{Listener: ln, lock: lock}, nil
}

func listenLocked(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("daemon: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("daemon: removing stale socket: %w", err)
		}
	}
	// Umask is process-wide; Listen runs once at startup, before other goroutines.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return ln, nil
}

// lockedListener releases the singleton lock when the listener closes.
type lockedListener struct {
	net.Listener
	lock *os.File
	once sync.Once
}

func (l *lockedListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { _ = l.lock.Close() })
	return err
}

// Serve answers clients on ln until ctx ends, then closes ln (which removes
// the socket file) and waits for in-flight replies.
func (d *Daemon) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	var backoff time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return fmt.Errorf("daemon: accept: %w", err)
			}
			// Temporary, e.g. EMFILE under a burst of shim calls: back off and
			// keep serving, as net/http does, rather than taking the daemon down.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			d.log.Warn("accept failed, retrying", "err", err, "in", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
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
	case protocol.OpCheck:
		if d.check == nil || req.Check == nil {
			return protocol.Reply{Error: "check: not supported by this daemon"}
		}
		dec := d.check(req.Check, d.Snapshot())
		return protocol.Reply{OK: true, Decision: &dec}
	case protocol.OpRelease:
		if d.release == nil {
			return protocol.Reply{Error: "release: not supported by this daemon"}
		}
		if req.Release == "" {
			return protocol.Reply{Error: "release: no lease given"}
		}
		if !d.release(req.Release) {
			return protocol.Reply{Error: "release: no open lease " + req.Release}
		}
		return protocol.Reply{OK: true}
	case protocol.OpPing:
		return protocol.Reply{OK: true, PID: os.Getpid()}
	case protocol.OpStatus:
		return protocol.Reply{OK: true, Snapshot: d.Snapshot()}
	default:
		return protocol.Reply{Error: fmt.Sprintf("unknown op %q", req.Op)}
	}
}
