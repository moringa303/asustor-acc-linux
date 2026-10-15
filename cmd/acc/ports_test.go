package main

import (
	"net"
	"testing"
	"time"
)

func TestCheckPorts(t *testing.T) {
	// open: something listens
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	open := ln.Addr().(*net.TCPAddr).Port

	// closed: a port that was just released
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()

	ports := []knownPort{{open, "open one"}, {closed, "closed one"}}
	got := checkPorts("127.0.0.1", ports, portTimeout)
	if len(got) != 2 || got[0].State != "open" || got[1].State != "closed" {
		t.Fatalf("checkPorts = %+v", got)
	}
	if got[0].Port != open || got[0].Service != "open one" {
		t.Errorf("result keeps port and service: %+v", got[0])
	}

	// A host that never answers (TEST-NET-1 is not routed) ends with the
	// timeout as "filtered"; a sandbox that refuses the route at once
	// reports "closed" instead, so only the timing is checked.
	start := time.Now()
	got = checkPorts("192.0.2.1", []knownPort{{80, "HTTP"}}, 100*time.Millisecond)
	if got[0].State == "open" {
		t.Errorf("unroutable host: state %q", got[0].State)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the timeout was not honoured: %s", time.Since(start))
	}
}
