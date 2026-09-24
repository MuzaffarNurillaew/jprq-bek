// Copied verbatim (aside from the package clause) from
// github.com/azimjohn/jprq at server/events/event_test.go, upstream
// commit 3c10e25, licensed under the MIT License (Copyright (c) 2020
// Azimjon Pulatov; see the "Create LICENSE" commit in that repository's
// history).
//
// This test guards gob encoding/decoding compatibility with the live
// jprq server: if a drifted copy of events.go breaks the wire format,
// this test fails in CI instead of in a pod. See DESIGN.md §4.
package jprq

import (
	"io"
	"testing"
)

func TestEvent_EncodeDecode(t *testing.T) {
	event := Event[ConnectionReceived]{
		Data: &ConnectionReceived{
			ClientIP:    []byte{127, 0, 0, 1},
			ClientPort:  8000,
			RateLimited: false,
		},
	}

	data, err := event.encode()
	if err != nil {
		t.Errorf("error encoding %v", err)
	}

	var result Event[ConnectionReceived]
	err = result.decode(data)
	if err != nil {
		t.Errorf("error decoding %v", err)
	}

	if result.Data.ClientIP.String() != event.Data.ClientIP.String() {
		t.Logf("expected %s, got %s", event.Data.ClientIP, result.Data.ClientIP)
		t.Fail()
	}

	if result.Data.ClientPort != event.Data.ClientPort {
		t.Logf("expected %d, got %d", event.Data.ClientPort, result.Data.ClientPort)
		t.Fail()
	}
}

func TestEvent_ReadWrite(t *testing.T) {
	pr, pw := io.Pipe()
	event := Event[ConnectionReceived]{
		Data: &ConnectionReceived{
			ClientIP:    []byte{127, 0, 0, 1},
			ClientPort:  8000,
			RateLimited: false,
		},
	}

	go func() {
		if err := event.Write(pw); err != nil {
			t.Errorf("error writing: %v", err)
		}
	}()

	var result Event[ConnectionReceived]
	if err := result.Read(pr); err != nil {
		t.Errorf("error reading: %v", err)
	}

	if result.Data.ClientIP.String() != event.Data.ClientIP.String() {
		t.Logf("expected %s, got %s", event.Data.ClientIP, result.Data.ClientIP)
		t.Fail()
	}

	if result.Data.ClientPort != event.Data.ClientPort {
		t.Logf("expected %d, got %d", event.Data.ClientPort, result.Data.ClientPort)
		t.Fail()
	}
}
