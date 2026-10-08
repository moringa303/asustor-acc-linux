// Test helper: advertise a fake ASUSTOR NAS over mDNS with the same
// service type and TXT schema a real ADM uses.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/grandcat/zeroconf"
)

func main() {
	uninit := flag.Bool("init", false, "advertise a factory-fresh (uninitialized) NAS instead")
	flag.Parse()

	service := "_ASUSTOR_ADM._tcp"
	state := "inited"
	if *uninit {
		service = "_ASUSTOR_INIT._tcp"
		state = "uninited"
	}
	txt := []string{
		"state=" + state,
		"httpenabled=Yes",
		"httpsport=8001",
		"model=AS5304T",
		"version=4.3.3.RC92",
		"admupdate=No",
		"bios=1.0",
		"hostid=00-11-32-aa-bb-cc",
		"serialnumber=AS2025TEST01",
		"wol=Yes",
		"appupdatable=No",
		"hasabnormalvolume=No",
		"ifnames=eth0",
	}
	server, err := zeroconf.Register("AS5304T-TEST", service, "local.", 8000, txt, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Shutdown()
	log.Println("fake NAS advertised, ctrl-c to stop")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
}
