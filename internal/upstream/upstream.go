// Package upstream is Icegator's source-client connection to the station.
package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Credentials are a source login on the station.
type Credentials struct {
	Username string
	Password string
}

func (c Credentials) header() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Username+":"+c.Password))
}

var (
	// ErrUnauthorized means the station rejected the credentials.
	ErrUnauthorized = errors.New("upstream: credentials rejected")
	// ErrBusy means another source already holds the mount.
	ErrBusy = errors.New("upstream: mount already has a source")
)

// Client knows how to reach one station mount.
type Client struct {
	addr    string // host:port
	mount   string
	tls     bool
	host    string
	Timeout time.Duration // dial and handshake; default 10s
}

// New parses a station URL such as http://azuracast.example.com:8005/.
func New(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("upstream url %q: scheme must be http or https", rawURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("upstream url %q: missing host", rawURL)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	mount := u.Path
	if mount == "" {
		mount = "/"
	}
	return &Client{
		addr: net.JoinHostPort(u.Hostname(), port), mount: mount,
		tls: u.Scheme == "https", host: u.Hostname(), Timeout: 10 * time.Second,
	}, nil
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: c.Timeout}
	if c.tls {
		return (&tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: c.host}}).DialContext(ctx, "tcp", c.addr)
	}
	return d.DialContext(ctx, "tcp", c.addr)
}

// Conn is a live source connection. Frames written to it go to air.
type Conn struct {
	conn net.Conn
	// Closed is closed when the station hangs up on us.
	Closed chan struct{}
}

// Info describes the stream to the station.
type Info struct {
	Name string // ice-name
}

// Connect opens a source connection. It returns once the station has
// accepted it, or ErrUnauthorized / ErrBusy / another error if it did not.
func (c *Client) Connect(ctx context.Context, creds Credentials, info Info) (*Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(c.Timeout))
	var req strings.Builder
	fmt.Fprintf(&req, "PUT %s HTTP/1.1\r\nHost: %s\r\nAuthorization: %s\r\n", c.mount, c.addr, creds.header())
	req.WriteString("User-Agent: icegator\r\nContent-Type: audio/mpeg\r\nExpect: 100-continue\r\nice-public: 0\r\n")
	if info.Name != "" {
		fmt.Fprintf(&req, "ice-name: %s\r\n", sanitize(info.Name))
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("upstream: no response: %w", err)
	}
	f := strings.SplitN(strings.TrimSpace(status), " ", 3)
	if len(f) < 2 {
		conn.Close()
		return nil, fmt.Errorf("upstream: bad response %q", status)
	}
	switch f[1] {
	case "100", "200":
	case "401":
		conn.Close()
		return nil, ErrUnauthorized
	case "403":
		// Liquidsoap says "403 Mountpoint already taken", and Icecast
		// says "403 Forbidden" with a "Mountpoint in use" body. Icecast also
		// uses 403 for a bad source password on some versions, so only
		// call it busy when the text says so.
		conn.Close()
		rest, _ := io.ReadAll(io.LimitReader(br, 2048))
		if text := strings.ToLower(status + string(rest)); strings.Contains(text, "taken") || strings.Contains(text, "in use") {
			return nil, ErrBusy
		}
		return nil, ErrUnauthorized
	default:
		conn.Close()
		return nil, fmt.Errorf("upstream: %s", strings.TrimSpace(status))
	}
	conn.SetDeadline(time.Time{})

	uc := &Conn{conn: conn, Closed: make(chan struct{})}
	go func() {
		// The station sends nothing more while the stream is healthy, so
		// a read returning means it has closed the connection.
		io.Copy(io.Discard, br)
		close(uc.Closed)
	}()
	return uc, nil
}

// Write sends one frame, failing if the station does not take it in time.
func (c *Conn) Write(frame []byte) error {
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(frame)
	return err
}

func (c *Conn) Close() error { return c.conn.Close() }

// metadata sends a title update and returns the HTTP status.
func (c *Client) metadata(ctx context.Context, creds Credentials, title string) (int, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(c.Timeout))
	q := url.Values{"mode": {"updinfo"}, "mount": {c.mount}, "song": {title}}
	fmt.Fprintf(conn, "GET /admin/metadata?%s HTTP/1.0\r\nHost: %s\r\nAuthorization: %s\r\nUser-Agent: icegator\r\n\r\n",
		q.Encode(), c.addr, creds.header())
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// SetTitle updates the station's now-playing title for the live source.
func (c *Client) SetTitle(ctx context.Context, creds Credentials, title string) error {
	status, err := c.metadata(ctx, creds, title)
	if err != nil {
		return err
	}
	if status == 401 || status == 403 {
		return ErrUnauthorized
	}
	if status != 200 {
		return fmt.Errorf("upstream: metadata update returned %d", status)
	}
	return nil
}

// Check asks the station whether it accepts creds, without opening a source
// connection. It works by sending a metadata update, which stations
// authenticate before doing anything else. A 401 or 403 means rejected and
// any other reply means accepted (see docs/upstream-azuracast.md).
//
// The update is a real one if a source is live, so title must be whatever
// is already showing. The caller passes the current on-air title, or any
// harmless text when nothing is on air.
func (c *Client) Check(ctx context.Context, creds Credentials, title string) (ok bool, err error) {
	status, err := c.metadata(ctx, creds, title)
	if err != nil {
		return false, err
	}
	return status != 401 && status != 403, nil
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
}
