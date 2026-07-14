package pricing

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wsSubscriber is a single connected WebSocket client with its subscribed pairs.
type wsSubscriber struct {
	conn  net.Conn
	write sync.Mutex
	pairs map[string]struct{}
	done  chan struct{}
}

func (sub *wsSubscriber) close() {
	select {
	case <-sub.done:
	default:
		close(sub.done)
		_ = sub.conn.Close()
	}
}

// sendFrame writes a WebSocket text frame. Returns false on write error.
func (sub *wsSubscriber) sendFrame(payload []byte) bool {
	sub.write.Lock()
	defer sub.write.Unlock()
	// Build frame: FIN|text (0x81), mask=0, len prefix.
	hdr := [10]byte{}
	hdr[0] = 0x81
	plen := len(payload)
	switch {
	case plen <= 125:
		hdr[1] = byte(plen)
		if _, err := sub.conn.Write(hdr[:2]); err != nil {
			globalMetrics.wsMessagesDropped.Inc()
			return false
		}
	case plen <= 0xFFFF:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(plen))
		if _, err := sub.conn.Write(hdr[:4]); err != nil {
			globalMetrics.wsMessagesDropped.Inc()
			return false
		}
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(plen))
		if _, err := sub.conn.Write(hdr[:10]); err != nil {
			globalMetrics.wsMessagesDropped.Inc()
			return false
		}
	}
	if _, err := sub.conn.Write(payload); err != nil {
		globalMetrics.wsMessagesDropped.Inc()
		return false
	}
	globalMetrics.wsMessagesSent.Inc()
	return true
}

// wsHub fans out L1 cache rate updates to all connected subscribers keyed by
// their subscribed pairs.
type wsHub struct {
	mu          sync.RWMutex
	subscribers map[*wsSubscriber]struct{}
}

func newWSHub() *wsHub {
	return &wsHub{subscribers: make(map[*wsSubscriber]struct{})}
}

func (h *wsHub) add(sub *wsSubscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subscribers[sub] = struct{}{}
	globalMetrics.wsActiveConnections.Inc()
}

func (h *wsHub) remove(sub *wsSubscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subscribers[sub]; ok {
		delete(h.subscribers, sub)
		globalMetrics.wsActiveConnections.Dec()
	}
	sub.close()
}

// fanout sends a rate frame to every subscriber whose pair set contains the
// given pair.
func (h *wsHub) fanout(pair string, r Rate) {
	frame, _ := json.Marshal(map[string]any{
		"pair":   pair,
		"rate":   r.Mid,
		"ts":     r.TS.UTC().Format(time.RFC3339Nano),
		"source": r.SourceVenue,
	})
	h.mu.RLock()
	subs := make([]*wsSubscriber, 0, len(h.subscribers))
	for s := range h.subscribers {
		subs = append(subs, s)
	}
	h.mu.RUnlock()
	for _, s := range subs {
		s.write.Lock()
		_, want := s.pairs[pair]
		s.write.Unlock()
		if !want {
			continue
		}
		s.sendFrame(frame)
	}
}

// wsHandshakeGUID is the WebSocket RFC 6455 handshake magic GUID.
const wsHandshakeGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// hijackAndUpgrade performs the WebSocket handshake on the ResponseWriter and
// returns the underlying connection, or an error.
func hijackAndUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	h, ok := w.(http.Hijacker)
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, io.ErrUnexpectedEOF
	}
	hsh := sha1.New()
	hsh.Write([]byte(key + wsHandshakeGUID))
	accept := base64.StdEncoding.EncodeToString(hsh.Sum(nil))
	conn, brw, err := h.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Flush any buffered reader data (there shouldn't be any after a clean handshake).
	if brw.Reader.Buffered() > 0 {
		_ = conn.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return conn, nil
}

// readWSFrame reads a single WebSocket frame from r and returns the (unmasked)
// payload, or an error. Only handles text/binary frames with the FIN bit set;
// closes (0x8) and pings (0x9) are handled and skipped; a close frame returns
// io.EOF.
func readWSFrame(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	opcode := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	plen := int(hdr[1] & 0x7f)
	if plen == 126 {
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, err
		}
		plen = int(binary.BigEndian.Uint16(ext))
	} else if plen == 127 {
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, err
		}
		plen = int(binary.BigEndian.Uint64(ext))
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return nil, err
		}
	}
	payload := make([]byte, plen)
	if plen > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	switch opcode {
	case 0x8: // close
		return nil, io.EOF
	case 0x9: // ping → respond pong (caller can loop)
		return []byte{}, nil
	case 0xA: // pong
		return []byte{}, nil
	default:
		return payload, nil
	}
}

// ratesSubscribeHandler implements WS /v1/rates/subscribe.
// Client sends { "pairs": ["USD-BTC", "USD-ETH"] } on connect; the server
// streams { "pair", "rate", "ts", "source" } frames on every L1 cache update
// for the subscribed pairs. Subsequent subscribe/unsubscribe messages update
// the pair set.
func (s *Server) ratesSubscribeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if strings.ToLower(r.Header.Get("Upgrade")) != "websocket" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "Upgrade: websocket required")
		return
	}
	conn, err := hijackAndUpgrade(w, r)
	if err != nil {
		return
	}
	sub := &wsSubscriber{conn: conn, pairs: make(map[string]struct{}), done: make(chan struct{})}
	if s.wsHub == nil {
		s.wsHub = newWSHub()
	}
	s.wsHub.add(sub)
	defer s.wsHub.remove(sub)
	// Read loop: handle subscribe/unsubscribe/ping commands.
	for {
		select {
		case <-sub.done:
			return
		default:
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		payload, err := readWSFrame(conn)
		if err != nil {
			return
		}
		if len(payload) == 0 {
			continue
		}
		var cmd struct {
			Action string   `json:"action"`
			Pairs  []string `json:"pairs"`
		}
		if err := json.Unmarshal(payload, &cmd); err != nil {
			continue
		}
		sub.write.Lock()
		switch strings.ToLower(cmd.Action) {
		case "subscribe":
			for _, p := range cmd.Pairs {
				sub.pairs[normalizePair(p)] = struct{}{}
			}
		case "unsubscribe":
			for _, p := range cmd.Pairs {
				delete(sub.pairs, normalizePair(p))
			}
		case "ping":
			sub.write.Unlock()
			sub.sendFrame([]byte(`{"pong":true}`))
			continue
		}
		sub.write.Unlock()
	}
}

// normalizePair canonicalizes a pair string to "FROM-TO" uppercase.
func normalizePair(p string) string {
	p = strings.ToUpper(strings.TrimSpace(p))
	if strings.Contains(p, "-") {
		return p
	}
	if len(p) >= 6 {
		return p[:3] + "-" + p[3:]
	}
	return p
}