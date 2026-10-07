// Package client talks to the headroom daemon over its Unix socket.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// Do sends one request and returns the reply. timeout bounds the whole
// exchange, dial included (R7: the shim fails open after 500 ms).
func Do(ctx context.Context, socket string, timeout time.Duration, req protocol.Request) (protocol.Reply, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var dialer net.Dialer
	c, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return protocol.Reply{}, err
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	req.V = protocol.Version
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return protocol.Reply{}, err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), protocol.MaxLine)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return protocol.Reply{}, err
		}
		return protocol.Reply{}, errors.New("daemon closed the connection without replying")
	}
	var rep protocol.Reply
	if err := json.Unmarshal(sc.Bytes(), &rep); err != nil {
		return protocol.Reply{}, fmt.Errorf("bad reply: %w", err)
	}
	if rep.V != protocol.Version {
		return protocol.Reply{}, fmt.Errorf("daemon speaks protocol version %d, this client speaks %d; restart the daemon after upgrading", rep.V, protocol.Version)
	}
	if !rep.OK {
		return rep, fmt.Errorf("daemon: %s", rep.Error)
	}
	return rep, nil
}

// Status returns the daemon's current snapshot.
func Status(ctx context.Context, socket string, timeout time.Duration) (*protocol.Snapshot, error) {
	rep, err := Do(ctx, socket, timeout, protocol.Request{Op: protocol.OpStatus})
	if err != nil {
		return nil, err
	}
	if rep.Snapshot == nil {
		return nil, errors.New("daemon: status reply has no snapshot")
	}
	return rep.Snapshot, nil
}

// Ping checks that a daemon answers on socket and returns its process ID.
func Ping(ctx context.Context, socket string, timeout time.Duration) (int, error) {
	rep, err := Do(ctx, socket, timeout, protocol.Request{Op: protocol.OpPing})
	if err != nil {
		return 0, err
	}
	return rep.PID, nil
}
