// Command icegator runs the gate. DJs stream to it, and it decides whose
// audio reaches the station.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/splatboard/icegator/internal/auth"
	"github.com/splatboard/icegator/internal/config"
	"github.com/splatboard/icegator/internal/controller"
	"github.com/splatboard/icegator/internal/ingest"
	"github.com/splatboard/icegator/internal/session"
	"github.com/splatboard/icegator/internal/upstream"
)

func main() {
	configPath := flag.String("config", "icegator.yaml", "path to the configuration file")
	newKey := flag.Bool("new-key", false, "print a new DJ stream key and exit")
	flag.Parse()

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
	ctl = controller.New(opt)

	ln, err := net.Listen("tcp", cfg.Listen.Ingest)
	if err != nil {
		return err
	}
	log.Info("icegator started", "ingest", ln.Addr().String(), "upstream", cfg.Upstream.URL,
		"auth", cfg.Auth, "policy", cfg.Handoff.Policy)

	srv := &ingest.Server{Auth: authn, Gate: ctl, Log: log, ReadyAfter: cfg.Handoff.ReadyAfter}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		log.Info("shutting down")
		ln.Close()
		return nil
	case err := <-errc:
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}
