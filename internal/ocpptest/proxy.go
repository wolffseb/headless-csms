package ocpptest

import (
	"bytes"
	"net"
	"sync"
	"testing"
)

// Proxy is a TCP relay between a station and a CSMS that a test can cut.
// That is how an outage looks to a station: the CSMS going away, or a proxy
// in front of it closing the connection on purpose. The connection drops,
// and redials fail until Restore.
type Proxy struct {
	ln     net.Listener
	target string

	mu       sync.Mutex
	refusing bool
	holding  bool
	conns    []net.Conn
	// fromCSMS is everything relayed towards the station, for Saw.
	fromCSMS bytes.Buffer
	wg       sync.WaitGroup
}

// NewProxy relays to target, a host:port, until the test ends.
func NewProxy(t *testing.T, target string) *Proxy {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &Proxy{ln: ln, target: target}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.Cut()
		p.wg.Wait()
	})
	return p
}

// URL is the WebSocket URL to give the station.
func (p *Proxy) URL() string { return "ws://" + p.ln.Addr().String() }

func (p *Proxy) serve() {
	defer p.wg.Done()
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.refusing {
			p.mu.Unlock()
			_ = down.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			p.mu.Unlock()
			_ = down.Close()
			continue
		}
		p.conns = append(p.conns, down, up)
		p.wg.Add(2)
		p.mu.Unlock()

		go p.relay(up, down, false)
		go p.relay(down, up, true)
	}
}

func (p *Proxy) relay(dst, src net.Conn, fromCSMS bool) {
	defer p.wg.Done()
	defer func() {
		_ = dst.Close()
		_ = src.Close()
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			p.mu.Lock()
			drop := fromCSMS && p.holding
			if fromCSMS && !drop {
				p.fromCSMS.Write(buf[:n])
			}
			p.mu.Unlock()
			if !drop {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// Cut drops every relayed connection and refuses new ones until Restore.
func (p *Proxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refusing = true
	p.holding = false
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// Restore accepts and relays connections again.
func (p *Proxy) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refusing = false
}

// Hold stops relaying what the CSMS sends, and drops it, while the station's
// side still gets through: a one-way partition, so that a CSMS command can
// be kept from the station while the connection itself looks fine. Cut ends
// it.
func (p *Proxy) Hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holding = true
}

// Saw reports whether anything relayed to the station so far contained s.
func (p *Proxy) Saw(s string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return bytes.Contains(p.fromCSMS.Bytes(), []byte(s))
}
