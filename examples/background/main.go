package main

import (
	"bytes"
	"context"
	"log"
	"log/slog"

	"github.com/erizeta/qinetic"
)

// sendReceipt stands in for slow work that outlives the request, such as sending an email.
var sendReceipt = func(ctx context.Context, orderID string, payload []byte) {
	slog.InfoContext(ctx, "receipt sent", "order", orderID, "bytes", len(payload))
}

func newApp() *qinetic.App {
	app := qinetic.New()
	app.Post("/orders/:id", func(c *qinetic.Ctx) error {
		// c is reused once this handler returns, so copy what the goroutine needs first.
		id := c.Params("id") // strings are already copies
		body, err := c.Body()
		if err != nil {
			return err
		}
		payload := bytes.Clone(body) // the body buffer is reused: copy it
		// Keep the request's values without its cancellation, which fires when the handler returns.
		ctx := context.WithoutCancel(c.Context())

		go sendReceipt(ctx, id, payload) // never touches c
		return c.SendStatus(202)
	})
	return app
}

func main() {
	log.Fatal(newApp().Listen(":3000"))
}
