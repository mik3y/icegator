// Package upstreamtest is a fake station for tests and local development.
// It behaves like the Liquidsoap harbor that Azuracast puts in front of its
// live-DJ input, as measured in docs/upstream-azuracast.md.
package upstreamtest

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Source is one source connection the station accepted.
type Source struct {
	Username string
	Started  time.Time
	mu       sync.Mutex
	data     bytes.Buffer
	ended    bool
}

// Bytes is the audio received so far.
func (s *Source) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data.Bytes()...)
}

// Ended reports whether the source has disconnected.
func (s *Source) Ended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// Title is a metadata update the station accepted.
type Title struct {
	Username string
	Song     string
}

// Station is a fake upstream listening on a local port.
type Station struct {
	ln       net.Listener
	mu       sync.Mutex
	accounts map[string]string
	sources  []*Source
	live     *Source
	liveConn net.Conn
	titles   []Title
	checks   int
}

// Start listens on addr ("127.0.0.1:0" for any free port) and accepts the
// given username -> password accounts.
func Start(addr string, accounts map[string]string) (*Station, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	st := &Station{ln: ln, accounts: accounts}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go st.serve(conn)
		}
	}()
	return st, nil
}

func (st *Station) Close()      { st.ln.Close(); st.Kick() }
func (st *Station) URL() string { return "http://" + st.ln.Addr().String() + "/" }

// Kick drops the live source, as a station restart would.
func (st *Station) Kick() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.liveConn != nil {
		st.liveConn.Close()
	}
}

// Sources lists every accepted source connection, oldest first.
func (st *Station) Sources() []*Source {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]*Source(nil), st.sources...)
}

// Live is the current source, or nil.
func (st *Station) Live() *Source {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.live
}

// Titles lists accepted metadata updates, oldest first.
func (st *Station) Titles() []Title {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]Title(nil), st.titles...)
}

// Checks counts metadata requests that arrived while nothing was live.
func (st *Station) Checks() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.checks
}

func (st *Station) serve(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	user, ok := st.auth(req)
	if !ok {
		reply(conn, "401 Unauthorized", "Wrong Authentication data")
		return
	}
	switch {
	case req.Method == "PUT" || req.Method == "SOURCE":
		if req.URL.Path != "/" {
			reply(conn, "400 Bad Request", "Bad Request")
			return
		}
		st.mu.Lock()
		if st.live != nil {
			st.mu.Unlock()
			reply(conn, "403 Mountpoint already taken", "Mountpoint already taken")
			return
		}
		src := &Source{Username: user, Started: time.Now()}
		st.live, st.liveConn = src, conn
		st.sources = append(st.sources, src)
		st.mu.Unlock()

		io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
		buf := make([]byte, 8192)
		for {
			n, err := br.Read(buf)
			src.mu.Lock()
			src.data.Write(buf[:n])
			src.mu.Unlock()
			if err != nil {
				break
			}
		}
		st.mu.Lock()
		st.live, st.liveConn = nil, nil
		st.mu.Unlock()
		src.mu.Lock()
		src.ended = true
		src.mu.Unlock()

	case req.Method == "GET" && req.URL.Path == "/admin/metadata":
		if req.URL.Query().Get("mount") != "/" {
			reply(conn, "400 Bad Request", "Bad Request")
			return
		}
		st.mu.Lock()
		if st.live == nil {
			st.checks++
			st.mu.Unlock()
			reply(conn, "500 Internal Server Error", "Internal Server Error")
			return
		}
		st.titles = append(st.titles, Title{Username: user, Song: req.URL.Query().Get("song")})
		st.mu.Unlock()
		io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\nUpdated metadatas for mount /")

	default:
		reply(conn, "404 Not Found", "Not Found")
	}
}

func (st *Station) auth(req *http.Request) (string, bool) {
	h := strings.TrimPrefix(req.Header.Get("Authorization"), "Basic ")
	raw, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		return "", false
	}
	user, pass, _ := strings.Cut(string(raw), ":")
	want, ok := st.accounts[user]
	return user, ok && want == pass
}

func reply(conn net.Conn, status, msg string) {
	fmt.Fprintf(conn, "HTTP/1.0 %s\r\nContent-Type: text/html\r\n\r\n<html><body><p>%s</p></body></html>", status, msg)
}
