package main

import (
	"encoding/hex"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

// Protocol v2: binary data frames.
//
// In v1 every byte of player traffic is base64-encoded inside a JSON text frame, which costs about a third more
// bandwidth and CPU than the bytes themselves. A host that sends "bin": 1 in its register frame, and gets "bin": 1 back
// in the port frame, switches both directions to binary WebSocket messages for TCP and UDP payloads:
//
//	byte 0       kind: 0x01 TCP data, 0x02 UDP datagram
//	bytes 1–16   connection id: the 16 raw bytes of the 32-character hex id the relay hands out
//	bytes 17…    the payload
//
// Everything else stays a JSON text frame in both modes (connect, close, ping, pong, udp_close, names, listing, errors and
// notices), and a JSON "data" or "udp" frame is still understood from a bin host, so a partial client keeps working.
// A host that does not ask for it never sees a binary frame; an old relay never sets "bin" in its reply.
// A UDP datagram's first frame carries no source address in this mode (v1's JSON udp frame had an "ip" on a session's first
// datagram); nothing but the log ever used it.
const (
	frameTCP       byte = 0x01
	frameUDP       byte = 0x02
	frameHeaderLen      = 17

	// A malformed binary frame is logged at most this often per tunnel, so a broken or hostile host can't fill the log.
	badFrameLogEvery = 30 * time.Second
)

// rawID turns the relay's 32-character hex connection id into its 16 bytes.
func rawID(id string) ([16]byte, bool) {
	var raw [16]byte
	if len(id) != 32 {
		return raw, false
	}
	if _, err := hex.Decode(raw[:], []byte(id)); err != nil {
		return raw, false
	}
	return raw, true
}

// putFrameHeader writes the kind and connection id into the first frameHeaderLen bytes of dst.
func putFrameHeader(dst []byte, kind byte, id [16]byte) {
	dst[0] = kind
	copy(dst[1:frameHeaderLen], id[:])
}

// writeFrame writes one WebSocket message to the host. Writes are serialised and deadline-bounded; if one fails (or times
// out) the control channel is unusable and the whole tunnel is torn down, exactly as for a JSON frame.
func (c *client) writeFrame(messageType int, data []byte) {
	c.writeMu.Lock()
	c.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	err := c.ws.WriteMessage(messageType, data)
	c.writeMu.Unlock()
	if err != nil {
		c.endWith(endSendFailed)
		c.close(c.rel)
	}
}

// sendFramed sends a binary frame whose header is already in place at the start of frame (so a caller that read the
// payload straight into frame[frameHeaderLen:] needs no second copy).
func (c *client) sendFramed(frame []byte) {
	c.writeFrame(websocket.BinaryMessage, frame)
}

// sendBinary builds and sends one binary frame in a single allocation.
func (c *client) sendBinary(kind byte, id [16]byte, payload []byte) {
	frame := make([]byte, frameHeaderLen+len(payload))
	putFrameHeader(frame, kind, id)
	copy(frame[frameHeaderLen:], payload)
	c.sendFramed(frame)
}

// noteBadFrame logs a rejected binary frame, at most once per badFrameLogEvery for this tunnel.
func (c *client) noteBadFrame(reason string) {
	now := time.Now().UnixNano()
	last := c.badFrameAt.Load()
	if now-last < int64(badFrameLogEvery) || !c.badFrameAt.CompareAndSwap(last, now) {
		return
	}
	log.Printf("[relay] token %s…: ignored a binary frame (%s); more are not logged for %s", c.id[:8], reason, badFrameLogEvery)
}

// handleBinary is the host->relay path for binary frames: it checks the header and hands the payload to the same
// delivery code the JSON "data" and "udp" frames use, so the two paths can't drift apart.
func (r *relay) handleBinary(c *client, data []byte) {
	if !c.bin {
		c.noteBadFrame("this tunnel did not ask for binary frames")
		return
	}
	if len(data) < frameHeaderLen {
		c.noteBadFrame("shorter than its header")
		return
	}
	payload := data[frameHeaderLen:]
	id := hex.EncodeToString(data[1:frameHeaderLen])
	switch data[0] {
	case frameTCP:
		if len(payload) == 0 {
			return
		}
		if !c.deliverToPlayer(id, payload) {
			c.noteBadFrame("unknown connection")
		}
	case frameUDP:
		if len(payload) == 0 {
			return
		}
		if !c.deliverUDP(id, payload) {
			c.noteBadFrame("unknown session, or a datagram that is too big")
		}
	default:
		c.noteBadFrame("unknown kind")
	}
}
