package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/coreos/go-systemd/v22/daemon"
	cfg "github.com/grmrgecko/repo-sync/config"
	"github.com/grmrgecko/repo-sync/server"
	"github.com/grmrgecko/repo-sync/state"
	"github.com/kardianos/service"
	log "github.com/sirupsen/logrus"
)

// stopChan carries a shutdown request from the service manager. It is
// buffered so a stop delivered after the signal loop exits cannot block the
// service supervisor.
var stopChan = make(chan struct{}, 1)

// ServerCmd runs the caching mirror server.
type ServerCmd struct {
	Bind string `name:"http.bind" help:"Override the configured bind address." optional:""`
	Port uint   `name:"http.port" help:"Override the configured port." optional:""`
}

// updateCFG applies the command line overrides onto the active
// configuration. The overridden fields are read only when the listener is
// built, which happens on this goroutine, so a reload can amend them in
// place after Init has published the new configuration.
func (c *ServerCmd) updateCFG() {
	if c.Bind != "" {
		cfg.C.HTTP.BindAddr = c.Bind
	}
	if c.Port != 0 {
		cfg.C.HTTP.Port = c.Port
	}
}

// Run starts the mirror server: configuration, state, crawl loops, and the
// HTTP listener, then handles signals until shutdown.
func (c *ServerCmd) Run() error {
	// main installed the configuration; the server additionally needs the
	// domains and mounts the sync commands do without.
	c.updateCFG()
	if err := cfg.C.RequireServer(); err != nil {
		return err
	}

	// Load persisted crawl state.
	if err := state.Load(); err != nil {
		return err
	}

	// Start the background loops and the listener.
	loopCtx, loopCancel := context.WithCancel(context.Background())
	defer loopCancel()
	go state.S.FlushLoop(loopCtx)
	crawlDone := make(chan struct{})
	go func() {
		defer close(crawlDone)
		server.CrawlLoop(loopCtx)
	}()

	// stopCrawls cancels the loops and waits for in-flight crawls so their
	// outcomes are recorded before the final state flush. The crawl loop
	// must return first: a scheduler pass in flight can still dispatch
	// crawls, and a WaitGroup Add racing Wait is a misuse panic.
	stopCrawls := func() {
		loopCancel()
		<-crawlDone
		server.WaitCrawls()
	}

	httpServer := server.NewServer()
	if err := httpServer.Start(); err != nil {
		return err
	}

	// Attach to the service manager when not run interactively, so a stop
	// request arrives on stopChan, then report readiness now that the
	// listener is bound.
	if !service.Interactive() {
		svc, err := new(ServiceCmd).service()
		if err != nil {
			return err
		}
		go svc.Run()
	}
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)

	// Handle signals: reload on SIGHUP, shut down on SIGINT/SIGTERM or on a
	// stop from the service manager.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
SigLoop:
	for {
		select {
		case <-stopChan:
			break SigLoop
		case sig := <-signals:
			if sig != syscall.SIGHUP {
				break SigLoop
			}
			log.Info("Reloading configuration.")
			if err := cfg.InitServer(flags.ConfigPath); err != nil {
				log.WithError(err).Error("Unable to reload configuration; keeping previous values.")
				continue
			}
			applyGlobalFlags()
			c.updateCFG()
			if err := httpServer.Reload(); err != nil {
				// Exiting on a failed rebind must still flush crawl state.
				stopCrawls()
				if saveErr := state.S.Save(); saveErr != nil {
					log.WithError(saveErr).Error("Failed to save state while shutting down.")
				}
				return err
			}
		}
	}

	// Shut down: stop the listener, stop the loops, then flush state.
	log.Info("Shutting down.")
	httpServer.Shutdown()
	stopCrawls()
	return state.S.Save()
}
