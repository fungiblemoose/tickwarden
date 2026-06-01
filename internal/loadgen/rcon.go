// Package loadgen drives a reproducible, controlled load against a Minecraft
// server so that before/after `tickwarden bench` runs measure the SAME work.
// A benchmark is only trustworthy if the load is identical across runs; an
// idle server or an ad-hoc player wandering around is not reproducible. We use
// the Chunky pregenerator over Source RCON because chunk GENERATION is the
// single heaviest, most deterministic server-tick workload we can summon on
// demand without a human in the loop.
//
// This file implements the Source RCON protocol from scratch (stdlib only) so
// tickwarden takes no third-party dependencies.
package loadgen

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// Source RCON packet types. The same numeric value (2) is reused for both the
// command request (SERVERDATA_EXECCOMMAND) and the auth success reply
// (SERVERDATA_AUTH_RESPONSE) — that ambiguity is intrinsic to the protocol, so
// we disambiguate by context (we only expect an auth response right after an
// auth request).
const (
	typeResponseValue = 0 // SERVERDATA_RESPONSE_VALUE — body of a command reply
	typeExecCommand   = 2 // SERVERDATA_EXECCOMMAND (request) / AUTH_RESPONSE (reply)
	typeAuth          = 3 // SERVERDATA_AUTH — login request
)

// authFailedID is the RequestID the server returns in the auth response when
// the password was rejected. A successful auth echoes our own request id.
const authFailedID int32 = -1

// maxPacketSize caps how large a packet body we will allocate when reading,
// guarding against a malformed/hostile length prefix from forcing a huge
// allocation. The Source protocol caps responses near 4 KiB; we allow generous
// headroom for multi-packet implementations.
const maxPacketSize = 4096 + 16

// dialTimeout is the TCP dialer used by Dial. It is a package var solely so
// tests can substitute a net.Pipe and exercise the real Dial (handshake, auth
// decision, cleanup) without opening a socket. Production code never reassigns
// it.
var dialTimeout = net.DialTimeout

// Client is a connected, authenticated Source RCON session. It is NOT safe for
// concurrent use: RCON multiplexes requests/replies on a single TCP stream by
// matching RequestIDs, and interleaving callers would scramble that pairing.
type Client struct {
	conn    net.Conn
	timeout time.Duration
	reqID   int32 // monotonically incremented per request so replies can be matched
}

// Dial connects to addr (host:port, conventionally :25575), authenticates with
// password, and returns a ready Client. timeout bounds both the initial dial
// and every subsequent read/write, so a wedged server can never hang a bench
// run indefinitely. The connection is closed on any auth failure so callers
// never hold a half-open socket.
func Dial(addr, password string, timeout time.Duration) (*Client, error) {
	conn, err := dialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("rcon dial %s: %w", addr, err)
	}
	c := &Client{conn: conn, timeout: timeout, reqID: 0}

	id := c.nextID()
	if err := c.writePacket(id, typeAuth, password); err != nil {
		conn.Close()
		return nil, fmt.Errorf("rcon auth write: %w", err)
	}

	// On success the server replies with a type-2 (AUTH_RESPONSE) packet whose
	// RequestID equals ours; on failure RequestID is -1. Some servers first
	// emit an empty type-0 RESPONSE_VALUE before the auth response, so we skip
	// a leading type-0 if we see one.
	resp, err := c.readPacket()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("rcon auth read: %w", err)
	}
	if resp.Type == typeResponseValue {
		resp, err = c.readPacket()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("rcon auth read: %w", err)
		}
	}
	if resp.RequestID == authFailedID || resp.RequestID != id {
		conn.Close()
		return nil, errors.New("rcon auth failed: bad password")
	}
	return c, nil
}

// Command sends a single console command and returns the server's response
// body (stripped of the trailing NUL terminators). It matches the reply by
// RequestID to avoid acting on a stale packet.
func (c *Client) Command(cmd string) (string, error) {
	id := c.nextID()
	if err := c.writePacket(id, typeExecCommand, cmd); err != nil {
		return "", fmt.Errorf("rcon command write %q: %w", cmd, err)
	}
	resp, err := c.readPacket()
	if err != nil {
		return "", fmt.Errorf("rcon command read %q: %w", cmd, err)
	}
	if resp.RequestID != id {
		return "", fmt.Errorf("rcon command %q: response id %d != request id %d", cmd, resp.RequestID, id)
	}
	return resp.Body, nil
}

// Close terminates the RCON session and underlying TCP connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// nextID returns a fresh, positive request id. It avoids -1 because that value
// is reserved by the protocol to signal auth failure.
func (c *Client) nextID() int32 {
	c.reqID++
	if c.reqID < 0 {
		c.reqID = 1
	}
	return c.reqID
}

// packet is a decoded RCON frame (the RequestID/Type/Body; the length prefix is
// implicit and recomputed on encode).
type packet struct {
	RequestID int32
	Type      int32
	Body      string
}

// encodePacket builds the wire form of an RCON packet:
//
//	int32 Length   — size of everything AFTER this field
//	int32 RequestID
//	int32 Type
//	[]byte Body    — ASCII, NUL-terminated
//	byte 0x00      — second/empty-string terminator required by the protocol
//
// All integers are little-endian. The trailing TWO NUL bytes are mandatory:
// one terminates the body string, the second is the protocol's empty trailing
// string.
func encodePacket(id, typ int32, body string) []byte {
	// Length counts: RequestID(4) + Type(4) + body + two trailing NULs.
	length := int32(4 + 4 + len(body) + 2)
	buf := bytes.NewBuffer(make([]byte, 0, 4+length))
	// binary.Write to a bytes.Buffer cannot fail, so errors are ignored.
	binary.Write(buf, binary.LittleEndian, length)
	binary.Write(buf, binary.LittleEndian, id)
	binary.Write(buf, binary.LittleEndian, typ)
	buf.WriteString(body)
	buf.WriteByte(0)
	buf.WriteByte(0)
	return buf.Bytes()
}

// decodePayload parses the bytes AFTER the length prefix (RequestID, Type,
// body, two NULs) into a packet. It validates the two trailing NUL bytes so a
// truncated/garbled frame is rejected rather than silently accepted.
func decodePayload(payload []byte) (packet, error) {
	if len(payload) < 10 { // 4 id + 4 type + 2 NUL
		return packet{}, fmt.Errorf("rcon payload too short: %d bytes", len(payload))
	}
	var p packet
	p.RequestID = int32(binary.LittleEndian.Uint32(payload[0:4]))
	p.Type = int32(binary.LittleEndian.Uint32(payload[4:8]))
	body := payload[8:]
	if body[len(body)-1] != 0 || body[len(body)-2] != 0 {
		return packet{}, errors.New("rcon payload missing trailing NUL bytes")
	}
	p.Body = string(body[:len(body)-2])
	return p, nil
}

// writePacket encodes and writes a frame under the client's deadline.
func (c *Client) writePacket(id, typ int32, body string) error {
	if c.timeout > 0 {
		c.conn.SetWriteDeadline(time.Now().Add(c.timeout))
	}
	_, err := c.conn.Write(encodePacket(id, typ, body))
	return err
}

// readPacket reads exactly one frame: the int32 length prefix, then that many
// bytes of payload, then decodes it.
func (c *Client) readPacket() (packet, error) {
	if c.timeout > 0 {
		c.conn.SetReadDeadline(time.Now().Add(c.timeout))
	}
	var length int32
	if err := binary.Read(c.conn, binary.LittleEndian, &length); err != nil {
		return packet{}, err
	}
	if length < 10 || length > maxPacketSize {
		return packet{}, fmt.Errorf("rcon: implausible packet length %d", length)
	}
	payload := make([]byte, length)
	if _, err := readFull(c.conn, payload); err != nil {
		return packet{}, err
	}
	return decodePayload(payload)
}

// readFull fills buf completely, looping over short reads. We implement it
// here rather than importing io to keep the dependency surface minimal and
// explicit for this protocol-critical path.
func readFull(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := conn.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
