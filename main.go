/*
 * FabricMon - an InfiniBand fabric monitor daemon.
 * Copyright 2017 Daniel Swarbrick
 *
 * cgo wrapper around libibumad / libibnetdiscover
 * Note: Due to the usual permissions on /dev/infiniband/umad*, this will probably need to be
 * executed as root.
 *
 * TODO: Implement user-friendly display of link / speed / rate etc. (see ib_types.h)
 */
package main

// #cgo CFLAGS: -I/usr/include/infiniband
// #cgo LDFLAGS: -libmad -libumad -libnetdisc
// #include <stdlib.h>
// #include <umad.h>
// #include <ibnetdisc.h>
import "C"

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	influxdb "github.com/influxdata/influxdb/client/v2"
)

const PMA_TIMEOUT = 0

type Fabric struct {
	mutex      sync.RWMutex
	ibndFabric *C.struct_ibnd_fabric
	ibmadPort  *C.struct_ibmad_port
	topology   d3Topology
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

// Extended (64-bit) counters and their display names
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

// getCounterUint32 decodes the specified counter from the supplied buffer and returns the uint32
// counter value
func getCounterUint32(buf *C.uint8_t, counter uint32) (v uint32) {
	C.mad_decode_field(buf, counter, unsafe.Pointer(&v))
	return v
}

// getCounterUint64 decodes the specified counter from the supplied buffer and returns the uint64
// counter value
func getCounterUint64(buf *C.uint8_t, counter uint32) (v uint64) {
	C.mad_decode_field(buf, counter, unsafe.Pointer(&v))
	return v
}

// iterateSwitches walks the null-terminated node linked-list in f.nodes, displaying only swtich
// nodes
func iterateSwitches(f *Fabric, nnMap *NodeNameMap, conf influxdbConf) {
	// Batch to hold InfluxDB points
	batch, err := influxdb.NewBatchPoints(influxdb.BatchPointsConfig{
		Database:  conf.Database,
		Precision: "s",
	})
	if err != nil {
		return
	}

	hostname, _ := os.Hostname()
	tags := map[string]string{"host": hostname}
	fields := map[string]interface{}{}
	now := time.Now()

	f.mutex.Lock()
	defer f.mutex.Unlock()

	for node := f.ibndFabric.nodes; node != nil; node = node.next {
		d3n := d3Node{
			Id:       fmt.Sprintf("%016x", node.guid),
			NodeType: int(node._type),
			Desc:     nnMap.remapNodeName(uint64(node.guid), C.GoString(&node.nodedesc[0])),
		}

		f.topology.Nodes = append(f.topology.Nodes, d3n)

		if node._type == C.IB_NODE_SWITCH {
			var portid C.ib_portid_t

			fmt.Printf("Node type: %d, node descr: %s, num. ports: %d, node GUID: %#016x\n\n",
				node._type, nnMap.remapNodeName(uint64(node.guid), C.GoString(&node.nodedesc[0])),
				node.numports, node.guid)

			tags["guid"] = fmt.Sprintf("%016x", node.guid)

			C.ib_portid_set(&portid, C.int(node.smalid), 0, 0)

			// node.ports is an array of pointers, in which any port may be null. We use pointer
			// arithmetic to get pointer to port struct.
			arrayPtr := uintptr(unsafe.Pointer(node.ports))

			for portNum := 0; portNum <= int(node.numports); portNum++ {
				// Get pointer to port struct and increment arrayPtr to next pointer
				pp := *(**C.ibnd_port_t)(unsafe.Pointer(arrayPtr))
				arrayPtr += unsafe.Sizeof(arrayPtr)

				portState := C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_STATE_F)
				physState := C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_PHYS_STATE_F)

				// TODO: Decode EXT_PORT_LINK_SPEED (i.e., FDR10)
				linkWidth := C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_LINK_WIDTH_ACTIVE_F)
				linkSpeed := C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_LINK_SPEED_ACTIVE_F)

				fmt.Printf("Port %d, port state: %d, phys state: %d, link width: %d, link speed: %d\n",
					portNum, portState, physState, linkWidth, linkSpeed)

				// TODO: Rework portState checking to optionally decode counters regardless of portState
				if portState != C.IB_LINK_DOWN {
					var buf [1024]byte

					fmt.Printf("port %#v\n", pp)
					tags["port"] = fmt.Sprintf("%d", portNum)

					// This should not be nil if the link is up, but check anyway
					// FIXME: portState may be polling / armed etc, and rp will be null!
					rp := pp.remoteport
					if rp != nil {
						fmt.Printf("Remote node type: %d, GUID: %#016x, descr: %s\n",
							rp.node._type, rp.node.guid,
							nnMap.remapNodeName(uint64(node.guid), C.GoString(&rp.node.nodedesc[0])))

						// Determine max width supported by both ends
						maxWidth := uint(1 << log2b(uint(C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_LINK_WIDTH_SUPPORTED_F)&
							C.mad_get_field(unsafe.Pointer(&rp.info), 0, C.IB_PORT_LINK_WIDTH_SUPPORTED_F))))
						if uint(linkWidth) != maxWidth {
							fmt.Println("NOTICE: Link width is not the max width supported by both ports")
						}

						// Determine max speed supported by both ends
						maxSpeed := uint(1 << log2b(uint(C.mad_get_field(unsafe.Pointer(&pp.info), 0, C.IB_PORT_LINK_SPEED_SUPPORTED_F)&
							C.mad_get_field(unsafe.Pointer(&rp.info), 0, C.IB_PORT_LINK_SPEED_SUPPORTED_F))))
						if uint(linkSpeed) != maxSpeed {
							fmt.Println("NOTICE: Link speed is not the max speed supported by both ports")
						}

						f.topology.Links = append(f.topology.Links, d3Link{fmt.Sprintf("%016x", node.guid), fmt.Sprintf("%016x", rp.node.guid)})
					}

					// PerfMgt ClassPortInfo is a required attribute
					pmaBuf := C.pma_query_via(unsafe.Pointer(&buf), &portid, C.int(portNum), PMA_TIMEOUT, C.CLASS_PORT_INFO, f.ibmadPort)

					if pmaBuf == nil {
						fmt.Printf("ERROR: CLASS_PORT_INFO query failed!")
						continue
					}

					capMask := nativeEndian.Uint16(buf[2:4])
					fmt.Printf("Cap Mask: %#02x\n", ntohs(capMask))

					// Note: In PortCounters, PortCountersExtended, PortXmitDataSL, and
					// PortRcvDataSL, components that represent Data (e.g. PortXmitData and
					// PortRcvData) indicate octets divided by 4 rather than just octets.

					// Fetch standard (32 bit, some 16 bit) counters
					pmaBuf = C.pma_query_via(unsafe.Pointer(&buf), &portid, C.int(portNum), PMA_TIMEOUT, C.IB_GSI_PORT_COUNTERS, f.ibmadPort)

					if pmaBuf != nil {
						// Iterate over standard counters
						for counter, displayName := range stdCounterMap {
							if (counter == C.IB_PC_XMT_WAIT_F) && (capMask&C.IB_PM_PC_XMIT_WAIT_SUP == 0) {
								continue // Counter not supported
							}

							tags["counter"] = displayName
							fields["value"] = getCounterUint32(pmaBuf, counter)
							if point, err := influxdb.NewPoint("fabricmon_counters", tags, fields, now); err == nil {
								batch.AddPoint(point)
							}

							fmt.Printf("%s => %d\n", displayName, getCounterUint32(pmaBuf, counter))
						}
					}

					if (capMask&C.IB_PM_EXT_WIDTH_SUPPORTED == 0) && (capMask&C.IB_PM_EXT_WIDTH_NOIETF_SUP == 0) {
						// TODO: Fetch standard data / packet counters if extended counters are not
						// supported (unlikely)
						fmt.Println("No extended counter support indicated")
						continue
					}

					// Fetch extended (64 bit) counters
					pmaBuf = C.pma_query_via(unsafe.Pointer(&buf), &portid, C.int(portNum), PMA_TIMEOUT, C.IB_GSI_PORT_COUNTERS_EXT, f.ibmadPort)

					if pmaBuf != nil {
						for counter, displayName := range extCounterMap {
							tags["counter"] = displayName
							fields["value"] = getCounterUint32(pmaBuf, counter)
							if point, err := influxdb.NewPoint("fabricmon_counters", tags, fields, now); err == nil {
								batch.AddPoint(point)
							}

							fmt.Printf("%s => %d\n", displayName, getCounterUint64(pmaBuf, counter))
						}
					}
				}
			}
		}
	}

	fmt.Printf("InfluxDB batch contains %d points\n", len(batch.Points()))
	writeBatch(conf, batch)
}

func main() {
	confFile := flag.String("conf", "fabricmon.conf", "Path to config file")
	flag.Parse()

	conf, err := readConfig(*confFile)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	caNames, _ := getCANames()
	nnMap, _ := NewNodeNameMap()

	for _, caName := range caNames {
		var ca C.umad_ca_t

		// Pointer to char array will be allocated on C heap; must free pointer explicitly
		ca_name := C.CString(caName)

		// TODO: Replace umad_get_ca() with pure Go implementation
		if ret := C.umad_get_ca(ca_name, &ca); ret == 0 {
			var (
				config C.ibnd_config_t
				err    error
				fabric Fabric
			)

			fmt.Printf("Found CA %s (%s) with %d ports and firmware %s\n",
				C.GoString(&ca.ca_name[0]), C.GoString(&ca.ca_type[0]), ca.numports, C.GoString(&ca.fw_ver[0]))
			fmt.Printf("Node GUID: %#016x, system GUID: %#016x\n\n",
				ntohll(uint64(ca.node_guid)), ntohll(uint64(ca.system_guid)))

			fmt.Printf("%s: %#v\n\n", caName, ca)

			for p := 1; ca.ports[p] != nil; p++ {
				fmt.Printf("port %d: %#v\n\n", p, ca.ports[p])
			}

			// Return pointer to fabric struct
			fabric.ibndFabric, err = C.ibnd_discover_fabric(&ca.ca_name[0], 1, nil, &config)

			if err != nil {
				fmt.Println("Unable to discover fabric:", err)
				os.Exit(1)
			}

			mgmt_classes := [3]C.int{C.IB_SMI_CLASS, C.IB_SA_CLASS, C.IB_PERFORMANCE_CLASS}
			fabric.ibmadPort, err = C.mad_rpc_open_port(ca_name, 1, &mgmt_classes[0], C.int(len(mgmt_classes)))

			if err != nil {
				fmt.Println("Unable to open MAD port:", err)
				os.Exit(1)
			}

			fmt.Printf("ibmad_port: %#v\n", fabric.ibmadPort)

			// Walk switch nodes in fabric
			iterateSwitches(&fabric, &nnMap, conf.InfluxDB)

			fabric.mutex.Lock()

			// Close MAD port
			C.mad_rpc_close_port(fabric.ibmadPort)

			// Free memory and resources associated with fabric
			C.ibnd_destroy_fabric(fabric.ibndFabric)

			fabric.mutex.Unlock()
		}

		C.free(unsafe.Pointer(ca_name))
	}

	// Start HTTP server to serve JSON for d3.js (WIP)
	//serve(conf.BindAddress, &fabric)
}
