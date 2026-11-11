// Package ws is a minimal WebSocket server (RFC 6455): enough to carry JSON
// text messages for the realtime API, written against the standard library
// so the daemon keeps having no dependencies.
//
// It supports text and binary messages, fragmentation, ping/pong and the
// closing handshake. It does not support extensions such as compression.
package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Opcodes of data messages.
const (
	Text   = 1
	Binary = 2
)

const (
	opContinuation = 0
	opClose        = 8
	opPing         = 9
	opPong         = 10

	magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// MaxMessage bounds a single message. A realtime client appends audio in
// small pieces; nothing legitimate comes close to this.
const MaxMessage = 16 << 20

// ErrClosed is returned by ReadMessage after the peer closed the connection
// in an orderly way.
var ErrClosed = errors.New("websocket closed")

// Conn is a server-side WebSocket connection.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu    sync.Mutex
	closed bool
}

// Accept upgrades an HTTP request to a WebSocket connection. On failure it
// has already written an HTTP error response.
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !headerHas(r.Header, "Connection", "upgrade") || !headerHas(r.Header, "Upgrade", "websocket") {
		http.Error(w, "this endpoint speaks WebSocket; connect with ws://", http.StatusUpgradeRequired)
		return nil, errors.New("not a websocket handshake")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported WebSocket version", http.StatusBadRequest)
		return nil, errors.New("unsupported websocket version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("missing websocket key")
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "cannot take over the connection", http.StatusInternalServerError)
		return nil, err
	}
	sum := sha1.Sum([]byte(key + magic))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n"
	// Clients may offer subprotocols (the OpenAI SDK offers "realtime");
	// echo the first so strict clients accept the handshake.
	if p := r.Header.Get("Sec-WebSocket-Protocol"); p != "" {
		first, _, _ := strings.Cut(p, ",")
		resp += "Sec-WebSocket-Protocol: " + strings.TrimSpace(first) + "\r\n"
	}
	conn.SetDeadline(time.Time{})
	if _, err := rw.WriteString(resp + "\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &Conn{conn: conn, br: rw.Reader}, nil
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// ReadMessage returns the next text or binary message. Pings are answered
// and pongs ignored along the way. It returns ErrClosed after a close frame.
func (c *Conn) ReadMessage() (opcode int, payload []byte, err error) {
	var msg []byte
	msgOp := 0
	for {
		fin, op, data, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(opPong, data); err != nil {
				return 0, nil, err
			}
		case opPong:
		case opClose:
			c.writeFrame(opClose, data) // echo, completing the handshake
			return 0, nil, ErrClosed
		case Text, Binary:
			if msgOp != 0 {
				return 0, nil, c.fail(1002, "new message before the previous one finished")
			}
			msgOp, msg = op, data
			if fin {
				return msgOp, msg, nil
			}
		case opContinuation:
			if msgOp == 0 {
				return 0, nil, c.fail(1002, "continuation without a message")
			}
			if len(msg)+len(data) > MaxMessage {
				return 0, nil, c.fail(1009, "message too large")
			}
			msg = append(msg, data...)
			if fin {
				return msgOp, msg, nil
			}
		default:
			return 0, nil, c.fail(1002, "unknown opcode")
		}
	}
}

func (c *Conn) readFrame() (fin bool, opcode int, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return
	}
	fin = head[0]&0x80 != 0
	if head[0]&0x70 != 0 {
		err = c.fail(1002, "reserved bits set; extensions are not supported")
		return
	}
	opcode = int(head[0] & 0x0F)
	masked := head[1]&0x80 != 0
	n := uint64(head[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if !masked {
		err = c.fail(1002, "client frames must be masked")
		return
	}
	if opcode >= 8 && (n > 125 || !fin) {
		err = c.fail(1002, "invalid control frame")
		return
	}
	if n > MaxMessage {
		err = c.fail(1009, "message too large")
		return
	}
	var key [4]byte
	if _, err = io.ReadFull(c.br, key[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= key[i&3]
	}
	return
}

func (c *Conn) writeFrame(opcode int, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return ErrClosed
	}
	head := []byte{0x80 | byte(opcode)}
	switch n := len(payload); {
	case n < 126:
		head = append(head, byte(n))
	case n < 1<<16:
		head = append(head, 126, byte(n>>8), byte(n))
	default:
		head = append(head, 127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.conn.Write(append(head, payload...)); err != nil {
		return err
	}
	if opcode == opClose {
		c.closed = true
	}
	return nil
}

// WriteText sends one text message. It is safe to call from several goroutines.
func (c *Conn) WriteText(data []byte) error { return c.writeFrame(Text, data) }

// fail sends a close frame with a status code and returns the matching error.
func (c *Conn) fail(code int, reason string) error {
	c.CloseWith(code, reason)
	return fmt.Errorf("websocket protocol error: %s", reason)
}

// CloseWith sends a close frame and closes the connection.
func (c *Conn) CloseWith(code int, reason string) {
	payload := binary.BigEndian.AppendUint16(nil, uint16(code))
	c.writeFrame(opClose, append(payload, reason...))
	c.conn.Close()
}

// Close closes the connection normally.
func (c *Conn) Close() { c.CloseWith(1000, "") }
