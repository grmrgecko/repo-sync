package server

import (
	"context"
	"net"
	"net/http"
	"time"

	cfg "github.com/grmrgecko/repo-sync/config"
	log "github.com/sirupsen/logrus"
)

// shutdownTimeout bounds the graceful shutdown of the listener, after which
// in-flight requests are abandoned.
const shutdownTimeout = 30 * time.Second

// Server owns the mirror's HTTP listener. It binds from the active
// configuration and rebinds on reload when the listen address moves.
type Server struct {
	server *http.Server
	addr   string
}

// NewServer builds a mirror HTTP server from the active configuration.
func NewServer() *Server {
	s := new(Server)
	s.configure()
	return s
}

// configure rebuilds the underlying server from the active configuration.
// A running server is never reconfigured in place; Reload binds a new one.
func (s *Server) configure() {
	s.addr = cfg.C.HTTP.ListenAddr()
	s.server = &http.Server{
		Addr:              s.addr,
		Handler:           Handler(),
		ReadHeaderTimeout: cfg.C.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.C.HTTP.ReadTimeout,
		WriteTimeout:      cfg.C.HTTP.WriteTimeout,
		IdleTimeout:       cfg.C.HTTP.IdleTimeout,
	}
}

// Start binds the listen address and serves in the background. A bind
// failure is returned so the caller can abort startup rather than run
// without a listener.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	log.WithField("addr", s.addr).Info("Mirror server listening.")
	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Error("HTTP server failed.")
		}
	}()
	return nil
}

// Reload rebinds the listener when the active configuration moved the
// listen address, and is a no-op otherwise. The old listener is shut down
// before the new one binds so the address is free. Other HTTP settings are
// read when the server is built, so they take effect on the next rebind.
func (s *Server) Reload() error {
	if cfg.C.HTTP.ListenAddr() == s.addr {
		return nil
	}
	s.Shutdown()
	s.configure()
	return s.Start()
}

// Shutdown stops accepting connections and waits for in-flight requests,
// giving up after shutdownTimeout.
func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = s.server.Shutdown(ctx)
}
