package main

import (
	"context"

	"github.com/dat267/dscli/cmd"
)

func main() {
	if version != "dev" {
		cmd.Version = version
	}
	cmd.Execute(context.Background())
}