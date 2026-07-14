package pricing

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
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

// wsTestClient is a minimal WebSocket client for integration tests.
type wsTestClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialWS(t *testing.T, url string) *wsTestClient {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(strings.TrimPrefix(url, "http://"), "https://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	key := randWSKey()
	req := "GET /v1/rates/subscribe HTTP/1.1\r\n" +
		"Host: " + strings.TrimPrefix(url, "http://") + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 101") {
		body, _ := io.ReadAll(br)
		t.Fatalf("expected 101, got %s %s", statusLine, body)
	}
	// drain headers
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return &wsTestClient{conn: conn, br: br}
}

func randWSKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}

func (c *wsTestClient) sendText(payload []byte) error {
	mask := [4]byte{1, 2, 3, 4}
	hdr := []byte{0x81, 0x80 | byte(len(payload))}
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.conn.Write(append(hdr, masked...)); err != nil {
		return err
	}
	return nil
}

func (c *wsTestClient) readFrame() ([]byte, error) {
	return readWSFrame(c.br)
}

func (c *wsTestClient) close() { _ = c.conn.Close() }

func TestWSHandshakeAndSubscribe(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.close()
	subMsg := `{"action":"subscribe","pairs":["USD-BTC"]}`
	if err := c.sendText([]byte(subMsg)); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Give the server time to process the subscribe command.
	time.Sleep(50 * time.Millisecond)
	// Trigger a fanout by updating the spot cache.
	s.spot.Update(Rate{From: "USD", To: "BTC", Bid: 65000, Ask: 65100, Mid: 65050, SourceVenue: "kraken"})
	// Read a frame (the fanout).
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := c.readFrame()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(frame) == 0 {
		t.Fatal("empty frame")
	}
	if !bytes.Contains(frame, []byte("USD-BTC")) {
		t.Fatalf("expected USD-BTC in frame %q", frame)
	}
}

func TestWSUnsubscribeNoFrame(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.close()
	// Subscribe then unsubscribe.
	_ = c.sendText([]byte(`{"action":"subscribe","pairs":["USD-ETH"]}`))
	time.Sleep(20 * time.Millisecond)
	_ = c.sendText([]byte(`{"action":"unsubscribe","pairs":["USD-ETH"]}`))
	time.Sleep(20 * time.Millisecond)
	// Update ETH; should not produce a frame to this client (subscribed set is empty now).
	s.spot.Update(Rate{From: "USD", To: "ETH", Bid: 3000, Ask: 3010, Mid: 3005, SourceVenue: "kraken"})
	c.conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	_, err := c.readFrame()
	if err == nil {
		t.Fatal("expected no frame after unsubscribe")
	}
}

func TestWSPingPong(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.close()
	_ = c.sendText([]byte(`{"action":"ping"}`))
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := c.readFrame()
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if !bytes.Contains(frame, []byte("pong")) {
		t.Fatalf("expected pong, got %q", frame)
	}
}

func TestWSBadUpgrade(t *testing.T) {
	s := helperServer(t)
	mux := http.NewServeMux()
	s.register(mux)
	req := httptest.NewRequest(http.MethodGet, "/v1/rates/subscribe", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-WS, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadWSFrameClose(t *testing.T) {
	// Build a close frame (opcode 0x8) in a buffer and read it.
	hdr := []byte{0x88, 0x00}
	_, err := readWSFrame(bytes.NewReader(hdr))
	if err != io.EOF {
		t.Fatalf("expected EOF on close frame, got %v", err)
	}
}

func TestReadWSFramePing(t *testing.T) {
	hdr := []byte{0x89, 0x02, 'h', 'i'}
	payload, err := readWSFrame(bytes.NewReader(hdr))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(payload) != 0 {
		t.Fatalf("expected empty payload for ping, got %q", payload)
	}
}

func TestReadWSFrameMaskedText(t *testing.T) {
	mask := [4]byte{1, 2, 3, 4}
	payload := []byte("hi")
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	hdr := []byte{0x81, 0x80 | 2}
	hdr = append(hdr, mask[:]...)
	hdr = append(hdr, masked...)
	out, err := readWSFrame(bytes.NewReader(hdr))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hi" {
		t.Fatalf("expected hi, got %q", out)
	}
}

// ---------- RunWithConfig integration ----------

func TestRunWithConfigHealthz(t *testing.T) {
	// Find a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	port := strings.Split(addr, ":")[1]
	t.Setenv("PORT", port)
	t.Setenv("REDIS_URL", "") // force in-memory
	cfg := LoadConfig()
	log := NewLogger("error")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = RunWithConfigCtx(ctx, cfg, log)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	// Wait for server to come up.
	time.Sleep(150 * time.Millisecond)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// ---------- redis URL parse ----------

func TestRedisLockBackendParseError(t *testing.T) {
	_, err := newRedisLockBackend(context.Background(), "://bad-url")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// ---------- binary frame 16-bit length ----------

func TestSendFrame16BitLen(t *testing.T) {
	// Build a payload > 125 bytes and verify the 16-bit length path in sendFrame.
	hub := newWSHub()
	a, b := net.Pipe()
	sub := &wsSubscriber{conn: a, pairs: map[string]struct{}{"USD-BTC": {}}, done: make(chan struct{})}
	hub.add(sub)
	defer hub.remove(sub)
	big := bytes.Repeat([]byte("x"), 200)
	// Read the full frame concurrently since net.Pipe writes block until read.
	type hdrResult struct {
		hdr []byte
		err error
	}
	hc := make(chan hdrResult, 1)
	go func() {
		b.SetReadDeadline(time.Now().Add(2 * time.Second))
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(b, hdr); err != nil {
			hc <- hdrResult{nil, err}
			return
		}
		plen := int(binary.BigEndian.Uint16(hdr[2:4]))
		body := make([]byte, plen)
		if _, err := io.ReadFull(b, body); err != nil {
			hc <- hdrResult{nil, err}
			return
		}
		hc <- hdrResult{hdr, nil}
	}()
	sub.sendFrame(big)
	res := <-hc
	if res.err != nil {
		t.Fatalf("read: %v", res.err)
	}
	if res.hdr[0] != 0x81 {
		t.Fatalf("expected text frame, got %x", res.hdr[0])
	}
	if res.hdr[1] != 126 {
		t.Fatalf("expected 126 (16-bit len), got %d", res.hdr[1])
	}
	plen := binary.BigEndian.Uint16(res.hdr[2:4])
	if plen != 200 {
		t.Fatalf("expected len 200, got %d", plen)
	}
	_ = b.Close()
}

// ---------- handshake accept computation ----------

func TestHandshakeAccept(t *testing.T) {
	// Verify our handshake logic matches the RFC 6455 example.
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	expect := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	hsh := sha1.New()
	hsh.Write([]byte(key + wsHandshakeGUID))
	got := base64.StdEncoding.EncodeToString(hsh.Sum(nil))
	if got != expect {
		t.Fatalf("expected %s, got %s", expect, got)
	}
}