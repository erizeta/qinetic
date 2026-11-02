package qinetic

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erizeta/qilat"
	"github.com/quic-go/quic-go/http3"
)

// protoApp answers every request with c.Protocol().
func protoApp(cfg ...Config) *App {
	app := New(cfg...)
	app.Get("/", func(c *Ctx) error { return c.SendString(c.Protocol()) })
	return app
}

// run serves app with listen in a goroutine and shuts it down when the test ends.
func run(t *testing.T, app *App, listen func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- listen() }()
	t.Cleanup(func() {
		if err := app.ShutdownWithTimeout(5 * time.Second); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; !errors.Is(err, qilat.ErrServerClosed) {
			t.Errorf("listen: %v", err)
		}
	})
}

func qilatGet(t *testing.T, client *qilat.Client, url string) (proto, body string) {
	t.Helper()
	resp := qilat.AcquireResponse()
	defer qilat.ReleaseResponse(resp)
	if err := client.Get(url, resp); err != nil {
		t.Fatal(err)
	}
	b, err := resp.ReadBody()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode(), b)
	}
	return resp.Proto(), string(b)
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestListenerHTTP1AndH2C(t *testing.T) {
	for _, tt := range []struct {
		name string
		h2c  bool
		want string
	}{{"HTTP/1.1", false, "HTTP/1.1"}, {"h2c", true, "HTTP/2.0"}} {
		t.Run(tt.name, func(t *testing.T) {
			app := protoApp(Config{H2C: tt.h2c})
			ln := listenLocal(t)
			run(t, app, func() error { return app.Listener(ln) })
			tr := &qilat.Transport{H2C: tt.h2c}
			defer tr.CloseIdleConnections()
			_, body := qilatGet(t, &qilat.Client{Transport: tr}, "http://"+ln.Addr().String()+"/")
			if body != tt.want {
				t.Fatalf("c.Protocol() = %q, want %q", body, tt.want)
			}
		})
	}
}

func TestListen(t *testing.T) {
	ln := listenLocal(t)
	addr := ln.Addr().String()
	_ = ln.Close()
	app := protoApp()
	run(t, app, func() error { return app.Listen(addr) })
	waitDial(t, "tcp", addr)
	tr := &qilat.Transport{}
	defer tr.CloseIdleConnections()
	if _, body := qilatGet(t, &qilat.Client{Transport: tr}, "http://"+addr+"/"); body != "HTTP/1.1" {
		t.Fatalf("got %q", body)
	}
}

func waitDial(t *testing.T, network, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial(network, addr)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server at %s did not start: %v", addr, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// freeTCPAndUDPAddr returns a loopback address whose port is free on both TCP and UDP, as ListenTLS needs.
func freeTCPAndUDPAddr(t *testing.T) string {
	t.Helper()
	for range 20 {
		ln := listenLocal(t)
		pc, err := net.ListenPacket("udp", ln.Addr().String())
		addr := ln.Addr().String()
		_ = ln.Close()
		if err == nil {
			_ = pc.Close()
			return addr
		}
	}
	t.Fatal("no port free on both TCP and UDP")
	return ""
}

func devCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	cert, err := qilat.GenerateDevCert()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	return cert, pool
}

// TestListenTLS covers HTTP/1.1 and HTTP/2 over TLS and the switch to HTTP/3 advertised through Alt-Svc.
func TestListenTLS(t *testing.T) {
	cert, pool := devCert(t)
	addr := freeTCPAndUDPAddr(t)
	app := protoApp()
	run(t, app, func() error { return app.ListenTLSWithCertificate(addr, cert) })
	waitDial(t, "tcp", addr)
	url := "https://" + addr + "/"

	h1 := &qilat.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, DisableHTTP2: true}
	h2 := &qilat.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	defer h1.CloseIdleConnections()
	defer h2.CloseIdleConnections()
	if _, body := qilatGet(t, &qilat.Client{Transport: h1}, url); body != "HTTP/1.1" {
		t.Fatalf("HTTP/1.1 over TLS: got %q", body)
	}
	if _, body := qilatGet(t, &qilat.Client{Transport: h2}, url); body != "HTTP/2.0" {
		t.Fatalf("HTTP/2 over TLS: got %q", body)
	}

	h3 := &qilat.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, EnableHTTP3: true}
	defer h3.CloseIdleConnections()
	client := &qilat.Client{Transport: h3}
	// The UDP listener may open just after TCP, so early responses can lack Alt-Svc.
	deadline := time.Now().Add(5 * time.Second)
	for {
		proto, body := qilatGet(t, client, url)
		if proto == "HTTP/3.0" {
			if body != "HTTP/3.0" {
				t.Fatalf("c.Protocol() over HTTP/3 = %q", body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("client never switched to HTTP/3, last protocol %s", proto)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestListenQUIC(t *testing.T) {
	cert, pool := devCert(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	app := protoApp(Config{TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}})
	run(t, app, func() error { return app.ListenQUIC(pc) })
	tr := &http3.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	defer func() { _ = tr.Close() }()
	resp, err := (&http.Client{Transport: tr}).Get("https://" + pc.LocalAddr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "HTTP/3.0" {
		t.Fatalf("got %q", body)
	}
}

func TestContextCanceledWhenClientGoes(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan error, 1)
	app := New()
	app.Get("/", func(c *Ctx) error {
		close(entered)
		select {
		case <-c.Context().Done():
			canceled <- c.Context().Err()
		case <-time.After(5 * time.Second):
			canceled <- errors.New("context not canceled")
		}
		return nil
	})
	ln := listenLocal(t)
	run(t, app, func() error { return app.Listener(ln) })
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+ln.Addr().String()+"/", nil)
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	go func() {
		<-entered
		cancel()
	}()
	if resp, err := (&http.Client{Transport: tr}).Do(req); err == nil {
		_ = resp.Body.Close()
	}
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("handler saw %v, want context.Canceled", err)
	}
}

func TestShutdownWaitsForRequests(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	app := New()
	app.Get("/", func(c *Ctx) error {
		close(entered)
		<-release
		return c.SendString("done")
	})
	var order []string
	var mu sync.Mutex
	hook := func(name string, err error) func(context.Context) error {
		return func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
			return err
		}
	}
	hookErr := errors.New("flush failed")
	app.OnShutdown(hook("first", nil))
	app.OnShutdown(hook("second", hookErr))

	ln := listenLocal(t)
	served := make(chan error, 1)
	go func() { served <- app.Listener(ln) }()
	got := make(chan string, 1)
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	go func() {
		resp, err := (&http.Client{Transport: tr}).Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		got <- string(b)
	}()
	<-entered
	shut := make(chan error, 1)
	go func() { shut <- app.ShutdownWithTimeout(5 * time.Second) }()
	select {
	case err := <-shut:
		t.Fatalf("Shutdown returned %v while a request was running", err)
	case <-time.After(100 * time.Millisecond):
	}
	mu.Lock()
	if len(order) != 0 {
		t.Fatalf("hooks ran before the server stopped: %v", order)
	}
	mu.Unlock()
	close(release)
	if err := <-shut; !errors.Is(err, hookErr) {
		t.Fatalf("Shutdown = %v, want the hook's error", err)
	}
	if body := <-got; body != "done" {
		t.Fatalf("in-flight request got %q", body)
	}
	if err := <-served; !errors.Is(err, qilat.ErrServerClosed) {
		t.Fatalf("Listener = %v", err)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Fatalf("hooks ran as %v", order)
	}
	// Hooks run once, and the App stays closed.
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown = %v", err)
	}
	if err := app.Listen("127.0.0.1:0"); !errors.Is(err, qilat.ErrServerClosed) {
		t.Fatalf("Listen after Shutdown = %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("hooks ran again: %v", order)
	}
}

// TestListenersShareOneServer serves TCP and UDP from separate Listen calls: HTTP/3 is advertised over
// TCP, and one Shutdown stops both.
func TestListenersShareOneServer(t *testing.T) {
	cert, pool := devCert(t)
	var ln net.Listener
	var pc net.PacketConn
	for range 20 { // same port on TCP and UDP, so Alt-Svc points at the QUIC listener
		ln = listenLocal(t)
		var err error
		if pc, err = net.ListenPacket("udp", ln.Addr().String()); err == nil {
			break
		}
		_ = ln.Close()
	}
	if pc == nil {
		t.Fatal("no port free on both TCP and UDP")
	}
	app := protoApp(Config{TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}})
	tcpDone, udpDone := make(chan error, 1), make(chan error, 1)
	go func() { tcpDone <- app.ListenerTLS(ln) }()
	go func() { udpDone <- app.ListenQUIC(pc) }()

	h3 := &qilat.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, EnableHTTP3: true}
	defer h3.CloseIdleConnections()
	client := &qilat.Client{Transport: h3}
	url := "https://" + ln.Addr().String() + "/"
	waitDial(t, "tcp", ln.Addr().String())
	seen := map[string]bool{}
	for deadline := time.Now().Add(5 * time.Second); !seen["HTTP/3.0"]; {
		_, body := qilatGet(t, client, url)
		seen[body] = true
		if time.Now().After(deadline) {
			t.Fatalf("protocols seen %v, want HTTP/3.0 after Alt-Svc", seen)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !seen["HTTP/2.0"] {
		t.Fatalf("first request was not served over TCP: %v", seen)
	}
	if err := app.ShutdownWithTimeout(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]chan error{"ListenerTLS": tcpDone, "ListenQUIC": udpDone} {
		if err := <-ch; !errors.Is(err, qilat.ErrServerClosed) {
			t.Errorf("%s = %v", name, err)
		}
	}
}

func TestListenTLSWithCertificateAfterStart(t *testing.T) {
	cert, _ := devCert(t)
	app := protoApp()
	ln := listenLocal(t)
	run(t, app, func() error { return app.Listener(ln) })
	waitDial(t, "tcp", ln.Addr().String())
	if err := app.ListenTLSWithCertificate("127.0.0.1:0", cert); !errors.Is(err, ErrCertificateAfterStart) {
		t.Fatalf("got %v, want ErrCertificateAfterStart", err)
	}
}

// TestListenWaitsForShutdown checks Listen returns only after Shutdown, hooks included, has finished,
// so a main that returns with Listen does not cut them short.
func TestListenWaitsForShutdown(t *testing.T) {
	app := protoApp()
	hookStarted, releaseHook := make(chan struct{}), make(chan struct{})
	app.OnShutdown(func(context.Context) error {
		close(hookStarted)
		<-releaseHook
		return nil
	})
	ln := listenLocal(t)
	listened := make(chan error, 1)
	go func() { listened <- app.Listener(ln) }()
	waitDial(t, "tcp", ln.Addr().String())
	shut := make(chan error, 1)
	go func() { shut <- app.ShutdownWithTimeout(5 * time.Second) }()
	<-hookStarted
	select {
	case err := <-listened:
		t.Fatalf("Listener returned %v while an OnShutdown hook was running", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseHook)
	if err := <-listened; !errors.Is(err, qilat.ErrServerClosed) {
		t.Fatalf("Listener = %v", err)
	}
	if err := <-shut; err != nil {
		t.Fatal(err)
	}
}

func TestFailedListenFreesTheSlot(t *testing.T) {
	busy := listenLocal(t)
	defer func() { _ = busy.Close() }()
	app := protoApp()
	if err := app.Listen(busy.Addr().String()); err == nil || errors.Is(err, qilat.ErrServerClosed) {
		t.Fatalf("Listen on a busy port = %v", err)
	}
	ln := listenLocal(t)
	run(t, app, func() error { return app.Listener(ln) })
}

func TestShutdownBeforeListen(t *testing.T) {
	app := protoApp()
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Listener(listenLocal(t)); !errors.Is(err, qilat.ErrServerClosed) {
		t.Fatalf("Listener after Shutdown = %v", err)
	}
}

// serverSettings is the part of a qilat.Server that Config sets; qilat.Server itself must not be copied.
type serverSettings struct {
	read, write, idle         time.Duration
	maxBody                   int64
	h2c, noHTTP3, debugPoison bool
	logger                    *slog.Logger
	handler                   qilat.Handler
}

func settingsOf(srv *qilat.Server) serverSettings {
	return serverSettings{srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout, srv.MaxRequestBodySize, srv.H2C,
		srv.DisableHTTP3, srv.DebugPoison, srv.Logger, srv.Handler}
}

func TestConfigReachesServer(t *testing.T) {
	logger, _ := testLogger()
	seen := make(chan serverSettings, 1)
	app := protoApp(Config{
		ReadTimeout: time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 3 * time.Second,
		BodyLimit: 1234, H2C: true, DisableHTTP3: true, DebugPoison: true, Logger: logger,
		Server: func(srv *qilat.Server) { seen <- settingsOf(srv) },
	})
	ln := listenLocal(t)
	run(t, app, func() error { return app.Listener(ln) })
	want := serverSettings{time.Second, 2 * time.Second, 3 * time.Second, 1234, true, true, true, logger, app}
	if got := <-seen; got != want {
		t.Fatalf("server settings %+v, want %+v", got, want)
	}

	var limit int64
	unlimited := New(Config{BodyLimit: -1, Server: func(srv *qilat.Server) { limit = srv.MaxRequestBodySize }})
	if _, err := unlimited.acquireServer(nil); err != nil || limit != -1 {
		t.Fatalf("BodyLimit -1: MaxRequestBodySize %d, %v", limit, err)
	}
}

func TestListenTLSFromFiles(t *testing.T) {
	cert, pool := devCert(t)
	dir := t.TempDir()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
	addr := freeTCPAndUDPAddr(t)
	app := protoApp(Config{DisableHTTP3: true})
	run(t, app, func() error { return app.ListenTLS(addr, certFile, keyFile) })
	waitDial(t, "tcp", addr)
	tr := &qilat.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	defer tr.CloseIdleConnections()
	if _, body := qilatGet(t, &qilat.Client{Transport: tr}, "https://"+addr+"/"); body != "HTTP/2.0" {
		t.Fatalf("got %q", body)
	}
}

// TestListenTLSWithCertificateKeepsTLSConfig checks the certificate is added to a TLSConfig set through
// Config.Server, without modifying the caller's value.
func TestListenTLSWithCertificateKeepsTLSConfig(t *testing.T) {
	cert, pool := devCert(t)
	base := &tls.Config{MinVersion: tls.VersionTLS13}
	addr := freeTCPAndUDPAddr(t)
	app := protoApp(Config{DisableHTTP3: true, Server: func(srv *qilat.Server) { srv.TLSConfig = base }})
	run(t, app, func() error { return app.ListenTLSWithCertificate(addr, cert) })
	waitDial(t, "tcp", addr)
	tr := &qilat.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS12}}
	defer tr.CloseIdleConnections()
	resp := qilat.AcquireResponse()
	defer qilat.ReleaseResponse(resp)
	if err := (&qilat.Client{Transport: tr}).Get("https://"+addr+"/", resp); err == nil {
		t.Fatal("TLS 1.2 client connected; the configured MinVersion was dropped")
	}
	if len(base.Certificates) != 0 {
		t.Fatal("the caller's TLSConfig was modified")
	}
}
