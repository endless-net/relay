package main

import (
	"os"

	"github.com/endless-net/relay/internal/app"
)

func main() { os.Exit(app.Run("relay", os.Args[1:])) }
