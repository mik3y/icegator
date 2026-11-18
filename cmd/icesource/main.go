// Command icesource is a minimal Icecast source client for development. It
// streams an MP3 file in real time to a server (Icegator or a real station)
// and can send metadata updates the way DJ software does. It speaks the
// protocol over a raw socket and prints everything the server says, so it
// doubles as a probe for how a given upstream behaves.
package main

import (
	"bufio"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/splatboard/icegator/internal/mp3"
)

func main() {
	var (
		addr    = flag.String("addr", "127.0.0.1:8000", "server host:port")
		mount   = flag.String("mount", "/", "mount point")
		user    = flag.String("user", "source", "username")
		pass    = flag.String("pass", "", "password")
		file    = flag.String("file", "testdata/tone-2s.mp3", "MP3 file to stream (looped)")
		method  = flag.String("method", "put", "put | source")
		dur     = flag.Duration("for", 30*time.Second, "how long to stream")
		title   = flag.String("title", "", "send this title as a metadata update")
		titleAt = flag.Duration("title-at", 5*time.Second, "when to send -title, from stream start")
		only    = flag.Bool("title-only", false, "send -title and exit without streaming")
		name    = flag.String("name", "icesource", "ice-name header")
	)
	flag.Parse()
	log.SetFlags(log.Lmicroseconds)
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(*user+":"+*pass))

	if *only {
		sendTitle(*addr, *mount, auth, *title)
		return
	}

	data, err := os.ReadFile(*file)
	if err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	conn, err := net.DialTimeout("tcp", *addr, 10*time.Second)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	log.Printf("connected in %v", time.Since(start).Round(time.Millisecond))

	var req strings.Builder
	switch *method {
	case "put":
		fmt.Fprintf(&req, "PUT %s HTTP/1.1\r\nHost: %s\r\nExpect: 100-continue\r\n", *mount, *addr)
	case "source":
		fmt.Fprintf(&req, "SOURCE %s HTTP/1.0\r\n", *mount)
	default:
		log.Fatalf("unknown method %q", *method)
	}
	fmt.Fprintf(&req, "Authorization: %s\r\nUser-Agent: icesource/0.1\r\nContent-Type: audio/mpeg\r\n", auth)
	fmt.Fprintf(&req, "ice-name: %s\r\nice-public: 0\r\n\r\n", *name)
	if _, err := io.WriteString(conn, req.String()); err != nil {
		log.Fatalf("write request: %v", err)
	}

	// Print whatever the server sends, whenever it sends it.
	first := make(chan string, 1)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		br := bufio.NewReader(conn)
		n := 0
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				log.Printf("< %q", line)
				if n == 0 {
					first <- line
				}
				n++
			}
			if err != nil {
				log.Printf("server closed: %v (after %v)", err, time.Since(start).Round(time.Millisecond))
				return
			}
		}
	}()

	select {
	case line := <-first:
		f := strings.Fields(line)
		if len(f) < 2 || (f[1] != "100" && f[1] != "200") {
			<-closed
			os.Exit(1)
		}
	case <-closed:
		os.Exit(1)
	case <-time.After(5 * time.Second):
		log.Printf("no response after 5s; sending audio anyway")
	}

	if *title != "" {
		time.AfterFunc(*titleAt, func() { sendTitle(*addr, *mount, auth, *title) })
	}

	// Pace frames against the wall clock so the server receives real time.
	log.Printf("streaming %s for %v", *file, *dur)
	begin := time.Now()
	var sent time.Duration
	var bytes int
	for sent < *dur {
		r := mp3.NewReader(strings.NewReader(string(data)))
		for sent < *dur {
			f, err := r.Next()
			if err != nil {
				break
			}
			if _, err := conn.Write(f.Data); err != nil {
				log.Fatalf("write failed after %v of audio: %v", sent.Round(time.Millisecond), err)
			}
			bytes += len(f.Data)
			sent += f.Header.Duration()
			if ahead := sent - time.Since(begin); ahead > 0 {
				time.Sleep(ahead)
			}
		}
	}
	log.Printf("sent %v of audio, %d bytes; disconnecting", sent.Round(time.Millisecond), bytes)
}

func sendTitle(addr, mount, auth, title string) {
	q := url.Values{"mode": {"updinfo"}, "mount": {mount}, "song": {title}}
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		log.Printf("metadata dial: %v", err)
		return
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /admin/metadata?%s HTTP/1.0\r\nHost: %s\r\nAuthorization: %s\r\nUser-Agent: icesource/0.1\r\n\r\n", q.Encode(), addr, auth)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	body, _ := io.ReadAll(conn)
	log.Printf("metadata %q -> %q", title, string(body))
}
