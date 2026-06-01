package loadgen

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// TestEncodeDecodeRoundTrip table-tests the wire codec directly: encode a
// frame, strip the length prefix, decode the payload, and verify every field
// plus the mandatory two trailing NUL bytes survive intact.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		id   int32
		typ  int32
		body string
	}{
		{"auth", 7, typeAuth, "hunter2"},
		{"empty body", 1, typeExecCommand, ""},
		{"response", 42, typeResponseValue, "Chunky task started"},
		{"negative id", authFailedID, typeExecCommand, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := encodePacket(tc.id, tc.typ, tc.body)

			// Length prefix must equal the size of the remainder.
			gotLen := int32(binary.LittleEndian.Uint32(raw[0:4]))
			wantLen := int32(4 + 4 + len(tc.body) + 2)
			if gotLen != wantLen {
				t.Fatalf("length = %d, want %d", gotLen, wantLen)
			}
			if int(gotLen) != len(raw)-4 {
				t.Fatalf("length %d != bytes after prefix %d", gotLen, len(raw)-4)
			}
			// Last two bytes must be NUL.
			if raw[len(raw)-1] != 0 || raw[len(raw)-2] != 0 {
				t.Fatalf("missing trailing NUL bytes: % x", raw[len(raw)-2:])
			}

			p, err := decodePayload(raw[4:])
			if err != nil {
				t.Fatalf("decodePayload: %v", err)
			}
			if p.RequestID != tc.id || p.Type != tc.typ || p.Body != tc.body {
				t.Fatalf("round trip = {%d,%d,%q}, want {%d,%d,%q}",
					p.RequestID, p.Type, p.Body, tc.id, tc.typ, tc.body)
			}
		})
	}
}

func TestDecodePayloadRejectsGarbage(t *testing.T) {
	if _, err := decodePayload([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for too-short payload")
	}
	// Valid length but no trailing NULs (body bytes are 'A','B').
	bad := encodePacket(1, typeResponseValue, "AB")
	bad[len(bad)-1] = 'X'
	if _, err := decodePayload(bad[4:]); err == nil {
		t.Fatal("expected error for missing trailing NUL")
	}
}

// fakeReadPacket reads one length-prefixed RCON frame off conn. It returns an
// error (rather than failing the test) so the server loop can exit quietly
// when the client closes the pipe — a closed connection is normal teardown,
// not a test failure, and a t.Fatalf from this background goroutine would race
// the completing test.
func fakeReadPacket(conn net.Conn) (packet, error) {
	var length int32
	if err := binary.Read(conn, binary.LittleEndian, &length); err != nil {
		return packet{}, err
	}
	payload := make([]byte, length)
	if _, err := readFull(conn, payload); err != nil {
		return packet{}, err
	}
	return decodePayload(payload)
}

// runFakeRCON answers an auth handshake then echoes each command as a type-0
// response. authID is what it echoes for the auth reply: pass the request id
// for success, or authFailedID (-1) to simulate a bad password.
func runFakeRCON(t *testing.T, conn net.Conn, authSucceeds bool) {
	defer conn.Close()
	auth, err := fakeReadPacket(conn)
	if err != nil {
		return
	}
	if auth.Type != typeAuth {
		t.Errorf("fake server: first packet type = %d, want auth(%d)", auth.Type, typeAuth)
	}
	replyID := auth.RequestID
	if !authSucceeds {
		replyID = authFailedID
	}
	// AUTH_RESPONSE is type 2.
	if _, err := conn.Write(encodePacket(replyID, typeExecCommand, "")); err != nil {
		return
	}
	if !authSucceeds {
		return
	}
	// Echo every subsequent command as a RESPONSE_VALUE carrying the body.
	// A read error (e.g. the client closed the pipe) ends the loop cleanly.
	for {
		cmd, err := fakeReadPacket(conn)
		if err != nil {
			return
		}
		body := "echo: " + cmd.Body
		if _, err := conn.Write(encodePacket(cmd.RequestID, typeResponseValue, body)); err != nil {
			return
		}
	}
}

// withFakeServer overrides the dialTimeout seam so the REAL Dial runs over a
// net.Pipe wired to runFakeRCON. It returns a restore func. The fake goroutine
// is started before Dial reads, so the request/reply handshake never deadlocks
// on the synchronous pipe.
func withFakeServer(t *testing.T, authSucceeds bool) func() {
	t.Helper()
	orig := dialTimeout
	dialTimeout = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		go runFakeRCON(t, serverConn, authSucceeds)
		return clientConn, nil
	}
	return func() { dialTimeout = orig }
}

// TestDialCommandClose drives the real Dial -> Command -> Close path end to end
// over net.Pipe, asserting the command body round-trips and Close succeeds.
func TestDialCommandClose(t *testing.T) {
	defer withFakeServer(t, true)()

	c, err := Dial("fake:25575", "pw", 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	out, err := c.Command("list")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if out != "echo: list" {
		t.Fatalf("Command body = %q, want %q", out, "echo: list")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDialFailedAuth confirms a -1 (authFailedID) reply makes the real Dial
// return an error rather than a usable client.
func TestDialFailedAuth(t *testing.T) {
	defer withFakeServer(t, false)()

	c, err := Dial("fake:25575", "wrong", time.Second)
	if err == nil {
		c.Close()
		t.Fatal("Dial succeeded with a rejected password; want auth error")
	}
}
