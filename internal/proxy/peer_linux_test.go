//go:build linux

package proxy

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestGetPeerUIDSameProcess verifies that GetPeerUID returns os.Getuid() when
// the connection is between two parts of the same process. Linux mirror of
// the darwin test in §15.6; closes the v1 gap noted in §19.5.
func TestGetPeerUIDSameProcess(t *testing.T) {
	dir, err := mkShortTempDir("iso-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeAll(dir)
	sock := filepath.Join(dir, "p.sock")

	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	var serverConn net.Conn
	go func() {
		defer wg.Done()
		c, err := l.Accept()
		if err != nil {
			return
		}
		serverConn = c
	}()

	clientConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	wg.Wait()
	defer serverConn.Close()

	uid, ok := GetPeerUID(serverConn.(*net.UnixConn))
	if !ok {
		t.Fatal("GetPeerUID returned ok=false")
	}
	want := uint32(os.Getuid())
	if uid != want {
		t.Errorf("GetPeerUID = %d, want %d", uid, want)
	}
}

func TestGetPeerUIDViaSeparateGoroutine(t *testing.T) {
	dir, err := mkShortTempDir("iso-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeAll(dir)
	sock := filepath.Join(dir, "p.sock")

	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Errorf("dial: %v", err)
			return
		}
		_ = c
		time.Sleep(100 * time.Millisecond)
		_ = c.Close()
	}()

	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	uid, ok := GetPeerUID(c.(*net.UnixConn))
	if !ok {
		t.Fatal("GetPeerUID returned ok=false")
	}
	if uid != uint32(os.Getuid()) {
		t.Errorf("GetPeerUID = %d, want %d", uid, os.Getuid())
	}
	<-done
}
