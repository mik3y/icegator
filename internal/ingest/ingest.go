// Package ingest is the DJ-facing listener. It speaks just enough of the
// Icecast source protocol for DJ software to stream to Icegator as if it
// were the station.
//
// Audio arrives on PUT /mount from Icecast 2.4+ clients and on SOURCE /mount
// from older ones. Track title updates arrive on GET /admin/metadata and
// GET /admin.cgi.
//
// The request line is parsed by hand because legacy clients send
// "SOURCE /mount ICE/1.0", which net/http refuses.
package ingest

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/splatboard/icegator/internal/auth"
	"github.com/splatboard/icegator/internal/session"
)

// Gate is what the listener hands sessions and titles to.
type Gate interface {
	Add(*session.Session)
	Remove(*session.Session)
	Find(match func(session.Identity) bool) *session.Session
}

// Server accepts DJ connections.
type Server struct {
	Auth auth.Authenticator
	Gate Gate
	Log  *slog.Logger
	// ReadyAfter is how much audio a DJ must send before they can go on
	// air. Default 3s.
	ReadyAfter time.Duration
	// IdleTimeout disconnects a DJ who stops sending. Default 15s.
	IdleTimeout time.Duration
}

// Serve accepts connections until ln is closed.
func (s *Server) Serve(ln net.Listener) error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.ReadyAfter == 0 {
		s.ReadyAfter = 3 * time.Second
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = 15 * time.Second
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

type request struct {
	method string
	target *url.URL
	header textproto.MIMEHeader
}

func readRequest(br *bufio.Reader) (*request, error) {
	tp := textproto.NewReader(br)
	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return nil, errors.New("malformed request line")
	}
	u, err := url.ParseRequestURI(f[1])
	if err != nil {
		return nil, err
	}
	h, err := tp.ReadMIMEHeader()
	if err != nil && len(h) == 0 && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &request{method: strings.ToUpper(f[0]), target: u, header: h}, nil
}

func (r *request) basicAuth() (user, pass string, ok bool) {
	h := r.header.Get("Authorization")
	if len(h) < 6 || !strings.EqualFold(h[:6], "basic ") {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[6:]))
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(raw), ":")
	return user, pass, ok
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(conn, 16*1024)
	req, err := readRequest(br)
	if err != nil {
		return
	}
	ip := auth.RemoteIP(conn.RemoteAddr().String())
	path := req.target.Path

	switch {
	case req.method == "PUT" || req.method == "SOURCE":
		s.source(conn, br, req, ip)
	case req.method == "GET" && (path == "/admin/metadata" || path == "/admin.cgi"):
		s.metadata(conn, req, ip)
	default:
		respond(conn, "404 Not Found", "", "Icegator accepts source connections only.")
	}
}

func (s *Server) authenticate(conn net.Conn, user, pass, ip string) (session.Identity, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	id, err := s.Auth.Authenticate(ctx, user, pass, ip)
	switch {
	case err == nil:
		return id, true
	case errors.Is(err, auth.ErrLimited):
		s.Log.Warn("dj login refused", "reason", "too many failed attempts from this address or for this username; try again in a few minutes",
			"username", user, "remote", ip)
		respond(conn, "403 Forbidden", "", "Too many attempts. Try again later.")
	default:
		// The log says what was wrong. The DJ is only told that it failed.
		s.Log.Warn("dj login refused", "reason", strings.TrimPrefix(err.Error(), auth.ErrDenied.Error()+": "),
			"username", user, "remote", ip)
		respond(conn, "401 Unauthorized", `WWW-Authenticate: Basic realm="Icegator"`, "Wrong username or password.")
	}
	return id, false
}

func (s *Server) source(conn net.Conn, br *bufio.Reader, req *request, ip string) {
	user, pass, ok := req.basicAuth()
	if !ok {
		respond(conn, "401 Unauthorized", `WWW-Authenticate: Basic realm="Icegator"`, "Authentication required.")
		return
	}
	if ct := req.header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "audio/mpeg") &&
		!strings.HasPrefix(strings.ToLower(ct), "audio/mp3") {
		s.Log.Warn("connection refused: not mp3", "remote", ip, "username", user, "content_type", ct)
		respond(conn, "415 Unsupported Media Type", "", "Icegator accepts MP3 only; this stream is "+ct+".")
		return
	}
	id, ok := s.authenticate(conn, user, pass, ip)
	if !ok {
		return
	}
	if id.Name == "" {
		id.Name = id.Username
	}

	// Clients that sent "Expect: 100-continue" are waiting for exactly
	// that. Everyone else wants the classic "200 OK" before sending.
	if strings.EqualFold(req.header.Get("Expect"), "100-continue") {
		io.WriteString(conn, "HTTP/1.1 100 Continue\r\n\r\n")
	} else {
		io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\n")
	}

	var body io.Reader = br
	if strings.EqualFold(req.header.Get("Transfer-Encoding"), "chunked") {
		body = httputil.NewChunkedReader(br)
	}
	sess := session.New(id, conn.RemoteAddr().String(), req.header.Get("User-Agent"), conn)
	sess.ReadyAfter = s.ReadyAfter

	s.Gate.Add(sess)
	defer s.Gate.Remove(sess)
	err := sess.Run(&idleReader{conn: conn, r: body, timeout: s.IdleTimeout})
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		s.Log.Info("dj stream ended", "dj", id.Username, "session", sess.ID, "err", err)
	}
}

// idleReader resets the connection's read deadline before every read, so a
// DJ who goes silent at the network level is dropped.
type idleReader struct {
	conn    net.Conn
	r       io.Reader
	timeout time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	r.conn.SetDeadline(time.Now().Add(r.timeout))
	return r.r.Read(p)
}

func (s *Server) metadata(conn net.Conn, req *request, ip string) {
	q := req.target.Query()
	user, pass, ok := req.basicAuth()
	if !ok {
		// Shoutcast-style clients put the password in the query, as
		// in /admin.cgi?pass=...&mode=updinfo&song=...
		if pass = q.Get("pass"); pass == "" {
			respond(conn, "401 Unauthorized", `WWW-Authenticate: Basic realm="Icegator"`, "Authentication required.")
			return
		}
		user = "source"
	}
	if q.Get("mode") != "updinfo" {
		respond(conn, "400 Bad Request", "", "Only mode=updinfo is supported.")
		return
	}
	user, pass = auth.SplitShoutcast(user, pass)

	// A DJ who is already connected has already proved who they are with
	// these exact credentials, so match the session before (and instead
	// of) asking the authenticator again.
	sess := s.Gate.Find(func(id session.Identity) bool {
		return id.Upstream != nil && id.Upstream.Username == user && id.Upstream.Password == pass
	})
	if sess == nil {
		id, ok := s.authenticate(conn, user, pass, ip)
		if !ok {
			return
		}
		sess = s.Gate.Find(func(other session.Identity) bool {
			return other.Upstream == nil && other.Username == id.Username
		})
	}
	if sess == nil {
		respond(conn, "400 Bad Request", "", "No stream is connected for this login.")
		return
	}

	title := q.Get("song")
	if title == "" {
		// Some clients send artist and title separately.
		artist, t := q.Get("artist"), q.Get("title")
		switch {
		case artist != "" && t != "":
			title = artist + " - " + t
		default:
			title = artist + t
		}
	}
	title = cleanTitle(title)
	sess.SetTitle(title)
	s.Log.Info("title", "dj", sess.DJ.Username, "session", sess.ID, "title", title)

	body := `<?xml version="1.0"?>` + "\n<iceresponse><message>Metadata update successful</message><return>1</return></iceresponse>\n"
	fmt.Fprintf(conn, "HTTP/1.0 200 OK\r\nContent-Type: text/xml\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

// cleanTitle turns whatever the client sent into tidy UTF-8. Older clients
// send Latin-1, which shows up here as invalid UTF-8.
func cleanTitle(s string) string {
	if !utf8.ValidString(s) {
		r := make([]rune, 0, len(s))
		for i := 0; i < len(s); i++ {
			r = append(r, rune(s[i]))
		}
		s = string(r)
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		s = strings.ToValidUTF8(s[:500], "")
	}
	return s
}

func respond(conn net.Conn, status, extraHeader, msg string) {
	if extraHeader != "" {
		extraHeader += "\r\n"
	}
	fmt.Fprintf(conn, "HTTP/1.0 %s\r\n%sContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\n\r\n%s\n",
		status, extraHeader, len(msg)+1, msg)
}
