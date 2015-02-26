package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

//
func main() {
	handle(ClusterStart())
	handle(StatusStart())
	handle(DecisionStart())
	handle(ActionStart())
	// signal Handle
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill, syscall.SIGQUIT)

	// Block until a signal is received.
	for {
		s := <-c
		switch s {
		case syscall.SIGQUIT, os.Kill:
			// kill the database then quit
			log.Info("Signal Recieved: %s", s.String())
			log.Info("Killing Database")
			actions <- "kill"
			// called twice because the first call returns when the job is picked up
			// the second call returns when the first job is complete
			actions <- "kill"
			os.Exit(0)
		case os.Interrupt:
			// demote
			log.Info("Signal Recieved: %s", s.String())
			log.Info("advising a demotion")
			advice <- "demote"
		}
	}


}

//
func handle(err error) {
	if err != nil {
		fmt.Println("error: " + err.Error())
		os.Exit(1)
	}
}
