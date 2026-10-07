package client

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// replyWith serves one connection on a short socket path, answering line.
func replyWith(t *testing.T, line string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
		_, _ = io.WriteString(c, line+"\n")
	}()
	return path
}

func TestStatusRejectsOtherProtocolVersion(t *testing.T) {
	path := replyWith(t, `{"v":2,"ok":true,"snapshot":{"seq":9}}`)
	_, err := Status(context.Background(), path, time.Second)
	if err == nil || !strings.Contains(err.Error(), "protocol version 2") {
		t.Fatalf("err = %v, want a protocol version mismatch", err)
	}
}
