// Bind is copied from github.com/azimjohn/jprq at
// server/tunnel/tunnel.go (the Bind function only, lines ~123-144),
// upstream commit 3c10e25, licensed under the MIT License (Copyright (c)
// 2020 Azimjon Pulatov; see the "Create LICENSE" commit in that
// repository's history).
//
// Only Bind is extracted here: the rest of tunnel.go imports
// github.com/azimjohn/jprq/server/server, which is server-side
// infrastructure the agent must not depend on. Bind itself is
// self-contained, needing only net, io, and time, so it is copied
// byte-for-byte rather than reimplemented.
package jprq

import (
	"io"
	"net"
	"time"
)

func Bind(src net.Conn, dst net.Conn, debug io.Writer) error {
	defer src.Close()
	defer dst.Close()
	buf := make([]byte, 4096)
	for {
		_ = src.SetReadDeadline(time.Now().Add(time.Second))
		n, err := src.Read(buf)
		if err == io.EOF {
			break
		}
		_ = dst.SetWriteDeadline(time.Now().Add(time.Second))
		_, err = dst.Write(buf[:n])
		if err != nil {
			return err
		}
		if debug != nil {
			debug.Write(buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}
