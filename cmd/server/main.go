package main

import (
	"flag"
	"fmt"
	"gameframework/pkg/server"
)

func main() {
	mode := flag.String("mode", "server", "server or client")
	addr := flag.String("addr", ":30000", "server listen addr (server)")
	tickHz := flag.Int("hz", 20, "tick hz")
	flag.Parse()

	if *mode != "server" {
		fmt.Println("usage: -mode=server")
		return
	}
	srv, err := server.NewServer(*addr, *tickHz)
	if err != nil {
		panic(err)
	}
	fmt.Println("Server listen", *addr)
	go srv.ListenLoop()
	go srv.ReliableRetransmitLoop()
	srv.BroadcastLoop()
}
