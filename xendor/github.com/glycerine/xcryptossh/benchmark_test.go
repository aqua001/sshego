// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssh

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
)

type server struct {
	*ServerConn
	chans <-chan NewChannel
}

func newServer(ctx context.Context, c net.Conn, conf *ServerConfig) (*server, error) {
	sconn, chans, reqs, err := NewServerConn(ctx, c, conf)
	if err != nil {
		return nil, err
	}
	go DiscardRequests(ctx, reqs, conf.Halt)
	return &server{sconn, chans}, nil
}

func (s *server) Accept() (NewChannel, error) {
	n, ok := <-s.chans
	if !ok {
		return nil, io.EOF
	}
	return n, nil
}

func sshPipe(halt *Halter) (Conn, *server, error) {
	c1, c2, err := netPipe()
	if err != nil {
		return nil, nil, err
	}
	ctx := context.Background()
	clientConf := ClientConfig{
		User:            "user",
		HostKeyCallback: InsecureIgnoreHostKey(),
		Config: Config{
			Halt: halt,
		},
	}
	serverConf := ServerConfig{
		NoClientAuth: true,
		Config: Config{
			Halt: halt,
		},
	}
	serverConf.AddHostKey(testSigners["ecdsa"])
	done := make(chan *server, 1)
	go func() {
		server, err := newServer(ctx, c2, &serverConf)
		if err != nil {
			done <- nil
		}
		done <- server
	}()

	client, _, reqs, err := NewClientConn(ctx, c1, "", &clientConf)
	if err != nil {
		return nil, nil, err
	}
	server := <-done
	if server == nil {
		return nil, nil, errors.New("server handshake failed.")
	}
	go DiscardRequests(ctx, reqs, nil)

	return client, server, nil
}

func BenchmarkEndToEnd(b *testing.B) {
	b.StopTimer()
	halt := NewHalter()
	defer halt.RequestStop()

	ctx := context.Background()

	client, server, err := sshPipe(halt)
	if err != nil {
		b.Fatalf("sshPipe: %v", err)
	}

	defer client.Close()
	defer server.Close()

	size := (1 << 20)
	input := make([]byte, size)
	output := make([]byte, size)
	b.SetBytes(int64(size))
	done := make(chan int, 1)

	go func() {
		newCh, err := server.Accept()
		if err != nil {
			b.Fatalf("Client: %v", err)
		}
		ch, incoming, err := newCh.Accept()
		go DiscardRequests(ctx, incoming, nil)
		for i := 0; i < b.N; i++ {
			if _, err := io.ReadFull(ch, output); err != nil {
				b.Fatalf("ReadFull: %v", err)
			}
		}
		ch.Close()
		done <- 1
	}()

	ch, in, err := client.OpenChannel(ctx, "speed", nil, nil)
	if err != nil {
		b.Fatalf("OpenChannel: %v", err)
	}
	go DiscardRequests(ctx, in, nil)

	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ch.Write(input); err != nil {
			b.Fatalf("WriteFull: %v", err)
		}
	}
	ch.Close()
	b.StopTimer()

	<-done
}
