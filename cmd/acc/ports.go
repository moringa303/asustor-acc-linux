package main

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// `acc ports`: which of the services a NAS commonly runs are reachable
// from this computer. It is a plain TCP connect to each port of the one
// NAS you name, on your own LAN, and nothing is sent over the connection;
// it is a reachability check, not a scanner.

type knownPort struct {
	Port    int    `json:"port"`
	Service string `json:"service"`
}

// knownPorts are the services ADM and its apps listen on.
var knownPorts = []knownPort{
	{21, "FTP"}, {22, "SSH / SFTP"}, {80, "HTTP"}, {139, "SMB (NetBIOS)"},
	{443, "HTTPS"}, {445, "SMB"}, {548, "AFP"}, {873, "rsync"},
	{2049, "NFS"}, {3260, "iSCSI"}, {3306, "MariaDB"}, {8000, "ADM"},
	{8001, "ADM (HTTPS)"}, {8080, "Web server"}, {8443, "Web server (HTTPS)"},
	{9800, "WebDAV"}, {9801, "WebDAV (HTTPS)"}, {32400, "Plex"},
}

type portResult struct {
	Port    int    `json:"port"`
	Service string `json:"service"`
	// open: a service accepted the connection; closed: the NAS refused
	// it (nothing listens); filtered: no answer within the timeout (a
	// firewall drops it, or the host is unreachable).
	State string `json:"state"`
}

// portTimeout is how long one connection attempt may take.
const portTimeout = 1500 * time.Millisecond

// checkPorts tries every port of host at once and returns the results in
// the order of ports.
func checkPorts(host string, ports []knownPort, timeout time.Duration) []portResult {
	results := make([]portResult, len(ports))
	var wg sync.WaitGroup
	for i, p := range ports {
		wg.Add(1)
		go func(i int, p knownPort) {
			defer wg.Done()
			results[i] = portResult{Port: p.Port, Service: p.Service, State: portState(host, p.Port, timeout)}
		}(i, p)
	}
	wg.Wait()
	return results
}

func portState(host string, port int, timeout time.Duration) string {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err == nil {
		conn.Close()
		return "open"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "closed"
	}
	return "filtered"
}
