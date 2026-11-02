package main

import (
	"log"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/middleware/compress"
	"github.com/erizeta/qinetic/middleware/cors"
	"github.com/erizeta/qinetic/middleware/logger"
	"github.com/erizeta/qinetic/middleware/recovery"
	"github.com/erizeta/qinetic/middleware/requestid"
	"github.com/erizeta/qinetic/middleware/static"
)

func main() {
	app := qinetic.New()
	// requestid first so the logger sees the ID; logger before recovery so panics are logged as 500s.
	app.Use(requestid.New(), logger.New(), recovery.New(), compress.New())

	api := app.Group("/api", cors.New(cors.Config{
		AllowOrigins:     []string{"https://app.example.com", "https://*.preview.example.com"},
		AllowCredentials: true,
	}))
	api.Get("/hello", func(c *qinetic.Ctx) error {
		return c.JSON(map[string]string{"request_id": requestid.FromContext(c)})
	})

	app.Use("/assets", static.New("./public", static.Config{MaxAge: 3600}))
	log.Fatal(app.Listen(":3000"))
}
