package main

import (
	"log"

	"github.com/erizeta/qinetic"
)

func main() {
	app := qinetic.New()
	app.Get("/", func(c *qinetic.Ctx) error {
		return c.SendString("Hello, " + c.Protocol())
	})
	log.Fatal(app.Listen(":3000"))
}
