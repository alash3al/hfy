// Command hfy is a simple harness as a service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/alash3al/hfy"
	"github.com/alash3al/hfy/internal/config"
)

// Set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app().Run(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "hfy:", err)
		os.Exit(1)
	}
}

func app() *cli.Command {
	return &cli.Command{
		Name:  "hfy",
		Usage: "a simple harness as a service",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name: "config", Aliases: []string{"c"}, Value: "./harness.yaml",
				Usage: "path to harness.yaml", Sources: cli.EnvVars("HFY_CONFIG"),
			},
		},
		Commands: []*cli.Command{
			{
				Name: "serve", Usage: "run the HTTP API and SSE",
				Flags: []cli.Flag{&cli.BoolFlag{Name: "work", Usage: "also run the workers in this process"}},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					a, err := load(cmd)
					if err != nil {
						return err
					}
					return a.Serve(ctx, cmd.Bool("work"))
				},
			},
			{
				Name: "queue:work", Usage: "run the workers only",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "queues", Aliases: []string{"q"}, Usage: "priority-ordered queues, e.g. turns,embeddings (default: all)"},
					&cli.IntFlag{Name: "workers", Usage: "worker count (default: queue.workers)"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					a, err := load(cmd)
					if err != nil {
						return err
					}
					var queues []string
					for q := range strings.SplitSeq(cmd.String("queues"), ",") {
						if q = strings.TrimSpace(q); q != "" {
							queues = append(queues, q)
						}
					}
					return a.Work(ctx, queues, cmd.Int("workers"))
				},
			},
			{
				Name: "validate", Usage: "check the config, env expansion and startup rules",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if _, err := config.Load(cmd.Root().String("config")); err != nil {
						return err
					}
					fmt.Println("config ok")
					return nil
				},
			},
			{
				Name: "migrate", Usage: "run or inspect migrations: up | down | status",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					a, err := load(cmd)
					if err != nil {
						return err
					}
					sub := cmd.Args().First()
					if sub == "" {
						sub = "up"
					}
					out, err := a.Migrate(ctx, sub)
					fmt.Print(out)
					return err
				},
			},
			{
				Name: "config:example", Usage: "print a commented starter harness.yaml",
				Action: func(context.Context, *cli.Command) error {
					fmt.Print(config.ExampleYAML)
					return nil
				},
			},
			{
				Name: "version", Usage: "print the version",
				Action: func(context.Context, *cli.Command) error {
					fmt.Println(version)
					return nil
				},
			},
		},
	}
}

func load(cmd *cli.Command) (*hfy.App, error) {
	cfg, err := hfy.LoadConfig(cmd.Root().String("config"))
	if err != nil {
		return nil, err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	return hfy.New(cfg), nil
}
