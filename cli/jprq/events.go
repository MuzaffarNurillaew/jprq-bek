// Copied verbatim from github.com/azimjohn/jprq at
// server/events/events.go, upstream commit 3c10e25 (short SHA of
// HEAD in the read-only clone at ~/programming/jprq at the time of
// copying), licensed under the MIT License (Copyright (c) 2020 Azimjon
// Pulatov; see the "Create LICENSE" commit in that repository's history —
// the LICENSE file itself is absent from the current checkout).
//
// Only the package clause was changed, from `events` to `jprq`. Every
// other line, including the known bug in WriteError where
// fmt.Sprintf(message, args) passes the variadic []string as a single
// operand instead of expanding it, is preserved intentionally: our
// exit-code classification (see exitcodes.go) matches on the literal
// prefix before the format verb, which is correct whether or not
// upstream ever fixes this, per DESIGN.md F6.
//
// This file, together with bind.go, is the wire-protocol contract shared
// with the live jprq server. Do not "fix" or refactor it in place; any
// change here must be re-verified against upstream.
package jprq

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"net"
)

const (
	TCP  string = "tcp"
	HTTP string = "http"
)

type EventType interface {
	TunnelRequested | TunnelOpened | ConnectionReceived
}

type Event[Type EventType] struct {
	Data *Type
}

type TunnelRequested struct {
	Protocol   string
	Subdomain  string
	CanonName  string
	AuthToken  string
	CliVersion string
}

type TunnelOpened struct {
	Hostname      string
	Protocol      string
	PublicServer  uint16
	PrivateServer uint16
	ErrorMessage  string
}

type ConnectionReceived struct {
	ClientIP    net.IP
	ClientPort  uint16
	RateLimited bool
}

func WriteError(eventWriter io.Writer, message string, args ...string) error {
	event := Event[TunnelOpened]{
		Data: &TunnelOpened{
			ErrorMessage: fmt.Sprintf(message, args),
		},
	}
	event.Write(eventWriter)
	return errors.New(event.Data.ErrorMessage)
}

func (e *Event[EventType]) encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(e.Data); err != nil {
		return nil, err
	}
	data := buf.Bytes()
	return data, nil
}

func (e *Event[EventType]) decode(data []byte) error {
	buf := bytes.NewBuffer(data)
	dec := gob.NewDecoder(buf)
	err := dec.Decode(&e.Data)
	return err
}

func (e *Event[EventType]) Read(conn io.Reader) error {
	buffer := make([]byte, 2)
	if _, err := conn.Read(buffer); err != nil {
		return err
	}
	length := binary.LittleEndian.Uint16(buffer)
	buffer = make([]byte, length)
	if _, err := conn.Read(buffer); err != nil {
		return err
	}
	err := e.decode(buffer)
	return err
}

func (e *Event[EventType]) Write(conn io.Writer) error {
	data, err := e.encode()
	if err != nil {
		return err
	}
	length := make([]byte, 2)
	binary.LittleEndian.PutUint16(length, uint16(len(data)))
	if _, err := conn.Write(length); err != nil {
		return err
	}
	_, err = conn.Write(data)
	return err
}
