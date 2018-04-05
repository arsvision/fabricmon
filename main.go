// Copyright 2017-18 Daniel Swarbrick. All rights reserved.
// Use of this source code is governed by a GPL license that can be found in the LICENSE file.

// cgo wrapper around libibumad / libibnetdiscover.
// Note: Due to the usual permissions on /dev/infiniband/umad*, this will probably need to be
// executed as root.
//
// TODO: Implement user-friendly display of link / speed / rate etc. (see ib_types.h).

// Package FabricMon is an InfiniBand fabric monitor daemon.
//
package main

// #cgo CFLAGS: -I/usr/include/infiniband
// #cgo LDFLAGS: -libmad -libumad -libnetdisc
// #include <stdlib.h>
// #include <umad.h>
// #include <ibnetdisc.h>
import "C"

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/dswarbrick/fabricmon/version"
)

const (
	PMA_TIMEOUT = 0

	SMINFO_NOTACT uint8 = iota
	SMINFO_DISCOVER
	SMINFO_STANDBY
	SMINFO_MASTER
)

var smStateMap = [...]string{
	"SMINFO_NOTACT",
	"SMINFO_DISCOVER",
	"SMINFO_STANDBY",
	"SMINFO_MASTER",
}

type Fabric struct {
	mutex      sync.RWMutex
	ibndFabric *C.struct_ibnd_fabric
	ibmadPort  *C.struct_ibmad_port
	topology   d3Topology
}

// FabricMap is a two-dimensional map holding the Fabric struct for each HCA / port pair.
type FabricMap map[string]map[int]*Fabric

type Node struct {
	guid     uint64
	nodeType int
	nodeDesc string
	ports    []Port
}

type Port struct {
	guid       uint64
	remoteGuid uint64
	counters   map[uint32]interface{}
}

// Standard (32-bit) counters and their display names
// TODO: Implement warnings and / or automatically reset counters when they are close to reaching
// 	     their maximum permissible value (according to IBTA spec).
var stdCounterMap = map[uint32]string{
	C.IB_PC_ERR_SYM_F:        "SymbolErrorCounter",
	C.IB_PC_LINK_RECOVERS_F:  "LinkErrorRecoveryCounter",
	C.IB_PC_LINK_DOWNED_F:    "LinkDownedCounter",
	C.IB_PC_ERR_RCV_F:        "PortRcvErrors",
	C.IB_PC_ERR_PHYSRCV_F:    "PortRcvRemotePhysicalErrors",
	C.IB_PC_ERR_SWITCH_REL_F: "PortRcvSwitchRelayErrors",
	C.IB_PC_XMT_DISCARDS_F:   "PortXmitDiscards",
	C.IB_PC_ERR_XMTCONSTR_F:  "PortXmitConstraintErrors",
	C.IB_PC_ERR_RCVCONSTR_F:  "PortRcvConstraintErrors",
	C.IB_PC_ERR_LOCALINTEG_F: "LocalLinkIntegrityErrors",
	C.IB_PC_ERR_EXCESS_OVR_F: "ExcessiveBufferOverrunErrors",
	C.IB_PC_VL15_DROPPED_F:   "VL15Dropped",
	C.IB_PC_XMT_WAIT_F:       "PortXmitWait", // Requires cap mask IB_PM_PC_XMIT_WAIT_SUP
}

// Extended (64-bit) counters and their display names.
var extCounterMap = map[uint32]string{
	C.IB_PC_EXT_XMT_BYTES_F: "PortXmitData",
	C.IB_PC_EXT_RCV_BYTES_F: "PortRcvData",
	C.IB_PC_EXT_XMT_PKTS_F:  "PortXmitPkts",
	C.IB_PC_EXT_RCV_PKTS_F:  "PortRcvPkts",
	C.IB_PC_EXT_XMT_UPKTS_F: "PortUnicastXmitPkts",
	C.IB_PC_EXT_RCV_UPKTS_F: "PortUnicastRcvPkts",
	C.IB_PC_EXT_XMT_MPKTS_F: "PortMulticastXmitPkts",
	C.IB_PC_EXT_RCV_MPKTS_F: "PortMulticastRcvPkts",
}

var portStates = [...]string{
	"No state change", // Valid only on Set() port state
	"Down",            // Includes failed links
	"Initialize",
	"Armed",
	"Active",
}

var portPhysStates = [...]string{
	"No state change", // Valid only on Set() port state
	"Sleep",
	"Polling",
	"Disabled",
	"PortConfigurationTraining",
	"LinkUp",
	"LinkErrorRecovery",
	"Phy Test",
}

var nnMap NodeNameMap

// getCounterUint32 decodes the specified counter from the supplied buffer and returns the uint32
// counter value.
func getCounterUint32(buf *C.uint8_t, counter uint32) (v uint32) {
	C.mad_decode_field(buf, counter, unsafe.Pointer(&v))
	return v
}

// getCounterUint64 decodes the specified counter from the supplied buffer and returns the uint64
// counter value.
func getCounterUint64(buf *C.uint8_t, counter uint32) (v uint64) {
	C.mad_decode_field(buf, counter, unsafe.Pointer(&v))
	return v
}

// getPortCounters retrieves all counters for a specific port.
func getPortCounters(portId *C.ib_portid_t, portNum int, ibmadPort *C.struct_ibmad_port) (map[uint32]interface{}, error) {
	var buf [1024]byte

	counters := make(map[uint32]interface{})

	// PerfMgt ClassPortInfo is a required attribute
	pmaBuf := C.pma_query_via(unsafe.Pointer(&buf), portId, C.int(portNum), PMA_TIMEOUT, C.CLASS_PORT_INFO, ibmadPort)

	if pmaBuf == nil {
		return counters, fmt.Errorf("ERROR: Port %d CLASS_PORT_INFO query failed!", portNum)
	}

	capMask := nativeEndian.Uint16(buf[2:4])
	log.Printf("Port %d Cap Mask: %#02x\n", portNum, ntohs(capMask))

	// Note: In PortCounters, PortCountersExtended, PortXmitDataSL, and PortRcvDataSL, components
	// that represent Data (e.g. PortXmitData and PortRcvData) indicate octets divided by 4 rather
	// than just octets.

	// Fetch standard (32 bit, some 16 bit) counters
	pmaBuf = C.pma_query_via(unsafe.Pointer(&buf), portId, C.int(portNum), PMA_TIMEOUT, C.IB_GSI_PORT_COUNTERS, ibmadPort)

	if pmaBuf != nil {
		// Iterate over standard counters
		for counter, _ := range stdCounterMap {
			if (counter == C.IB_PC_XMT_WAIT_F) && (capMask&C.IB_PM_PC_XMIT_WAIT_SUP == 0) {
				continue // Counter not supported
			}

			counters[counter] = getCounterUint32(pmaBuf, counter)
		}
	}

	if (capMask&C.IB_PM_EXT_WIDTH_SUPPORTED == 0) && (capMask&C.IB_PM_EXT_WIDTH_NOIETF_SUP == 0) {
		// TODO: Fetch standard data / packet counters if extended counters are not supported
		// (unlikely).
		log.Printf("NOTICE: Port %d does not support extended counters", portNum)
		return counters, nil
	}

	// Fetch extended (64 bit) counters
	pmaBuf = C.pma_query_via(unsafe.Pointer(&buf), portId, C.int(portNum), PMA_TIMEOUT, C.IB_GSI_PORT_COUNTERS_EXT, ibmadPort)

	if pmaBuf != nil {
		for counter, _ := range extCounterMap {
			counters[counter] = getCounterUint64(pmaBuf, counter)
		}
	}

	return counters, nil
}

func umadGetCANames() []string {
	var (
		buf  [C.UMAD_CA_NAME_LEN][C.UMAD_MAX_DEVICES]byte
		hcas = make([]string, 0, C.UMAD_MAX_DEVICES)
	)

	// Call umad_get_cas_names with pointer to first element in our buffer
	numHCAs := C.umad_get_cas_names((*[C.UMAD_CA_NAME_LEN]C.char)(unsafe.Pointer(&buf[0])), C.UMAD_MAX_DEVICES)

	for x := 0; x < int(numHCAs); x++ {
		hcas = append(hcas, strings.TrimRight(string(buf[x][:]), "\x00"))
	}

	return hcas
}

// smInfo is a proof of concept function to get the SM info for a CA & port.
func smInfo(caName string, portNum int) {
	var (
		sminfo [1024]C.uint8_t
		guid   uint64
		act    uint16
	)

	mgmt_classes := [3]C.int{C.IB_SMI_CLASS, C.IB_SMI_DIRECT_CLASS, C.IB_SA_CLASS}

	ibd_ca := C.CString(caName)
	defer C.free(unsafe.Pointer(ibd_ca))

	ibd_ca_port := C.int(portNum)

	// struct ibmad_port *mad_rpc_open_port(char *dev_name, int dev_port, int *mgmt_classes, int num_classes)
	srcport := C.mad_rpc_open_port(ibd_ca, ibd_ca_port, &mgmt_classes[0], 3)

	prio := SMINFO_STANDBY
	state := SMINFO_STANDBY

	var portid C.ib_portid_t

	//C.resolve_sm_portid(ibd_ca, ibd_ca_port, &portid)
	var port C.umad_port_t

	C.umad_get_port(ibd_ca, ibd_ca_port, &port)
	portid.lid = C.int(port.sm_lid)
	portid.sl = C.uchar(port.sm_sl)
	C.umad_release_port(&port)

	C.mad_encode_field(&sminfo[0], C.IB_SMINFO_PRIO_F, unsafe.Pointer(&prio))
	C.mad_encode_field(&sminfo[0], C.IB_SMINFO_STATE_F, unsafe.Pointer(&state))

	C.smp_query_via(unsafe.Pointer(&sminfo), &portid, C.IB_ATTR_SMINFO, 0, 0, srcport)

	C.mad_decode_field(&sminfo[0], C.IB_SMINFO_GUID_F, unsafe.Pointer(&guid))
	C.mad_decode_field(&sminfo[0], C.IB_SMINFO_ACT_F, unsafe.Pointer(&act))
	C.mad_decode_field(&sminfo[0], C.IB_SMINFO_PRIO_F, unsafe.Pointer(&prio))
	C.mad_decode_field(&sminfo[0], C.IB_SMINFO_STATE_F, unsafe.Pointer(&state))

	fmt.Printf("sminfo: sm lid %d sm guid %#16x, activity count %d priority %d state %d %s\n",
		portid.lid, guid, act, prio, state, smStateMap[state])
}

func walkPorts(node *C.struct_ibnd_node, mad_port *C.struct_ibmad_port) []Port {
	var portid C.ib_portid_t

	log.Printf("Node type: %d, node descr: %s, num. ports: %d, node GUID: %#016x\n",
		node._type, nnMap.remapNodeName(uint64(node.guid), C.GoString(&node.nodedesc[0])),
		node.numports, node.guid)

	ports := make([]Port, node.numports+1)

	C.ib_portid_set(&portid, C.int(node.smalid), 0, 0)

	// node.ports is an array of ports, indexed by port number:
	//   ports[1] == port 1,
	//   ports[2] == port 2,
	//   etc...
	// Any port in the array MAY BE NIL! Most notably, non-switches have no port zero, therefore
	// ports[0] == nil for those nodes!
	arrayPtr := uintptr(unsafe.Pointer(node.ports))

	for portNum := 0; portNum <= int(node.numports); portNum++ {
		// Get pointer to port struct and increment arrayPtr to next pointer.
		pp := *(**C.ibnd_port_t)(unsafe.Pointer(arrayPtr))
		arrayPtr += unsafe.Sizeof(arrayPtr)

		myPort := Port{guid: uint64(pp.guid)}

		portState := C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_STATE_F)
		physState := C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_PHYS_STATE_F)

		// TODO: Decode EXT_PORT_LINK_SPEED (i.e., FDR10).
		linkWidth := C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_LINK_WIDTH_ACTIVE_F)
		linkSpeed := C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_LINK_SPEED_ACTIVE_F)

		log.Printf("Port %d, port state: %d, phys state: %d, link width: %d, link speed: %d\n",
			portNum, portState, physState, linkWidth, linkSpeed)

		// Remote port may be nil if port state is polling / armed.
		rp := pp.remoteport

		if rp != nil {
			log.Printf("Remote node type: %d, GUID: %#016x, descr: %s\n",
				rp.node._type, rp.node.guid,
				nnMap.remapNodeName(uint64(rp.node.guid), C.GoString(&rp.node.nodedesc[0])))

			myPort.remoteGuid = uint64(rp.node.guid)

			// Port counters will only be fetched if port is ACTIVE + LINKUP
			if (portState == C.IB_LINK_ACTIVE) && (physState == C.IB_PORT_PHYS_STATE_LINKUP) {
				// Determine max width supported by both ends
				maxWidth := uint(1 << log2b(uint(
					C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_LINK_WIDTH_SUPPORTED_F)&
						C.mad_get_field(unsafe.Pointer(&rp.info), 0, C.IB_PORT_LINK_WIDTH_SUPPORTED_F))))
				if uint(linkWidth) != maxWidth {
					log.Printf("NOTICE: Port %d link width is not the max width supported by both ports",
						portNum)
				}

				// Determine max speed supported by both ends
				maxSpeed := uint(1 << log2b(uint(
					C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_LINK_SPEED_SUPPORTED_F)&
						C.mad_get_field(unsafe.Pointer(&rp.info), 0, C.IB_PORT_LINK_SPEED_SUPPORTED_F))))
				if uint(linkSpeed) != maxSpeed {
					log.Printf("NOTICE: Port %d link speed is not the max speed supported by both ports",
						portNum)
				}

				if counters, err := getPortCounters(&portid, portNum, mad_port); err == nil {
					myPort.counters = counters
				} else {
					log.Printf("ERROR: Cannot get counters for port %d: %s\n", portNum, err)
				}
			}
		}

		ports[portNum] = myPort
	}

	return ports
}

func walkFabric(fabric *C.struct_ibnd_fabric, mad_port *C.struct_ibmad_port) []Node {
	nodes := make([]Node, 0)

	for node := fabric.nodes; node != nil; node = node.next {
		myNode := Node{
			guid:     uint64(node.guid),
			nodeType: int(node._type),
			nodeDesc: C.GoString(&node.nodedesc[0]),
		}

		log.Printf("node: %#v\n", myNode)

		if node._type == C.IB_NODE_SWITCH {
			myNode.ports = walkPorts(node, mad_port)
		}

		nodes = append(nodes, myNode)
	}

	return nodes
}

func caDiscoverFabric(ca C.umad_ca_t, outputDir string) {
	caName := C.GoString(&ca.ca_name[0])

	mgmt_classes := [3]C.int{C.IB_SMI_CLASS, C.IB_SA_CLASS, C.IB_PERFORMANCE_CLASS}

	// Iterate over CA's umad_port array
	for _, umad_port := range ca.ports {
		// ca.ports may contain noncontiguous umad_port pointers
		if umad_port == nil {
			continue
		}

		portNum := int(umad_port.portnum)
		log.Printf("Polling %s port %d", caName, portNum)

		// ibnd_config_t specifies max hops, timeout, max SMPs etc
		var config C.ibnd_config_t

		// NOTE: Under ibsim, this will fail after a certain number of iterations with a
		// mad_rpc_open_port() errors (presumably due to a resource leak in ibsim).
		// ibnd_fabric_t *ibnd_discover_fabric(char *ca_name, int ca_port, ib_portid_t *from, ibnd_config_t *config)
		fabric, err := C.ibnd_discover_fabric(&ca.ca_name[0], umad_port.portnum, nil, &config)

		if err != nil {
			log.Println("Unable to discover fabric:", err)
			continue
		}

		// Open MAD port, which is needed for getting port counters.
		// struct ibmad_port *mad_rpc_open_port(char *dev_name, int dev_port, int *mgmt_classes, int num_classes)
		mad_port := C.mad_rpc_open_port(&ca.ca_name[0], umad_port.portnum, &mgmt_classes[0], C.int(len(mgmt_classes)))

		if mad_port != nil {
			nodes := walkFabric(fabric, mad_port)
			C.mad_rpc_close_port(mad_port)

			if outputDir != "" {
				hostname, _ := os.Hostname()
				filename := fmt.Sprintf("%s-%s-p%d.json", hostname, caName, portNum)

				writeD3JSON(path.Join(outputDir, filename), nodes)
			}
		} else {
			log.Printf("ERROR: Unable to open MAD port: %s: %d", caName, portNum)
		}

		C.ibnd_destroy_fabric(fabric)
	}
}

func main() {
	var (
		configFile = kingpin.Flag("config", "Path to config file.").Default("fabricmon.conf").String()
	)

	kingpin.Parse()

	conf, err := readConfig(*configFile)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Initialise umad library (also required in order to run under ibsim)
	// NOTE: ibsim indicates that FabricMon is not "disconnecting" when it exits - resource leak?
	if C.umad_init() < 0 {
		fmt.Println("Error initialising umad library. Exiting.")
		os.Exit(1)
	}

	caNames := umadGetCANames()

	if len(caNames) == 0 {
		fmt.Println("No HCAs found in system. Exiting.")
		os.Exit(1)
	}

	log.Println("FabricMon", version.Info())

	nnMap, _ = NewNodeNameMap()

	// umad_ca_t contains an array of pointers - associated memory must be freed with
	// umad_release_ca(umad_ca_t *ca)
	umad_ca_list := make([]C.umad_ca_t, len(caNames))

	for i, caName := range caNames {
		var ca C.umad_ca_t

		ca_name := C.CString(caName)
		C.umad_get_ca(ca_name, &ca)
		C.free(unsafe.Pointer(ca_name))

		log.Printf("Found CA %s (%s) with %d ports, firmware version: %s, hardware version: %s, "+
			"node GUID: %#016x, system GUID: %#016x\n",
			C.GoString(&ca.ca_name[0]), C.GoString(&ca.ca_type[0]), ca.numports,
			C.GoString(&ca.fw_ver[0]), C.GoString(&ca.hw_ver[0]),
			ntohll(uint64(ca.node_guid)), ntohll(uint64(ca.system_guid)))

		umad_ca_list[i] = ca
	}

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

	// First sweep.
	for _, ca := range umad_ca_list {
		caDiscoverFabric(ca, ".")
	}

	ticker := time.NewTicker(time.Duration(conf.PollInterval))
	defer ticker.Stop()

	// Loop indefinitely, scanning fabrics every tick.
	for {
		select {
		case <-ticker.C:
			for _, ca := range umad_ca_list {
				caDiscoverFabric(ca, "")
			}
		case <-shutdownChan:
			log.Println("Shutdown received in polling loop.")

			// Free associated memory from pointers in umad_ca_t.ports
			for _, ca := range umad_ca_list {
				C.umad_release_ca(&ca)
			}

			C.umad_done()
			os.Exit(1)
		}
	}

	// TODO: Re-enable these
	// writeInfluxDB(nodes, conf.InfluxDB, caName, portNum)
	// makeD3(nodes)

	// Start HTTP server to serve JSON for d3.js (WIP)
	// serve(conf.BindAddress, fabrics)
}
