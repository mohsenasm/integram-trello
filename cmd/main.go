package main

import (
	"github.com/kelseyhightower/envconfig"
	trello "github.com/mohsenasm/integram-trello"
	"github.com/requilence/integram"
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
