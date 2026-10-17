package main

import (
	"encoding/base64"
	"log"
	"net"
	"sync"
	"time"
)

// Bedrock players (via Geyser on the host) speak RakNet over UDP. When a host
// registers with udp:true, the relay also listens for UDP on the tunnel's
// public port and bridges each remote address as a "session" over the same
// control WebSocket: udp{conn, ip?, data} frames in both directions, ip only
// on a session's first datagram, udp_close when either side gives up.

const (
	maxUDPSessionsPerTunnel = 256
	maxUDPSessionsPerIP     = 8
	maxUDPDatagram          = 2048 // RakNet MTU tops out around 1500
	udpSessionIdle          = 60 * time.Second
	udpSweepTick            = 15 * time.Second

	// Until a session has sent udpTrustPackets datagrams, the host may send
	// it at most udpPreTrustRatio×(bytes received)+udpPreTrustAllowance — so a
	// spoofed source address can't turn this relay into a traffic amplifier,
	// while a real client's handshake (a small ping answered by a pong carrying
	// the MOTD) still fits comfortably.
	udpTrustPackets      = 4
	udpPreTrustRatio     = 4
	udpPreTrustAllowance = 600
)

type udpSession struct {
	id       string
	addr     *net.UDPAddr
	host     string
	lastSeen time.Time
	inPkts   int
	inBytes  int64
	outBytes int64
}

type udpState struct {
	conn *net.UDPConn
	done chan struct{}
	once sync.Once

	mu     sync.Mutex
	byAddr map[string]*udpSession
	byID   map[string]*udpSession
	perIP  map[string]int // limitKey(player address) -> live sessions
}

func (u *udpState) close() {
	u.once.Do(func() {
		close(u.done)
		u.conn.Close()
	})
}

func (u *udpState) remove(s *udpSession) {
	if u.byID[s.id] != s {
		return
	}
	delete(u.byID, s.id)
	delete(u.byAddr, s.addr.String())
	key := limitKey(s.host)
	if u.perIP[key]--; u.perIP[key] <= 0 {
		delete(u.perIP, key)
	}
}

// startUDP opens the tunnel's UDP listener on its public port.
func (c *client) startUDP() error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: c.publicPort})
	if err != nil {
		return err
	}
	c.udp = &udpState{
		conn:   conn,
		done:   make(chan struct{}),
		byAddr: map[string]*udpSession{},
		byID:   map[string]*udpSession{},
		perIP:  map[string]int{},
	}
	go c.udpReadLoop()
	go c.udpSweepLoop()
	return nil
}

func (c *client) udpReadLoop() {
	u := c.udp
	buf := make([]byte, maxUDPDatagram)
	for {
		n, addr, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-u.done:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			log.Printf("[udp] port %d read: %v", c.publicPort, err)
			return
		}
		host := addr.IP.String()

		c.mu.Lock()
		blocked := c.blocked[host]
		c.mu.Unlock()
		if blocked {
			continue
		}

		u.mu.Lock()
		s := u.byAddr[addr.String()]
		isNew := false
		if s == nil {
			if len(u.byID) >= maxUDPSessionsPerTunnel || u.perIP[limitKey(host)] >= maxUDPSessionsPerIP {
				u.mu.Unlock()
				continue
			}
			id, err := newID()
			if err != nil {
				u.mu.Unlock()
				continue
			}
			s = &udpSession{id: id, addr: addr, host: host}
			u.byAddr[addr.String()] = s
			u.byID[id] = s
			u.perIP[limitKey(host)]++
			isNew = true
		}
		s.lastSeen = time.Now()
		s.inPkts++
		s.inBytes += int64(n)
		id := s.id
		u.mu.Unlock()

		c.bytesIn.Add(int64(n))
		m := Msg{Type: "udp", Conn: id, Data: base64.StdEncoding.EncodeToString(buf[:n])}
		if isNew {
			m.IP = host
		}
		c.send(m)
	}
}

// handleHostUDP sends a host's datagram back to the session's player.
func (c *client) handleHostUDP(m Msg) {
	u := c.udp
	if u == nil {
		return
	}
	data, err := base64.StdEncoding.DecodeString(m.Data)
	if err != nil || len(data) == 0 || len(data) > maxUDPDatagram {
		return
	}
	u.mu.Lock()
	s := u.byID[m.Conn]
	if s == nil {
		u.mu.Unlock()
		return
	}
	if s.inPkts < udpTrustPackets && s.outBytes+int64(len(data)) > udpPreTrustRatio*s.inBytes+udpPreTrustAllowance {
		u.mu.Unlock()
		return
	}
	s.outBytes += int64(len(data))
	addr := s.addr
	u.mu.Unlock()

	if _, err := u.conn.WriteToUDP(data, addr); err == nil {
		c.bytesOut.Add(int64(len(data)))
	}
}

// closeUDPSession forgets a session; notifyHost tells the host it's gone.
func (c *client) closeUDPSession(id string, notifyHost bool) {
	u := c.udp
	if u == nil {
		return
	}
	u.mu.Lock()
	s := u.byID[id]
	if s != nil {
		u.remove(s)
	}
	u.mu.Unlock()
	if s != nil && notifyHost {
		c.send(Msg{Type: "udp_close", Conn: id})
	}
}

func (c *client) udpSweepLoop() {
	u := c.udp
	t := time.NewTicker(udpSweepTick)
	defer t.Stop()
	for {
		select {
		case <-u.done:
			return
		case <-t.C:
		}
		var idle []string
		u.mu.Lock()
		for id, s := range u.byID {
			if time.Since(s.lastSeen) > udpSessionIdle {
				idle = append(idle, id)
			}
		}
		u.mu.Unlock()
		for _, id := range idle {
			c.closeUDPSession(id, true)
		}
	}
}

// udpSessionCount is the number of live Bedrock sessions on this tunnel.
func (c *client) udpSessionCount() int {
	if c.udp == nil {
		return 0
	}
	c.udp.mu.Lock()
	defer c.udp.mu.Unlock()
	return len(c.udp.byID)
}
