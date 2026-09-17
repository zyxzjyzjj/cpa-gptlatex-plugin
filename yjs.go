package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// prism will not hand a turn to its sandbox agent until the sandbox reports a
// synced y-sweet provider, and the provider only reports that once a client has
// connected the Yjs socket and exchanged sync messages. The socket therefore has
// to stay open for as long as the sandbox is in use — closing it drops the
// sandbox back to "syncing" and the next turn dies in prism's gateway timeout.
//
// Only the handshake is implemented. The document lives on the provider and is
// pushed to us; we never author CRDT updates, so no Yjs implementation is
// needed beyond lib0 varints and the three sync frame shapes.

const (
	yTagSync      = 0
	yTagAuth      = 1
	yTagAwareness = 2

	ySyncStep1 = 0
	ySyncStep2 = 1
)

// appendVarUint writes a lib0 variable-length unsigned integer.
func appendVarUint(dst []byte, n uint64) []byte {
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(dst, b)
		}
		dst = append(dst, b|0x80)
	}
}

// readVarUint reads a lib0 variable-length unsigned integer.
func readVarUint(buf []byte, i int) (uint64, int, bool) {
	var n uint64
	var shift uint
	for {
		if i >= len(buf) || shift > 63 {
			return 0, i, false
		}
		b := buf[i]
		i++
		n |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return n, i, true
		}
		shift += 7
	}
}

// appendVarBytes writes a lib0 length-prefixed byte array.
func appendVarBytes(dst, payload []byte) []byte {
	dst = appendVarUint(dst, uint64(len(payload)))
	return append(dst, payload...)
}

// emptyStateVector is a state vector with zero clock entries, i.e. "we hold
// nothing", which asks the provider to send the whole document.
var emptyStateVector = []byte{0}

// emptyUpdate is a Yjs update carrying zero structs, used to answer the
// provider's SyncStep1 without claiming any content of our own.
var emptyUpdate = []byte{0}

// ySyncStep1Frame requests everything the provider has that we lack.
func ySyncStep1Frame() []byte {
	frame := appendVarUint(nil, yTagSync)
	frame = appendVarUint(frame, ySyncStep1)
	return appendVarBytes(frame, emptyStateVector)
}

// ySyncStep2Frame answers a provider SyncStep1 with the updates we hold.
func ySyncStep2Frame(update []byte) []byte {
	frame := appendVarUint(nil, yTagSync)
	frame = appendVarUint(frame, ySyncStep2)
	return appendVarBytes(frame, update)
}

// yjsSession is one live Yjs socket. It is only meaningful while connected.
type yjsSession struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// close shuts the socket down and waits for the reader loop to finish. Safe to
// call more than once.
func (s *yjsSession) close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		_ = s.conn.Close()
	})
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
}

// run pumps the socket until it fails or ctx is cancelled, answering the
// provider's sync requests so it considers this client in sync.
func (s *yjsSession) run(ctx context.Context) {
	defer close(s.done)

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			_ = s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			continue
		default:
		}

		mt, payload, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		tag, i, ok := readVarUint(payload, 0)
		if !ok || tag != yTagSync {
			continue
		}
		sub, _, ok := readVarUint(payload, i)
		if !ok {
			continue
		}
		if sub == ySyncStep1 {
			// The provider wants our state; we hold none, so say so and it
			// stops waiting on us.
			_ = s.conn.WriteMessage(websocket.BinaryMessage, ySyncStep2Frame(emptyUpdate))
		}
	}
}

// dialYjs opens the provider socket and completes the Auth + SyncStep1
// handshake. The returned session must be kept alive (see close).
func dialYjs(endpoint string, authTokenB64 string) (*yjsSession, error) {
	authBlob, err := base64.RawURLEncoding.DecodeString(authTokenB64)
	if err != nil {
		// Tokens are sometimes padded; retry with the strict decoder.
		if authBlob, err = base64.URLEncoding.DecodeString(authTokenB64); err != nil {
			return nil, fmt.Errorf("解码 y-sweet token 失败: %w", err)
		}
	}

	dialer, err := yjsDialer()
	if err != nil {
		return nil, err
	}
	conn, resp, err := dialer.Dial(endpoint, http.Header{
		"Origin":     []string{baseURL},
		"User-Agent": []string{userAgent},
	})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("连接 y-sweet (%s) 失败: %w (HTTP %d)", endpoint, err, resp.StatusCode)
		}
		return nil, fmt.Errorf("连接 y-sweet (%s) 失败: %w", endpoint, err)
	}

	// The token blob is already a serialised sequence of y-sweet protocol
	// messages whose first entry is Auth(<docId>), so it goes out verbatim.
	if err := conn.WriteMessage(websocket.BinaryMessage, authBlob); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("发送 y-sweet Auth 失败: %w", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, ySyncStep1Frame()); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("发送 y-sweet SyncStep1 失败: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	session := &yjsSession{conn: conn, cancel: cancel, done: make(chan struct{})}
	go session.run(ctx)
	return session, nil
}

// yjsDialer builds a WebSocket dialer that follows the same proxy environment
// variables the HTTP client uses.
func yjsDialer() (*websocket.Dialer, error) {
	dialer := &websocket.Dialer{HandshakeTimeout: 30 * time.Second}

	proxyURL, err := proxyFromEnvironment()
	if err != nil {
		return nil, err
	}
	if proxyURL == nil {
		return dialer, nil
	}
	switch proxyURL.Scheme {
	case "socks5", "socks5h":
		// gorilla ships its own SOCKS5 dialer and picks it up from Proxy.
		dialer.Proxy = http.ProxyFromEnvironment
	default:
		// gorilla cannot speak HTTP proxies itself; tunnel with CONNECT.
		dialer.NetDialContext = connectDialer(proxyURL)
	}
	return dialer, nil
}

// proxyFromEnvironment resolves the proxy Go's HTTP transport would use, so the
// WebSocket path and the HTTP path agree on what "the proxy" is.
func proxyFromEnvironment() (*url.URL, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL, nil)
	if err != nil {
		return nil, err
	}
	return http.ProxyFromEnvironment(req)
}

// connectDialer tunnels a TCP connection through an HTTP CONNECT proxy.
func connectDialer(proxyURL *url.URL) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	host := proxyURL.Host
	if proxyURL.Port() == "" {
		if proxyURL.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, "tcp", host)
		if err != nil {
			return nil, fmt.Errorf("连接代理 %s 失败: %w", host, err)
		}
		var req strings.Builder
		fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
		if u := proxyURL.User; u != nil {
			password, _ := u.Password()
			credentials := base64.StdEncoding.EncodeToString([]byte(u.Username() + ":" + password))
			fmt.Fprintf(&req, "Proxy-Authorization: Basic %s\r\n", credentials)
		}
		req.WriteString("\r\n")

		if _, err := io.WriteString(conn, req.String()); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("代理 CONNECT 写入失败: %w", err)
		}
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("代理 CONNECT 响应解析失败: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = conn.Close()
			return nil, fmt.Errorf("代理 CONNECT %s 返回 %d", addr, resp.StatusCode)
		}
		// Hand back the buffered reader too: ReadResponse may have consumed
		// bytes belonging to the tunnelled stream.
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
