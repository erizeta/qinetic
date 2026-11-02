package main

import (
	"html/template"
	"log"
	"time"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/middleware/csrf"
	"github.com/erizeta/qinetic/middleware/limiter"
)

var page = template.Must(template.New("form").Parse(`<form method="post" action="/comments">
<input type="hidden" name="_csrf" value="{{.}}">
<textarea name="body"></textarea>
<button>Send</button>
</form>`))

func newApp() *qinetic.App {
	app := qinetic.New()
	// 20 requests per minute per IP, counted over a sliding window.
	app.Use(limiter.New(limiter.Config{Max: 20, Expiration: time.Minute, Algorithm: limiter.SlidingWindow}))
	app.Use(csrf.New())

	app.Get("/", func(c *qinetic.Ctx) error {
		c.Type("html")
		return page.Execute(c.Writer(), csrf.TokenFromContext(c))
	})
	app.Post("/comments", func(c *qinetic.Ctx) error {
		// Only reached with the token from the form and a same-origin request.
		return c.SendString("saved: " + c.FormValue("body"))
	})
	return app
}

func main() {
	log.Fatal(newApp().Listen(":3000"))
}
