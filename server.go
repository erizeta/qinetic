package qinetic

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"github.com/erizeta/qilat"
)

// ErrCertificateAfterStart is returned by ListenTLSWithCertificate when another Listen call already started
// the App's server, whose TLS configuration can no longer change. Use Config.TLSConfig for both instead.
var ErrCertificateAfterStart = errors.New("qinetic: ListenTLSWithCertificate called while the app is already serving")

// acquireServer returns the App's qilat.Server, built from Config on first use, and counts the caller as
// one of the Listen calls using it. Concurrent Listen calls share it, so one Shutdown stops them all.
func (app *App) acquireServer(cert *tls.Certificate) (*qilat.Server, error) {
	app.srvMu.Lock()
	defer app.srvMu.Unlock()
	if app.closed {
		return nil, qilat.ErrServerClosed
	}
	if app.srv != nil {
		if cert != nil {
			return nil, ErrCertificateAfterStart
		}
		app.active++
		return app.srv, nil
	}
	cfg := &app.config
	srv := &qilat.Server{
		Handler:            app,
		TLSConfig:          cfg.TLSConfig,
		ReadTimeout:        cfg.ReadTimeout,
		WriteTimeout:       cfg.WriteTimeout,
		IdleTimeout:        cfg.IdleTimeout,
		MaxRequestBodySize: app.bodyLimit,
		H2C:                cfg.H2C,
		DisableHTTP3:       cfg.DisableHTTP3,
		DebugPoison:        cfg.DebugPoison,
		Logger:             cfg.Logger,
	}
	if cfg.Server != nil {
		cfg.Server(srv)
	}
	if cert != nil {
		// Set before any Serve call reads it; qilat clones TLSConfig when serving starts.
		srv.TLSConfig = withCertificate(srv.TLSConfig, *cert)
	}
	app.srv, app.active = srv, 1
	return srv, nil
}

// serve runs fn on the App's server. When the last Listen call returns without a Shutdown, the server is
// dropped so a later Listen starts afresh. After a Shutdown, it waits for Shutdown to finish, so a program
// whose main returns when Listen does still lets in-flight requests and OnShutdown hooks complete.
func (app *App) serve(cert *tls.Certificate, fn func(srv *qilat.Server) error) error {
	srv, err := app.acquireServer(cert)
	if err != nil {
		return err
	}
	err = fn(srv)
	app.srvMu.Lock()
	app.active--
	if app.active == 0 && !app.closed {
		app.srv = nil
	}
	closed := app.closed
	app.srvMu.Unlock()
	if closed && errors.Is(err, qilat.ErrServerClosed) {
		<-app.stopped
	}
	return err
}

// Listen serves HTTP/1.1 on the TCP address addr, and cleartext HTTP/2 when Config.H2C is set. It blocks
// until the server stops; after Shutdown it returns qilat.ErrServerClosed once Shutdown has finished.
//
// The Listen methods may run concurrently on one App, for example Listener for TCP and ListenQUIC for UDP:
// they share one qilat.Server and one Shutdown.
func (app *App) Listen(addr string) error {
	return app.serve(nil, func(srv *qilat.Server) error { return srv.ListenAndServe(addr) })
}

// ListenTLS serves HTTPS on addr: HTTP/2 and HTTP/1.1 over TCP and, unless Config.DisableHTTP3 is set,
// HTTP/3 over UDP on the same port, advertised to TCP clients with Alt-Svc. The certificate files may be
// empty when Config.TLSConfig holds certificates.
func (app *App) ListenTLS(addr, certFile, keyFile string) error {
	return app.serve(nil, func(srv *qilat.Server) error { return srv.ListenAndServeTLS(addr, certFile, keyFile) })
}

// ListenTLSWithCertificate is like ListenTLS with a certificate already loaded, such as one from
// qilat.GenerateDevCert, added to Config.TLSConfig. It must start the App's server: while another Listen
// call is running it returns ErrCertificateAfterStart.
func (app *App) ListenTLSWithCertificate(addr string, cert tls.Certificate) error {
	return app.serve(&cert, func(srv *qilat.Server) error { return srv.ListenAndServeTLS(addr, "", "") })
}

func withCertificate(cfg *tls.Config, cert tls.Certificate) *tls.Config {
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		cfg = cfg.Clone()
	}
	cfg.Certificates = append(cfg.Certificates, cert)
	return cfg
}

// Listener serves HTTP/1.1, and cleartext HTTP/2 with Config.H2C, on ln. A listener from tls.NewListener
// offers HTTP/2 only if its config lists "h2" in NextProtos; ListenerTLS does that for you.
func (app *App) Listener(ln net.Listener) error {
	return app.serve(nil, func(srv *qilat.Server) error { return srv.Serve(ln) })
}

// ListenerTLS serves HTTPS, HTTP/2 and HTTP/1.1, on ln with the certificates of Config.TLSConfig.
// Run ListenQUIC alongside it on a UDP socket to add HTTP/3.
func (app *App) ListenerTLS(ln net.Listener) error {
	return app.serve(nil, func(srv *qilat.Server) error { return srv.ServeTLS(ln, "", "") })
}

// ListenQUIC serves HTTP/3 on pc with the certificates of Config.TLSConfig. Responses from a ListenerTLS
// on the same port advertise it with Alt-Svc.
func (app *App) ListenQUIC(pc net.PacketConn) error {
	return app.serve(nil, func(srv *qilat.Server) error { return srv.ServeQUIC(pc) })
}

// OnShutdown registers fn to run, in registration order, after Shutdown has stopped the server.
// It receives Shutdown's context.
func (app *App) OnShutdown(fn func(ctx context.Context) error) {
	app.srvMu.Lock()
	defer app.srvMu.Unlock()
	app.onShutdown = append(app.onShutdown, fn)
}

// Shutdown stops accepting connections, waits for in-flight requests until ctx is done, then runs the
// OnShutdown hooks. Running Listen calls return qilat.ErrServerClosed once Shutdown has finished, and later
// ones at once. The hooks run once; the errors of the server and every hook are joined.
func (app *App) Shutdown(ctx context.Context) error {
	app.srvMu.Lock()
	app.closed = true
	srv := app.srv
	app.srvMu.Unlock()
	var errs []error
	if srv != nil {
		errs = append(errs, srv.Shutdown(ctx))
	}
	app.hooksOnce.Do(func() {
		app.srvMu.Lock()
		hooks := app.onShutdown
		app.srvMu.Unlock()
		for _, fn := range hooks {
			errs = append(errs, fn(ctx))
		}
		close(app.stopped)
	})
	return errors.Join(errs...)
}

// ShutdownWithTimeout is Shutdown with a context that expires after d.
func (app *App) ShutdownWithTimeout(d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return app.Shutdown(ctx)
}
