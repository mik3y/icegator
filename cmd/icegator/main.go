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
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/splatboard/icegator/internal/api"
	"github.com/splatboard/icegator/internal/auth"
	"github.com/splatboard/icegator/internal/config"
	"github.com/splatboard/icegator/internal/controller"
	"github.com/splatboard/icegator/internal/ingest"
	"github.com/splatboard/icegator/internal/levels"
	"github.com/splatboard/icegator/internal/recorder"
	"github.com/splatboard/icegator/internal/session"
	"github.com/splatboard/icegator/internal/streamlover"
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
		Upstream:     up,
		Account:      upstream.Credentials{Username: cfg.Upstream.Username, Password: cfg.Upstream.Password},
		Policy:       controller.Policy(cfg.Handoff.Policy),
		KeepUpstream: cfg.Handoff.StationConnection == "keep",
		FallbackTitle: func(t controller.Turn) string {
			show := ""
			if t.Show != nil {
				show = t.Show.Name
			}
			return cfg.FallbackTitle(t.DJ.DisplayName(), show)
		},
		Log: log,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var authn auth.Authenticator
	var ctl *controller.Controller
	var reporter *streamlover.Reporter
	switch {
	case cfg.Mode() == "streamlover":
		// The station's DJs and schedule come from StreamLover, and what
		// goes to air is reported back to it.
		sl := cfg.Streamlover
		client := streamlover.NewClient(sl.URL, sl.Station, sl.APIKey)
		station := streamlover.NewStation(client, filepath.Join(cfg.StateDir, "streamlover-station.json"), log)
		first, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := station.Sync(first)
		cancel()
		switch {
		case err == nil:
			log.Info("synced with streamlover", "url", sl.URL, "station", sl.Station)
		case streamlover.Permanent(err):
			// A wrong key or station will not come right by itself.
			return fmt.Errorf("streamlover refused the first sync: %w", err)
		default:
			log.Warn("streamlover could not be reached at startup; using the last copy, if there is one", "err", err)
		}
		go station.Run(ctx, sl.SyncInterval)
		authn = station.DJs
		opt.Schedule = station.Schedule
		unlimited, limit, _ := cfg.Handoff.OverrunLimit() // checked when the config was loaded
		opt.Overrun = controller.Overrun{Unlimited: unlimited, Limit: limit}

		reporter = streamlover.NewReporter(client, filepath.Join(cfg.StateDir, "streamlover-turns.json"), log)
		reporter.Visibility = cfg.Streamlover.Visibility
		opt.OnEvent = reporter.OnEvent
		go reporter.Run(ctx)
	case cfg.Auth == "local":
		djs := make([]auth.DJ, len(cfg.DJs))
		for i, d := range cfg.DJs {
			djs[i] = auth.DJ{Slug: d.Slug, Name: d.Name, Key: d.Key}
		}
		if authn, err = auth.NewLocal(djs); err != nil {
			return err
		}
	case cfg.Auth == "forward":
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
	if reporter != nil && cfg.Streamlover.UploadRecordings {
		// Each turn on air with a show is recorded to disk, uploaded
		// through StreamLover when the turn ends, and then deleted here.
		rec, err := recorder.New(recorder.Options{
			Dir: filepath.Join(cfg.StateDir, "recordings"), Deliver: reporter.Deliver, Log: log,
		})
		if err != nil {
			return err
		}
		go rec.Run(ctx)
		opt.Record = func(t controller.Turn) (controller.Recording, error) {
			sub := recorder.Subject{DJ: t.DJ, SessionID: t.SessionID, TurnID: t.ID}
			if t.Show != nil {
				sub.ShowID, sub.Show = t.Show.ID, t.Show.Name
			}
			return rec.Start(sub)
		}
		log.Info("recording shows, to be uploaded to streamlover")
	}

	dash := &api.Server{
		Info:      api.Info{Station: cfg.Station.Name, Mode: cfg.Mode(), Auth: cfg.Auth, Policy: cfg.Handoff.Policy, Overrun: cfg.Handoff.Overrun},
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
	go ctl.Run(ctx)

	ln, err := net.Listen("tcp", cfg.Listen.Ingest)
	if err != nil {
		return err
	}
	log.Info("icegator started", "mode", cfg.Mode(), "ingest", ln.Addr().String(), "upstream", cfg.Upstream.URL,
		"auth", cfg.Auth, "policy", cfg.Handoff.Policy,
		"station_connection", cfg.Handoff.StationConnection)

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
