package ws

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// echoServer answers every message with the same bytes.
func echoServer(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			c.WriteText(data)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func dial(t *testing.T, ts *httptest.Server, extra string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n%s\r\n", extra)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn, br, resp
}

func frame(fin bool, opcode byte, payload []byte, masked bool) []byte {
	b := []byte{opcode}
	if fin {
		b[0] |= 0x80
	}
	mask := byte(0)
	if masked {
		mask = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		b = append(b, mask|byte(n))
	case n < 1<<16:
		b = append(b, mask|126, byte(n>>8), byte(n))
	default:
		b = append(b, mask|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	if !masked {
		return append(b, payload...)
	}
	key := []byte{1, 2, 3, 4}
	b = append(b, key...)
	for i, p := range payload {
		b = append(b, p^key[i&3])
	}
	return b
}

func read(t *testing.T, br *bufio.Reader) (opcode byte, payload []byte) {
	t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	n := uint64(head[1] & 0x7F)
	if n == 126 {
		var b [2]byte
		io.ReadFull(br, b[:])
		n = uint64(binary.BigEndian.Uint16(b[:]))
	} else if n == 127 {
		var b [8]byte
		io.ReadFull(br, b[:])
		n = binary.BigEndian.Uint64(b[:])
	}
	payload = make([]byte, n)
	io.ReadFull(br, payload)
	return head[0] & 0x0F, payload
}

func TestHandshake(t *testing.T) {
	ts := echoServer(t)
	// The accept value for this key is the example in RFC 6455 section 1.3.
	_, _, resp := dial(t, ts, "Sec-WebSocket-Protocol: realtime, other\r\n")
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("handshake: %d %v", resp.StatusCode, resp.Header)
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "realtime" {
		t.Errorf("subprotocol %q, want the first one offered", got)
	}
	plain, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	plain.Body.Close()
	if plain.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("plain request: %d, want 426", plain.StatusCode)
	}
}

func TestMessages(t *testing.T) {
	ts := echoServer(t)
	conn, br, _ := dial(t, ts, "")

	// Each payload length encoding: 7-bit, 16-bit, 64-bit.
	for _, size := range []int{5, 300, 70000} {
		msg := bytes.Repeat([]byte("x"), size)
		conn.Write(frame(true, Text, msg, true))
		if op, got := read(t, br); op != Text || !bytes.Equal(got, msg) {
			t.Errorf("echo of %d bytes: opcode %d, %d bytes", size, op, len(got))
		}
	}
	// A message split across frames, with a ping in the middle, arrives whole,
	// and the ping is answered with a pong carrying the same payload.
	conn.Write(frame(false, Text, []byte("hel"), true))
	conn.Write(frame(true, opPing, []byte("p"), true))
	conn.Write(frame(false, opContinuation, []byte("lo "), true))
	conn.Write(frame(true, opContinuation, []byte("world"), true))
	if op, got := read(t, br); op != opPong || string(got) != "p" {
		t.Errorf("ping answered with opcode %d %q", op, got)
	}
	if op, got := read(t, br); op != Text || string(got) != "hello world" {
		t.Errorf("fragmented message: opcode %d %q", op, got)
	}
	// Closing is acknowledged with a close frame.
	conn.Write(frame(true, opClose, []byte{0x03, 0xE8}, true))
	if op, got := read(t, br); op != opClose || len(got) < 2 || binary.BigEndian.Uint16(got) != 1000 {
		t.Errorf("close answered with opcode %d %v", op, got)
	}
}

func TestProtocolViolations(t *testing.T) {
	cases := map[string][]byte{
		"unmasked client frame":    frame(true, Text, []byte("x"), false),
		"continuation out of turn": frame(true, opContinuation, []byte("x"), true),
		"fragmented control frame": frame(false, opPing, []byte("x"), true),
		"oversized message":        {0x82, 0x80 | 127, 0, 0, 0, 0, 0x7F, 0xFF, 0xFF, 0xFF, 1, 2, 3, 4},
		"reserved bit set":         {0xC1, 0x80, 1, 2, 3, 4},
	}
	for name, bad := range cases {
		ts := echoServer(t)
		conn, br, _ := dial(t, ts, "")
		conn.Write(bad)
		op, payload := read(t, br)
		if op != opClose || len(payload) < 2 {
			t.Errorf("%s: answered with opcode %d, want a close frame", name, op)
			continue
		}
		if code := binary.BigEndian.Uint16(payload); code != 1002 && code != 1009 {
			t.Errorf("%s: close code %d", name, code)
		}
	}
}
