package hostnet

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

type Status struct {
	Egress string `json:"egress"`
	Bypass string `json:"bypass,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type DialFunc func(context.Context, string) (net.Conn, error)

// Router retains the guarded VM NIC and changes only its host-side dialers.
// A generation change cancels pending dials and closes established flows.
type Router struct {
	Detect      func(context.Context) (Network, error)
	Rules       func() ([]Exception, error)
	Direct      func(context.Context, Network, string, string) (net.Conn, error)
	TCP, UDP    DialFunc
	refresh     sync.Mutex
	mu          sync.Mutex
	status      Status
	network     Network
	epoch       context.Context
	cancel      context.CancelFunc
	connections map[*trackedConn]struct{}
	closed      bool
}

func (r *Router) Refresh(ctx context.Context) Status {
	r.refresh.Lock()
	defer r.refresh.Unlock()
	s := Status{Egress: "vpn"}
	var n Network
	rules, err := r.Rules()
	if err != nil {
		s.Reason = "cannot read bypass rules"
	} else if len(rules) != 0 {
		n, err = r.Detect(ctx)
		if err != nil || !n.Valid() {
			n = Network{}
			s.Reason = "network could not be identified"
		} else if name := Match(n, rules); name != "" {
			s.Egress, s.Bypass = "direct", name
		} else {
			s.Reason = "network is not in bypass list"
		}
	} else {
		s.Reason = "no bypass networks configured"
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Status{Egress: "blocked", Reason: "gateway stopped"}
	}
	var old map[*trackedConn]struct{}
	if r.epoch == nil || r.network != n || r.status.Egress != s.Egress || r.status.Bypass != s.Bypass {
		if r.cancel != nil {
			r.cancel()
		}
		r.epoch, r.cancel = context.WithCancel(context.Background())
		old = r.connections
		r.connections = make(map[*trackedConn]struct{})
	}
	r.status, r.network = s, n
	r.mu.Unlock()
	for c := range old {
		_ = c.Close()
	}
	return s
}

func (r *Router) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Router) Watch(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Refresh(ctx)
		}
	}
}

func (r *Router) Dial(ctx context.Context, protocol, address string) (net.Conn, error) {
	if protocol != "tcp" && protocol != "udp" {
		return nil, errors.New("unsupported egress protocol")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// An individual request timing out must not invalidate every existing
	// connection. Detect has its own bounded timeout, independent of this dial.
	r.Refresh(context.WithoutCancel(ctx))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("gateway stopped")
	}
	epoch, n, mode := r.epoch, r.network, r.status.Egress
	r.mu.Unlock()
	// Make the network epoch the parent so a policy change synchronously
	// cancels the dial. Preserve the caller's deadline and cancellation too.
	var dialCtx context.Context
	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		dialCtx, cancel = context.WithDeadline(epoch, deadline)
	} else {
		dialCtx, cancel = context.WithCancel(epoch)
	}
	stop := context.AfterFunc(ctx, cancel)
	if ctx.Err() != nil {
		cancel()
	}
	defer func() { stop(); cancel() }()
	var conn net.Conn
	var err error
	if mode == "direct" {
		conn, err = r.Direct(dialCtx, n, protocol, address)
	} else if protocol == "tcp" {
		conn, err = r.TCP(dialCtx, address)
	} else {
		conn, err = r.UDP(dialCtx, address)
	}
	if err != nil {
		return nil, err
	} // Never retry via another policy.
	r.mu.Lock()
	if r.closed || epoch != r.epoch || epoch.Err() != nil || dialCtx.Err() != nil {
		r.mu.Unlock()
		conn.Close()
		return nil, errors.New("network changed during connection setup")
	}
	c := &trackedConn{Conn: conn, owner: r}
	r.connections[c] = struct{}{}
	r.mu.Unlock()
	return c, nil
}

func (r *Router) Close() {
	r.mu.Lock()
	r.closed = true
	if r.cancel != nil {
		r.cancel()
	}
	connections := r.connections
	r.connections = nil
	r.mu.Unlock()
	for c := range connections {
		_ = c.Close()
	}
}

type trackedConn struct {
	net.Conn
	owner *Router
	once  sync.Once
}

func (c *trackedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.connections, c)
		c.owner.mu.Unlock()
	})
	return err
}
