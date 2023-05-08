package main

import (
	"github.com/kelseyhightower/envconfig"
	"github.com/mohsenasm/integram"
	trello "github.com/mohsenasm/integram-trello"
)

func main() {
	var cfg trello.Config
	envconfig.MustProcess("TRELLO", &cfg)

	integram.Register(
		cfg,
		cfg.BotConfig.Token, // hx_gitlab_bot,
	)

	integram.Run()
}
