package proxy

import (
	"io"
	"net"
	"sync"
)

// Relay copies bytes bidirectionally between client and upstream until EOF in
// both directions, then closes both. CloseWrite is called on each side as soon
// as the corresponding direction finishes (so upstream sees the client's EOF
// promptly, and vice versa). Used for streaming endpoints and large-body
// passthroughs (§8, §12).
func Relay(client, upstream net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
	}()
	wg.Wait()
	_ = client.Close()
	_ = upstream.Close()
}

// closeWrite calls CloseWrite if the conn supports half-close. TCP and Unix
// sockets do; otherwise this is a no-op so the relay still terminates cleanly
// when the goroutine returns.
func closeWrite(c net.Conn) {
	type halfCloser interface {
		CloseWrite() error
	}
	if hc, ok := c.(halfCloser); ok {
		_ = hc.CloseWrite()
	}
}
