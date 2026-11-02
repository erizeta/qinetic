package main

import (
	"log"
	"strconv"
	"sync"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
)

type user struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type store struct {
	mu    sync.Mutex
	next  int
	users map[int]user
}

func newApp() *qinetic.App {
	s := &store{users: map[int]user{}}
	app := qinetic.New()

	api := app.Group("/api")
	api.Get("/users/:id", func(c *qinetic.Ctx) error {
		id, err := c.ParamsInt("id")
		if err != nil {
			return qinetic.ErrBadRequest
		}
		s.mu.Lock()
		u, ok := s.users[id]
		s.mu.Unlock()
		if !ok {
			return qinetic.ErrNotFound
		}
		return c.JSON(u)
	})
	api.Post("/users", func(c *qinetic.Ctx) error {
		var u user
		if err := c.Bind().JSON(&u); err != nil {
			return err
		}
		if u.Name == "" {
			return qinetic.NewError(422, "name is required")
		}
		s.mu.Lock()
		s.next++
		u.ID = s.next
		s.users[u.ID] = u
		s.mu.Unlock()
		c.Set("Location", "/api/users/"+strconv.Itoa(u.ID))
		return c.Status(201).JSON(u)
	})
	api.Delete("/users/:id", func(c *qinetic.Ctx) error {
		id, err := c.ParamsInt("id")
		if err != nil {
			return qinetic.ErrBadRequest
		}
		s.mu.Lock()
		delete(s.users, id)
		s.mu.Unlock()
		return c.SendStatus(204)
	})
	return app
}

func main() {
	srv := &qilat.Server{Handler: newApp()}
	log.Fatal(srv.ListenAndServe(":3000"))
}
