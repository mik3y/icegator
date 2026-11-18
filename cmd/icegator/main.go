// Command icegator runs the gate. DJs stream to it, and it decides whose
// audio reaches the station.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/crypto/bcrypt"

	"github.com/splatboard/icegator/internal/api"
	"github.com/splatboard/icegator/internal/auth"
	"github.com/splatboard/icegator/internal/config"
	"github.com/splatboard/icegator/internal/controller"
	"github.com/splatboard/icegator/internal/ingest"
	"github.com/splatboard/icegator/internal/levels"
	"github.com/splatboard/icegator/internal/session"
	"github.com/splatboard/icegator/internal/upstream"
	"github.com/splatboard/icegator/web"
)

func main() {
	configPath := flag.String("config", "icegator.yaml", "path to the configuration file")
	newKey := flag.Bool("new-key", false, "print a new DJ stream key and exit")
	hashPassword := flag.Bool("hash-password", false, "read an operator password from stdin, print its hash for the config, and exit")
	flag.Parse()

	if *hashPassword {
		pw, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw = strings.TrimRight(pw, "\r\n")
		if len(pw) < 8 {
			fmt.Fprintln(os.Stderr, "password must be at least 8 characters")
			os.Exit(1)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(string(hash))
		return
	}

	if *newKey {
		fmt.Println(auth.NewKey())
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*configPath, log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	up, err := upstream.New(cfg.Upstream.URL)
	if err != nil {
		return err
	}

	opt := controller.Options{
		Upstream:      up,
		Account:       upstream.Credentials{Username: cfg.Upstream.Username, Password: cfg.Upstream.Password},
		Policy:        controller.Policy(cfg.Handoff.Policy),
		FallbackTitle: func(id session.Identity) string { return cfg.FallbackTitle(id.DisplayName()) },
		Log:           log,
	}
	var authn auth.Authenticator
	var ctl *controller.Controller
	switch cfg.Auth {
	case "local":
		djs := make([]auth.DJ, len(cfg.DJs))
		for i, d := range cfg.DJs {
			djs[i] = auth.DJ{Slug: d.Slug, Name: d.Name, Key: d.Key}
		}
		if authn, err = auth.NewLocal(djs); err != nil {
			return err
		}
	case "forward":
		known, err := auth.OpenKnown(filepath.Join(cfg.StateDir, "known-credentials.json"))
		if err != nil {
			return err
		}
		f := auth.NewForward(up, known)
		f.CurrentTitle = func() (string, bool) { return ctl.OnAirTitle() }
		f.Unverified = func(ip string) (int, int) { return ctl.Unverified(ip) }
		opt.OnAccepted, opt.OnRejected = f.Confirm, f.Reject
		authn = f
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dash := &api.Server{
		Info:      api.Info{Station: cfg.Station.Name, Mode: "standalone", Auth: cfg.Auth, Policy: cfg.Handoff.Policy},
		Operators: map[string]string{},
		Web:       web.App(),
		Log:       log,
	}
	for _, op := range cfg.Operators {
		dash.Operators[op.Username] = op.PasswordHash
	}
	opt.OnChange = dash.Changed
	ctl = controller.New(opt)
	dash.Gate = ctl

	ln, err := net.Listen("tcp", cfg.Listen.Ingest)
	if err != nil {
		return err
	}
	log.Info("icegator started", "ingest", ln.Addr().String(), "upstream", cfg.Upstream.URL,
		"auth", cfg.Auth, "policy", cfg.Handoff.Policy)

	thresholds := levels.Thresholds{Silent: cfg.Levels.SilentDBFS, Low: cfg.Levels.LowDBFS, High: cfg.Levels.HighDBFS}
	srv := &ingest.Server{
		Auth: authn, Gate: ctl, Log: log, ReadyAfter: cfg.Handoff.ReadyAfter,
		OnSession: func(s *session.Session) { go levels.Meter(s, thresholds) },
	}
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()

	if len(cfg.Operators) == 0 {
		log.Warn("no operators configured; the dashboard is not being served")
	} else {
		dln, err := net.Listen("tcp", cfg.Listen.Dashboard)
		if err != nil {
			return err
		}
		log.Info("dashboard listening", "addr", dln.Addr().String())
		if dash.Web == nil {
			log.Warn("this binary was built without the dashboard app, so only its API is served; build with `make`")
		}
		go func() { errc <- http.Serve(dln, dash) }()
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		ln.Close()
		// Disconnecting the DJs ends their turns, which closes any
		// recording in progress so that it is uploaded on the next start.
		ctl.Shutdown()
		return nil
	case err := <-errc:
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}
