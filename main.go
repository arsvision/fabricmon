// Copyright 2017-18 Daniel Swarbrick. All rights reserved.
// Use of this source code is governed by a GPL license that can be found in the LICENSE file.

// cgo wrapper around libibumad / libibnetdiscover.
// Note: Due to the usual permissions on /dev/infiniband/umad*, this will probably need to be
// executed as root.

// Package FabricMon is an InfiniBand fabric monitor daemon.
//
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/unix"

	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/dswarbrick/fabricmon/config"
	"github.com/dswarbrick/fabricmon/infiniband"
	"github.com/dswarbrick/fabricmon/version"
	"github.com/dswarbrick/fabricmon/writer"
	"github.com/dswarbrick/fabricmon/writer/forcegraph"
	"github.com/dswarbrick/fabricmon/writer/influxdb"
)

// router duplicates a Fabric struct received via channel and outputs it to multiple receiver
// channels.
func router(input chan infiniband.Fabric, writers []writer.FMWriter) {
	outputs := make([]chan infiniband.Fabric, len(writers))

	// Create output channels for workers, and start worker goroutine
	for i, w := range writers {
		outputs[i] = make(chan infiniband.Fabric)
		go w.Receiver(outputs[i])
	}

	for fabric := range input {
		for _, c := range outputs {
			c <- fabric
		}
	}

	// Close output channels
	for _, c := range outputs {
		close(c)
	}

	log.Println("Router input channel closed. Exiting function.")
}

func main() {
	var (
		configFile = kingpin.Flag("config", "Path to config file.").Default("fabricmon.conf").String()
		jsonDir    = kingpin.Flag("json-dir", "Output directory for JSON topologies.").Default("./").String()
		daemonize  = kingpin.Flag("daemonize", "Run forever, fetching counters periodically.").Default("true").Bool()
	)

	kingpin.Parse()

	conf, err := config.ReadConfig(*configFile)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Initialise umad library (also required in order to run under ibsim).
	// FIXME: Check error status of this.
	infiniband.UmadInit()

	caNames := infiniband.UmadGetCANames()

	if len(caNames) == 0 {
		fmt.Println("No HCAs found in system. Exiting.")
		os.Exit(1)
	}

	log.Println("FabricMon", version.Info())

	infiniband.ScanCAs(caNames)

	// Channel to signal goroutines that we are shutting down.
	shutdownChan := make(chan bool)

	// Setup signal handler to catch SIGINT, SIGTERM.
	sigChan := make(chan os.Signal)
	signal.Notify(sigChan, unix.SIGINT, unix.SIGTERM)
	go func() {
		s := <-sigChan
		log.Printf("Caught signal: %s. Shutting down.", s)
		close(shutdownChan)
	}()

	// Initialize writers slice with just the d3.js ForceGraphWriter
	writers := []writer.FMWriter{&forcegraph.ForceGraphWriter{OutputDir: *jsonDir}}

	// First sweep.
	infiniband.Sweep(nil)

	if *daemonize {
		for _, c := range conf.InfluxDB {
			w := &influxdb.InfluxDBWriter{Config: c}
			writers = append(writers, w)
		}

		// FIXME: Move this outside of daemonize if-block
		splitter := make(chan infiniband.Fabric)
		go router(splitter, writers)

		ticker := time.NewTicker(time.Duration(conf.PollInterval))
		defer ticker.Stop()

	Loop:
		// Loop indefinitely, scanning fabrics every tick.
		for {
			select {
			case <-ticker.C:
				infiniband.Sweep(splitter)
			case <-shutdownChan:
				log.Println("Shutdown received in polling loop.")
				break Loop
			}
		}

		close(splitter)
	}

	log.Println("Cleaning up")

	infiniband.UmadDone()
}
