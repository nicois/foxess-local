package sim

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // G505: the RFC 6455 handshake mandates SHA-1.
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RFC 6455 wire constants. The server side is the minimal subset the
// integration's aiohttp client needs: text frames (getdata + pushes),
// ping/pong, and close.
const (
	wsOpText  = 0x1
	wsOpClose = 0x8
	wsOpPing  = 0x9
	wsOpPong  = 0xA

	wsFinBit   = 0x80
	wsMaskBit  = 0x80
	wsLen16    = 126
	wsLen64    = 127
	wsShortMax = 125
	wsMaskKey  = 4

	wsHdrSize   = 2  // first frame header row (also the 16-bit length extension)
	wsHdrCap    = 10 // header capacity (64-bit length + 4 mask slots worst case)
	wsLen64Size = 8  // 64-bit length extension
	wsLenMask   = 0x7F
	wsByteShift = 8 // bits per byte

	// wsMaxPayload bounds one client frame (the getdata text is 8 bytes).
	wsMaxPayload = 64 * 1024

	// wsGUID is the RFC 6455 protocol constant (the accept-key salt).
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	wsGetdata = "getdata"
)

var (
	errwsNotUpgrade = errors.New("sim ws: not a websocket upgrade")
	errwsNoKey      = errors.New("sim ws: missing Sec-WebSocket-Key")
	errwsHijack     = errors.New("sim ws: response writer does not support hijacking")
	errwsFragmented = errors.New("sim ws: fragmented frames are not supported")
	errwsUnmasked   = errors.New("sim ws: client frames must be masked")
	errwsTooBig     = errors.New("sim ws: frame payload exceeds the limit")
)

// wsStaleKeepalive is the no-data keepalive the non-newest clients get on
// a tick (the reference-simulator stale_msg shape; the client skips it on
// timeDiff > 30).
var wsStaleKeepalive = []byte(`{"errno":0,"result":{"timeDiff":999}}`)

// wsClient is one live wsmaitian connection.
type wsClient struct {
	srv  *Server
	conn net.Conn
	mu   sync.Mutex
}

// send writes one encoded frame (the writer is serialized per client).
func (c *wsClient) send(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.conn.Write(b)
	return err
}

// sendText writes one unfragmented text frame.
func (c *wsClient) sendText(b []byte) error {
	return c.send(wsEncodeFrame(wsOpText, b))
}

// serve reads client frames until the connection closes: getdata answers
// with the real message, pings are answered, close is echoed.
func (c *wsClient) serve() {
	br := bufio.NewReader(c.conn)
	for {
		op, payload, err := wsDecodeFrame(br)
		if err != nil {
			return
		}
		switch op {
		case wsOpText:
			if string(payload) == wsGetdata {
				_ = c.srv.pushReal(c)
			}
		case wsOpPing:
			_ = c.send(wsEncodeFrame(wsOpPong, payload))
		case wsOpClose:
			_ = c.send(wsEncodeFrame(wsOpClose, nil))
			return
		case wsOpPong:
			// No server pings; ignore the client's answers.
		}
	}
}

// wsEncodeFrame encodes one unfragmented server frame (unmasked).
func wsEncodeFrame(op byte, payload []byte) []byte {
	hdr := make([]byte, wsHdrSize, wsHdrCap)
	hdr[0] = wsFinBit | op
	n := len(payload)
	switch {
	case n <= wsShortMax:
		hdr[1] = byte(n)
	case n <= 0xFFFF:
		hdr[1] = wsLen16
		//nolint:gosec // G115: n ≤ 0xFFFF in this branch, each byte fits.
		hdr = append(hdr, byte(n>>wsByteShift), byte(n))
	default:
		hdr[1] = wsLen64
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, b[:]...)
	}
	return append(hdr, payload...)
}

// wsDecodeFrame reads one client frame (masked per RFC 6455) from r.
func wsDecodeFrame(r io.Reader) (byte, []byte, error) {
	hdr := make([]byte, wsHdrSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	if hdr[0]&wsFinBit == 0 {
		return 0, nil, errwsFragmented
	}
	if hdr[1]&wsMaskBit == 0 {
		return 0, nil, errwsUnmasked
	}
	op := hdr[0] & 0x0F
	n := int(hdr[1] & wsLenMask)
	switch n {
	case wsLen16:
		e := make([]byte, wsHdrSize)
		if _, err := io.ReadFull(r, e); err != nil {
			return 0, nil, err
		}

		n = int(binary.BigEndian.Uint16(e))
	case wsLen64:
		e := make([]byte, wsLen64Size)
		if _, err := io.ReadFull(r, e); err != nil {
			return 0, nil, err
		}
		//nolint:gosec // G115: capped by the wsMaxPayload check below.
		n = int(binary.BigEndian.Uint64(e))
	}
	if n > wsMaxPayload {
		return 0, nil, errwsTooBig
	}
	mask := make([]byte, wsMaskKey)
	if _, err := io.ReadFull(r, mask); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%wsMaskKey]
	}
	return op, payload, nil
}

// wsUpgrade performs the RFC 6455 handshake and returns the hijacked conn.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if r.Header.Get("Upgrade") != "websocket" ||
		!headerContains(r.Header.Get("Connection"), "upgrade") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, errwsNotUpgrade
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errwsNoKey
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errwsHijack
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"
	if _, err := rw.Write([]byte(resp)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// wsAcceptKey computes Sec-WebSocket-Accept (the protocol-mandated SHA-1).
func wsAcceptKey(key string) string {
	//nolint:gosec // G401: the RFC 6455 handshake mandates SHA-1.
	h := sha1.New()
	_, _ = io.WriteString(h, key)
	_, _ = io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// headerContains reports whether a comma-separated header value carries
// the token (case-insensitive per RFC 9110).
func headerContains(v, token string) bool {
	for part := range strings.SplitSeq(v, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// Start runs the 5 s real-time feed push loop until ctx is cancelled
// (C-004: the ticker is runtime-virtualized under synctest).
func (s *Server) Start(ctx context.Context) {
	ticker := time.NewTicker(wsPushPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pushTick()
		}
	}
}

// pushTick performs one feed push: the newest client gets the real
// message, older clients the no-data keepalive (reference-simulator
// parity).
func (s *Server) pushTick() {
	st, err := s.state()
	if err != nil {
		return
	}
	fresh, err := json.Marshal(s.wsMessageFor(st))
	if err != nil {
		return
	}
	s.wsMu.Lock()
	clients := append([]*wsClient(nil), s.clients...)
	s.wsMu.Unlock()
	for i, c := range clients {
		out := wsStaleKeepalive
		if i == len(clients)-1 {
			out = fresh
		}
		_ = c.sendText(out)
	}
}

// pushReal sends the current real-time message to one client (the getdata
// answer — every client may request real data, reference-simulator parity).
func (s *Server) pushReal(c *wsClient) error {
	st, err := s.state()
	if err != nil {
		return err
	}
	b, err := json.Marshal(s.wsMessageFor(st))
	if err != nil {
		return err
	}
	return c.sendText(b)
}

// handleWS upgrades the connection (web-token auth first) and runs the
// reader loop until the connection closes.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.dewAuthed(r) {
		writeEnvelope(w, http.StatusUnauthorized, errEnvelope(errnoUnauthorized, "unauthorized"))
		return
	}
	conn, err := wsUpgrade(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c := &wsClient{srv: s, conn: conn}
	s.addClient(c)
	defer s.dropClient(c)
	c.serve()
}

func (s *Server) addClient(c *wsClient) {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	s.clients = append(s.clients, c)
}

func (s *Server) dropClient(c *wsClient) {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	for i, cl := range s.clients {
		if cl == c {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			return
		}
	}
}
