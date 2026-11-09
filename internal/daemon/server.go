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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// connTimeout bounds one client exchange so a stuck client cannot pin a goroutine.
const connTimeout = 2 * time.Second

// ErrLocked is Listen's error while another daemon holds the lock.
var ErrLocked = errors.New("another daemon is already running")

// Listen binds the daemon socket at path. An exclusive lock on path+".lock",
// held until the listener closes, makes the daemon a singleton: a second
// daemon fails here even if the live one's socket is gone or slow to answer.
// Holding the lock also proves any existing socket is stale, so it is removed.
// The socket is created owner-only, with no window where others can connect.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if err := CheckDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	lockPath := path + ".lock"
	lock, err := OpenOwnFile(lockPath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("daemon: %w (%s is locked)", ErrLocked, lockPath)
		}
		return nil, fmt.Errorf("daemon: locking %s: %w", lockPath, err)
	}
	// The holder's PID, for a daemon refused to name it (#194).
	if err := lock.Truncate(0); err == nil {
		_, _ = lock.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	ln, err := listenLocked(path)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &lockedListener{Listener: ln, lock: lock}, nil
}

// stat is os.Stat, replaced in tests to fake another user's dir.
var stat = os.Stat

// CheckDir refuses a socket dir that another user owns or can write: they
// could plant links at the lock or squat the socket (#276). Only dir itself
// is checked, following a symlink to it.
func CheckDir(dir string) error {
	fi, err := stat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner to check", dir)
	}
	const fix = "put the socket in a directory only you can write, such as ~/.config/headroom"
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by other users (mode %s): %s", dir, octalMode(fi.Mode()), fix)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by another user (mode %s): %s", dir, octalMode(fi.Mode()), fix)
	}
	return nil
}

// octalMode shows m as chmod takes it, sticky bit included: 1777 for /tmp.
func octalMode(m fs.FileMode) string {
	o := uint32(m.Perm())
	if m&fs.ModeSticky != 0 {
		o |= 0o1000
	}
	if m&fs.ModeSetgid != 0 {
		o |= 0o2000
	}
	if m&fs.ModeSetuid != 0 {
		o |= 0o4000
	}
	return fmt.Sprintf("%04o", o)
}

// OpenOwnFile opens path, with flag, without following a symlink there, and
// only if it is a regular file this user owns with no other link: a link
// planted there is not written through (#276). A file it creates is 0600.
func OpenOwnFile(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || !fi.Mode().IsRegular() || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file only you own with one link: remove it", path)
	}
	return f, nil
}

// LockHolder is the PID the daemon holding the lock beside the socket at
// path wrote into it.
func LockHolder(path string) (int, error) {
	b, err := os.ReadFile(path + ".lock")
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("no PID in %s.lock", path)
	}
	return pid, nil
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
	var check *protocol.CheckRequest
	rep := d.answer(c, &check)
	rep.V = protocol.Version
	if err := json.NewEncoder(c).Encode(rep); err != nil {
		d.log.Debug("reply failed", "err", err)
		if rep.Decision != nil {
			d.undo(check, rep.Decision, err)
		}
	}
}

// undo settles a check whose reply was lost (its client closed, or
// connTimeout passed), whose call the shim ran ungated (#271). A labelled
// call's lease (a run or create) would never bind, its container lacking
// the label: it is released, as an exec failure's is. Any other binds by
// name what the call starts, so it stays (plan amendment 1). The ticket
// is dropped, so the calls behind it move up now. A reply written just
// before the client gave up still succeeds: that lease is held.
func (d *Daemon) undo(check *protocol.CheckRequest, dec *protocol.Decision, err error) {
	if dec.LeaseID == "" && dec.Ticket == "" {
		return
	}
	release := dec.LeaseID != "" && check.Labelled && d.release != nil
	d.log.Info("reply lost", "lease", dec.LeaseID, "released", release, "ticket", dec.Ticket, "err", err)
	if release {
		d.release(dec.LeaseID)
	}
	if dec.Ticket != "" && d.drop != nil {
		d.drop(dec.Ticket)
	}
}

// answer replies to the request read from c; for a check, it sets *check
// to the request.
func (d *Daemon) answer(c net.Conn, check **protocol.CheckRequest) protocol.Reply {
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
		*check = req.Check
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
