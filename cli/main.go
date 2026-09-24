// Copied from github.com/azimjohn/jprq at cli/main.go, upstream commit
// 3c10e25, licensed under the MIT License (Copyright (c) 2020 Azimjon
// Pulatov). Diffs from upstream are limited to the exit-code, health
// endpoint, JSON-logging, SIGTERM and env-var patches called out inline
// below; see docs/DESIGN.md §7 for the contract they implement.
package main

import (
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/muzaffarnurillaew/jprq-bek/cli/jprq"
)

var version = "2.4"

type Flags struct {
	debug     bool
	cname     string
	subdomain string
}

func printVersion() {
	log.Printf("v%s", version)
	os.Exit(0)
}

func printHelp() {
	fmt.Printf("Usage: jprq <command> [arguments]\n\n")
	fmt.Println("Commands:")
	fmt.Println("  auth  <token>               Set authentication token from jprq.io/auth")
	fmt.Println("  tcp   <port>                Start a TCP tunnel on the specified port")
	fmt.Println("  http  <port>                Start an HTTP tunnel on the specified port")
	fmt.Println("  http  <port> -s <subdomain> Start an HTTP tunnel with a custom subdomain")
	fmt.Println("  http  <port> --debug        Debug an HTTP tunnel with Jprq Debugger")
	fmt.Println("  serve <dir>                 Serve files with built-in Http Server")
	fmt.Println("  --help                      Show this help message")
	fmt.Println("  --version                   Show the version number")
	fmt.Println()
	fmt.Println("Environment variables (fall back for the arg/flag of the same meaning):")
	fmt.Println("  JPRQ_PROTOCOL      must be \"http\" in this version")
	fmt.Println("  JPRQ_BACKEND_HOST  backend host to dial instead of localhost, e.g. a Kubernetes Service DNS name")
	fmt.Println("  JPRQ_BACKEND_PORT  backend port")
	fmt.Println("  JPRQ_SUBDOMAIN     requested subdomain")
	fmt.Println("  JPRQ_TOKEN_FILE    path to a file containing the raw auth token")
	fmt.Println("  JPRQ_HEALTH_ADDR   address for the /healthz listener (default :9000; kept on all interfaces, not 127.0.0.1, so kubelet's node-local probe can reach it)")
	os.Exit(0)
}

func main() {
	log.SetFlags(0)
	// Logs are always JSON (DESIGN.md §7.4): redirect the standard log
	// package's output through an slog JSON handler without touching any
	// existing log.Printf/log.Fatalf call site.
	log.SetOutput(slog.NewLogLogger(slog.NewJSONHandler(os.Stderr, nil), slog.LevelInfo).Writer())

	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "help", "--help":
			printHelp()
		case "version", "--version":
			printVersion()
		}
	}

	// JPRQ_PROTOCOL and JPRQ_BACKEND_PORT stand in for the command and
	// port positional args when the caller doesn't supply them (DESIGN.md
	// §7.1); args still win over env when present.
	command := argOrEnv(os.Args, 1, "JPRQ_PROTOCOL")
	arg := argOrEnv(os.Args, 2, "JPRQ_BACKEND_PORT")
	if command == "" {
		log.Println("no command specified")
		printHelp()
	}
	if arg == "" {
		log.Println("no arg supplied")
		printHelp()
	}

	protocol, port := "", 0
	var flagArgs []string
	if len(os.Args) > 3 {
		flagArgs = os.Args[3:]
	}
	flags := parseFlags(flagArgs)
	if flags.subdomain == "" {
		flags.subdomain = os.Getenv("JPRQ_SUBDOMAIN")
	}

	switch command {
	case "auth":
		handleAuth(arg)
	case "serve":
		protocol, port = handleServe(arg)
	case "tcp", "http":
		protocol = command
		port, _ = strconv.Atoi(arg)
	default:
		log.Fatalf("unknown command: %s, jprq --help", command)
	}

	if port <= 0 {
		log.Fatalf("port number must be a positive integer")
	}

	var conf Config
	if err := conf.Load(); err != nil {
		log.Print(err)
		os.Exit(jprq.ExitConfigFailure)
	}

	fmt.Printf("jprq %s \t press Ctrl+C to quit\n\n", version)
	defer log.Println("jprq tunnel closed")

	client := jprqClient{
		config:      conf,
		protocol:    protocol,
		subdomain:   flags.subdomain,
		cname:       flags.cname,
		backendHost: os.Getenv("JPRQ_BACKEND_HOST"),
	}

	go client.Start(port, flags.debug)

	signalChan := make(chan os.Signal, 1)
	// SIGTERM is included alongside SIGINT because Kubernetes sends
	// SIGTERM on pod deletion (DESIGN.md §7.5).
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	<-signalChan
}

// argOrEnv returns os.Args[i] if present, else the named env var
// (DESIGN.md §7.1: args and flags win over env when both are given).
func argOrEnv(args []string, i int, env string) string {
	if i < len(args) {
		return args[i]
	}
	return os.Getenv(env)
}

func parseFlags(args []string) Flags {
	var flags Flags
	for i, arg := range args {
		switch arg {
		case "-d", "-debug", "--debug":
			flags.debug = true
		case "-s", "-subdomain", "--subdomain":
			if i+1 >= len(args) {
				log.Fatal("missing value for subdomain flag, jprq --help")
			}
			flags.subdomain = args[i+1]
		case "-c", "-cname", "--cname":
			if i+1 >= len(args) {
				log.Fatal("missing value for cname flag, jprq --help")
			}
			flags.cname = args[i+1]
		}
	}
	return flags
}

func handleAuth(token string) {
	config := Config{
		Local: struct {
			AuthToken string `json:"auth_token"`
		}{token},
	}
	if err := config.Write(); err != nil {
		log.Fatalf("error writing config: %s", err)
	}
	log.Println("auth token has been set")
	os.Exit(0)
}

func handleServe(dir string) (string, int) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		log.Fatalf("no such dir %s", dir)
	}

	handler := http.FileServer(http.Dir(dir))
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatalf("failed to start server: %s", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	go func() {
		if err := http.Serve(listener, handler); err != nil {
			log.Fatalf("cannot serve files on %s: %s", dir, err)
		}
	}()

	time.AfterFunc(600*time.Millisecond, func() {
		log.Println("Serving: \t", dir)
	})
	return "http", port
}
