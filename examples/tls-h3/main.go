package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
)

func main() {
	cert, err := qilat.GenerateDevCert()
	if err != nil {
		log.Fatal(err)
	}
	app := qinetic.New()
	app.Get("/", func(c *qinetic.Ctx) error {
		return c.SendString("Hello over " + c.Protocol() + "\n")
	})
	app.OnShutdown(func(_ context.Context) error {
		log.Println("server stopped")
		return nil
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
			log.Println(err)
		}
	}()
	// HTTP/1.1 and HTTP/2 on TCP, HTTP/3 on UDP, all on port 8443. After Shutdown, Listen returns once
	// in-flight requests and OnShutdown hooks are done.
	if err := app.ListenTLSWithCertificate(":8443", cert); !errors.Is(err, qilat.ErrServerClosed) {
		log.Fatal(err)
	}
}
