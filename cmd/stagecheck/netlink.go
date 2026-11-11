package main

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// netLink 是放在 Gateway 和 Manager 之间的一段转发。它能停住（连接还在，但一个字节都不过，
// 对端收不到 RST，只能靠超时发现）、加延迟、或者把现有连接全部掐断。
type netLink struct {
	port   int
	target string
	ln     net.Listener
	mu     sync.Mutex
	cond   *sync.Cond
	stuck  bool
	delay  time.Duration
	conns  map[net.Conn]struct{}
	closed bool
}

func newLink(target string) (*netLink, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l := &netLink{port: ln.Addr().(*net.TCPAddr).Port, target: target, ln: ln, conns: map[net.Conn]struct{}{}}
	l.cond = sync.NewCond(&l.mu)
	go l.accept()
	return l, nil
}

func (l *netLink) accept() {
	for {
		in, err := l.ln.Accept()
		if err != nil {
			return
		}
		go l.serve(in)
	}
}

func (l *netLink) serve(in net.Conn) {
	// 停住期间新连接也能在 TCP 层连上（三次握手由内核完成），但没有任何数据往返。
	l.wait()
	out, err := net.DialTimeout("tcp", l.target, 3*time.Second)
	if err != nil {
		_ = in.Close()
		return
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = in.Close()
		_ = out.Close()
		return
	}
	l.conns[in] = struct{}{}
	l.conns[out] = struct{}{}
	l.mu.Unlock()
	done := func() {
		_ = in.Close()
		_ = out.Close()
		l.mu.Lock()
		delete(l.conns, in)
		delete(l.conns, out)
		l.mu.Unlock()
	}
	go func() {
		l.pump(in, out)
		done()
	}()
	l.pump(out, in)
	done()
}

func (l *netLink) wait() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.stuck && !l.closed {
		l.cond.Wait()
	}
	return l.delay
}

func (l *netLink) pump(src, dst net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if d := l.wait(); d > 0 {
				time.Sleep(d)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (l *netLink) stall() {
	l.mu.Lock()
	l.stuck = true
	l.mu.Unlock()
}

func (l *netLink) resume() {
	l.mu.Lock()
	l.stuck = false
	l.mu.Unlock()
	l.cond.Broadcast()
}

func (l *netLink) setDelay(d time.Duration) {
	l.mu.Lock()
	l.delay = d
	l.mu.Unlock()
}

// reset 掐断所有现有连接，两端都会收到断开。
func (l *netLink) reset() {
	l.mu.Lock()
	for c := range l.conns {
		_ = c.Close()
	}
	l.mu.Unlock()
}

func (l *netLink) close() {
	l.mu.Lock()
	l.closed = true
	l.stuck = false
	for c := range l.conns {
		_ = c.Close()
	}
	l.mu.Unlock()
	l.cond.Broadcast()
	_ = l.ln.Close()
}

// addManagerBehindLinks 拉起一台 Manager，并在它前面放两段转发（TCP 口和 gRPC 口各一段）。
func (c *cluster) addManagerBehindLinks(name string) (*mgr, error) {
	m, err := c.addManager(name)
	if err != nil {
		return nil, err
	}
	if m.viaTCP, err = newLink(fmt.Sprintf("127.0.0.1:%d", m.tcp)); err != nil {
		return nil, err
	}
	if m.viaGRPC, err = newLink(fmt.Sprintf("127.0.0.1:%d", m.grpc)); err != nil {
		return nil, err
	}
	c.links = append(c.links, m.viaTCP, m.viaGRPC)
	return m, nil
}

func (m *mgr) partition() {
	m.viaTCP.stall()
	m.viaGRPC.stall()
}

func (m *mgr) heal() {
	m.viaTCP.resume()
	m.viaGRPC.resume()
}
