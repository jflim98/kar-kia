// Command assistant is a multi-tenant Telegram assistant powered by headless Claude.
// Each chat (group/DM) is an isolated tenant with its own persona, memory, crons, and
// bot token. Configure chats via the web UI dashboard.
//
//	assistant init        [--data-dir DIR]   scaffold the global data dir (idempotent)
//	assistant run         [--data-dir DIR]   run the daemon (auto-inits an empty dir)
//	assistant consolidate [--data-dir DIR]   run nightly consolidation for all chats once
//	assistant mcp <ls|add|import|rm> …       manage the external MCP server registry
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"assistant/internal/brain"
	"assistant/internal/chat"
	"assistant/internal/config"
	"assistant/internal/initcmd"
	"assistant/internal/mcpserver"
	"assistant/internal/proposals"
	"assistant/internal/telegram"
	"assistant/internal/webui"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("assistant: ")

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "consolidate":
		cmdConsolidate(os.Args[2:])
	case "mcp":
		cmdMCP(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `assistant - multi-tenant Telegram assistant powered by headless Claude

Usage:
  assistant init        [--data-dir DIR]   scaffold the global data dir
  assistant run         [--data-dir DIR]   run the daemon
  assistant consolidate [--data-dir DIR]   run nightly consolidation once
  assistant mcp <ls|add|import|rm> …       manage external MCP servers

Common flags: --data-dir DIR (default $ASSISTANT_DATA_DIR or ./data)
`)
}

func dataDirFlag(fs *flag.FlagSet, args []string) string {
	def := os.Getenv("ASSISTANT_DATA_DIR")
	if def == "" {
		def = "data"
	}
	dir := fs.String("data-dir", def, "data directory")
	_ = fs.Parse(args)
	if abs, err := filepath.Abs(*dir); err == nil {
		return abs
	}
	return *dir
}

func autoInit(dir string) {
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); os.IsNotExist(err) {
		log.Printf("data dir %s not initialized; scaffolding it", dir)
		if _, err := initcmd.Run(dir); err != nil {
			log.Fatalf("auto-init: %v", err)
		}
	}
}

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := dataDirFlag(fs, args)
	created, err := initcmd.Run(dir)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	if len(created) == 0 {
		fmt.Printf("Nothing to do: %s is already initialized.\n", dir)
	} else {
		fmt.Printf("Initialized %s\nCreated:\n", dir)
		for _, p := range created {
			fmt.Printf("  + %s\n", p)
		}
	}
	fmt.Print("\n" + initcmd.NextSteps(dir))
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dir := dataDirFlag(fs, args)
	autoInit(dir)

	cfgMgr, err := config.Load(dir)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg := cfgMgr.Get()
	log.Printf("loaded global config: concurrency=%d default_model=%s", cfg.Concurrency, cfg.DefaultModel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	limiter := brain.NewLimiter(cfg.Concurrency)

	// Gateway needs the chat manager (Handler); the chat manager needs the gateway
	// (Sender) and a way to wake it on token changes — wire via a forward reference.
	var gw *telegram.Gateway
	onTokens := func() {
		if gw != nil {
			gw.Wake()
		}
	}
	chatMgr, err := chat.New(dir, cfgMgr, limiter, onTokens)
	if err != nil {
		log.Fatalf("chat manager: %v", err)
	}

	tokensFn := func() []string {
		toks := append([]string{}, cfgMgr.Secrets().BotTokens...)
		return append(toks, chatMgr.Tokens()...)
	}
	gw = telegram.New(chatMgr, tokensFn)
	chatMgr.SetSender(gw)

	pm := proposals.New(gw,
		func(id int64) (proposals.Committer, bool) { m, ok := chatMgr.MemoryFor(id); return m, ok },
		chatMgr.TokenFor,
	)
	chatMgr.SetProposals(pm)

	mcpSrv := mcpserver.New(chatMgr, pm, chatMgr, chatMgr)
	web := webui.New(cfgMgr, chatMgr)

	if err := chatMgr.Start(ctx); err != nil {
		log.Fatalf("start chats: %v", err)
	}
	cfgMgr.OnChange(onTokens) // global bot_tokens change -> reconnect

	go func() {
		if err := mcpSrv.Serve(ctx, cfg.MCPAddr); err != nil && ctx.Err() == nil {
			log.Printf("mcp server: %v", err)
		}
	}()
	go func() {
		if err := web.Serve(ctx, cfg.WebUIAddr); err != nil && ctx.Err() == nil {
			log.Printf("web ui: %v", err)
		}
	}()

	log.Print("starting telegram gateway (Ctrl-C to stop)")
	if err := gw.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("gateway: %v", err)
	}
	log.Print("shutting down")
}

func cmdConsolidate(args []string) {
	fs := flag.NewFlagSet("consolidate", flag.ExitOnError)
	dir := dataDirFlag(fs, args)

	cfgMgr, err := config.Load(dir)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	limiter := brain.NewLimiter(cfgMgr.Get().Concurrency)
	chatMgr, err := chat.New(dir, cfgMgr, limiter, func() {})
	if err != nil {
		log.Fatalf("chat manager: %v", err)
	}
	ctx := context.Background()
	if err := chatMgr.Start(ctx); err != nil {
		log.Fatalf("start chats: %v", err)
	}
	log.Print("running consolidation for all enabled chats")
	chatMgr.ConsolidateAll(ctx)
	log.Print("done")
}
