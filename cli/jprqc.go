// Copied from github.com/azimjohn/jprq at cli/jprqc.go, upstream commit
// 3c10e25, licensed under the MIT License (Copyright (c) 2020 Azimjon
// Pulatov). Import paths for server/events and server/tunnel were
// rewritten to this module's cli/jprq package (see cli/jprq/events.go,
// cli/jprq/bind.go); the backend-host and exit-code patches are called
// out inline below; see docs/DESIGN.md §7 for the contract they
// implement.
package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/muzaffarnurillaew/jprq-bek/cli/debugger"
	"github.com/muzaffarnurillaew/jprq-bek/cli/jprq"
)

type jprqClient struct {
	config       Config
	protocol     string
	subdomain    string
	cname        string
	backendHost  string // JPRQ_BACKEND_HOST; overrides upstream's hardcoded "localhost" (DESIGN.md §7.1)
	localServer  string
	remoteServer string
	publicServer string
	httpDebugger debugger.Debugger
	healthy      atomic.Bool // set once TunnelOpened is received; read by /healthz (DESIGN.md §7.3)
}

func (j *jprqClient) Start(port int, debug bool) {
	go j.serveHealth()

	eventCon, err := net.Dial("tcp", j.config.Remote.Events)
	if err != nil {
		log.Printf("failed to connect to event server: %s\n", err)
		os.Exit(jprq.ExitEventServerUnreachable)
	}
	defer eventCon.Close()

	request := jprq.Event[jprq.TunnelRequested]{
		Data: &jprq.TunnelRequested{
			Protocol:   j.protocol,
			Subdomain:  j.subdomain,
			CanonName:  j.cname,
			AuthToken:  j.config.Local.AuthToken,
			CliVersion: version,
		},
	}
	if err := request.Write(eventCon); err != nil {
		log.Printf("failed to send request: %s\n", err)
		os.Exit(jprq.ExitEventServerUnreachable)
	}

	var t jprq.Event[jprq.TunnelOpened]
	if err := t.Read(eventCon); err != nil {
		log.Printf("failed to receive tunnel info: %s\n", err)
		os.Exit(jprq.ExitEventServerUnreachable)
	}
	if t.Data.ErrorMessage != "" {
		log.Print(t.Data.ErrorMessage)
		os.Exit(exitCodeForServerError(t.Data.ErrorMessage))
	}
	j.healthy.Store(true)

	host := j.backendHost
	if host == "" {
		host = "localhost"
	}
	j.localServer = fmt.Sprintf("%s:%d", host, port)
	j.remoteServer = fmt.Sprintf("jprq.%s:%d", j.config.Remote.Domain, t.Data.PrivateServer)
	j.publicServer = fmt.Sprintf("%s:%d", t.Data.Hostname, t.Data.PublicServer)

	if j.protocol == "http" {
		j.publicServer = fmt.Sprintf("https://%s", t.Data.Hostname)
	}

	fmt.Printf("Status: \t Online \n")
	fmt.Printf("Protocol: \t %s \n", strings.ToUpper(j.protocol))
	fmt.Printf("Forwarded: \t %s -> %s \n", strings.TrimSuffix(j.publicServer, ":80"), j.localServer)

	if j.protocol == "http" && debug {
		j.httpDebugger = debugger.New()
		if port, err := j.httpDebugger.Run(0); err == nil {
			fmt.Printf("Http Debugger: \t http://127.0.0.1:%d \n", port)
		}
	}

	var event jprq.Event[jprq.ConnectionReceived]
	for {
		if err := event.Read(eventCon); err != nil {
			log.Printf("failed to receive connection-received event: %s\n", err)
			os.Exit(jprq.ExitEventStreamDropped)
		}
		go j.handleEvent(*event.Data)
	}
}

// serveHealth answers /healthz per DESIGN.md §7.3: 200 once TunnelOpened
// has been received, 503 before that. Bound to all interfaces, not
// 127.0.0.1 — kubelet's httpGet probe runs on the node, outside this
// pod's network namespace, so a loopback-only listener could never
// answer it.
func (j *jprqClient) serveHealth() {
	addr := os.Getenv("JPRQ_HEALTH_ADDR")
	if addr == "" {
		addr = ":9000"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if j.healthy.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("failed to start health server: %s\n", err)
	}
}

// exitCodeForServerError classifies a TunnelOpened.ErrorMessage against
// the stderr prefixes in cli/jprq/exitcodes.go (DESIGN.md §7.2). Matching
// is on the prefix before the format verb, not the full string, per the
// WriteError bug documented there.
func exitCodeForServerError(message string) int {
	switch {
	case strings.HasPrefix(message, jprq.ErrPrefixSubdomainBusy):
		return jprq.ExitSubdomainBusy
	case strings.HasPrefix(message, jprq.ErrPrefixCNAMEBusy):
		return jprq.ExitCNAMEBusy
	case strings.HasPrefix(message, jprq.ErrPrefixTunnelsLimitReached):
		return jprq.ExitAccountTunnelLimit
	case strings.HasPrefix(message, jprq.ErrPrefixAuthFailed):
		return jprq.ExitAuthFailed
	case strings.HasPrefix(message, jprq.ErrPrefixInvalidSubdomain):
		return jprq.ExitInvalidSubdomain
	case strings.HasPrefix(message, jprq.ErrPrefixInviteOnly):
		return jprq.ExitNotAllowlisted
	default:
		return jprq.ExitUnexpected
	}
}

func (j *jprqClient) handleEvent(event jprq.ConnectionReceived) {
	localCon, err := net.Dial("tcp", j.localServer)
	if err != nil {
		log.Printf("failed to connect to local server: %s\n", err)
		return
	}
	defer localCon.Close()

	remoteCon, err := net.Dial("tcp", j.remoteServer)
	if err != nil {
		log.Printf("failed to connect to remote server: %s\n", err)
		return
	}
	defer remoteCon.Close()

	buffer := make([]byte, 2)
	binary.LittleEndian.PutUint16(buffer, event.ClientPort)
	remoteCon.Write(buffer)

	if j.httpDebugger == nil {
		go jprq.Bind(localCon, remoteCon, nil)
		jprq.Bind(remoteCon, localCon, nil)
		return
	}

	debugCon := j.httpDebugger.Connection(event.ClientPort)
	go jprq.Bind(localCon, remoteCon, debugCon.Response())
	jprq.Bind(remoteCon, localCon, debugCon.Request())
}
