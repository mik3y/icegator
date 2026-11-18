// Command fakestation runs a stand-in for the station, for developing
// against Icegator without putting anything on air. It accepts the logins
// given on the command line and logs what it receives.
package main

import (
	"flag"
	"log"
	"strings"
	"time"

	"github.com/splatboard/icegator/internal/upstreamtest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8005", "listen address")
	logins := flag.String("logins", "icegator:shared-pw", "comma-separated user:password pairs to accept")
	flag.Parse()

	accounts := map[string]string{}
	for _, pair := range strings.Split(*logins, ",") {
		if user, pass, ok := strings.Cut(pair, ":"); ok {
			accounts[user] = pass
		}
	}
	st, err := upstreamtest.Start(*addr, accounts)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fake station at %s", st.URL())

	var sources, titles int
	var live *upstreamtest.Source
	for range time.Tick(250 * time.Millisecond) {
		if s := st.Sources(); len(s) > sources {
			sources = len(s)
			log.Printf("source connected: %s", s[sources-1].Username)
		}
		if l := st.Live(); l != live {
			if l == nil {
				log.Printf("source disconnected after %d bytes", len(live.Bytes()))
			}
			live = l
		}
		for t := st.Titles(); titles < len(t); titles++ {
			log.Printf("title from %s: %q", t[titles].Username, t[titles].Song)
		}
	}
}
