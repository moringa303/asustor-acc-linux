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
	name := flag.String("name", "AS5304T-TEST", "instance name of the fake NAS")
	model := flag.String("model", "AS5304T", "model reported in the TXT record")
	mac := flag.String("mac", "00-11-32-aa-bb-cc", "MAC reported as hostid (dash-separated)")
	port := flag.Int("port", 8000, "ADM web port announced in the SRV record")
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
		"model=" + *model,
		"version=4.3.3.RC92",
		"admupdate=No",
		"bios=1.0",
		"hostid=" + *mac,
		"serialnumber=AS2025TEST01",
		"wol=Yes",
		"appupdatable=No",
		"hasabnormalvolume=No",
		"ifnames=eth0",
	}
	server, err := zeroconf.Register(*name, service, "local.", *port, txt, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Shutdown()
	log.Printf("fake NAS %q advertised as %s on port %d, ctrl-c to stop", *name, service, *port)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
}
