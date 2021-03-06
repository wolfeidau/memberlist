/*
memberlist is a library that manages cluster
membership and member failure detection using a gossip based protocol.

The use cases for such a library are far-reaching: all distributed systems
require membership, and memberlist is a re-usable solution to managing
cluster membership and node failure detection.

memberlist is eventually consistent but converges quickly on average.
The speed at which it converges can be heavily tuned via various knobs
on the protocol. Node failures are detected and network partitions are partially
tolerated by attempting to communicate to potentially dead nodes through
multiple routes.
*/
package memberlist

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

type Memberlist struct {
	config         *Config
	shutdown       bool
	leave          bool
	leaveBroadcast chan struct{}

	udpListener *net.UDPConn
	tcpListener *net.TCPListener

	sequenceNum uint32 // Local sequence number
	incarnation uint32 // Local incarnation number

	nodeLock sync.RWMutex
	nodes    []*nodeState          // Known nodes
	nodeMap  map[string]*nodeState // Maps Addr.String() -> NodeState

	tickerLock sync.Mutex
	tickers    []*time.Ticker
	stopTick   chan struct{}
	probeIndex int

	ackLock     sync.Mutex
	ackHandlers map[uint32]*ackHandler

	broadcasts *TransmitLimitedQueue

	startStopLock sync.Mutex

	logger *log.Logger
}

// newMemberlist creates the network listeners.
// Does not schedule execution of background maintenence.
func newMemberlist(conf *Config) (*Memberlist, error) {
	if conf.ProtocolVersion < ProtocolVersionMin {
		return nil, fmt.Errorf("Protocol version '%d' too low. Must be in range: [%d, %d]",
			conf.ProtocolVersion, ProtocolVersionMin, ProtocolVersionMax)
	} else if conf.ProtocolVersion > ProtocolVersionMax {
		return nil, fmt.Errorf("Protocol version '%d' too high. Must be in range: [%d, %d]",
			conf.ProtocolVersion, ProtocolVersionMin, ProtocolVersionMax)
	}

	if len(conf.SecretKey) > 0 {
		if conf.ProtocolVersion < 1 {
			return nil, fmt.Errorf("Encryption is not supported before protocol version 1")
		}
		if len(conf.SecretKey) != 16 {
			return nil, fmt.Errorf("SecretKey must be 16 bytes in length")
		}
	} else {
		conf.SecretKey = nil
	}

	tcpAddr := &net.TCPAddr{IP: net.ParseIP(conf.BindAddr), Port: conf.Port}
	tcpLn, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return nil, fmt.Errorf("Failed to start TCP listener. Err: %s", err)
	}

	udpAddr := &net.UDPAddr{IP: net.ParseIP(conf.BindAddr), Port: conf.Port}
	udpLn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		tcpLn.Close()
		return nil, fmt.Errorf("Failed to start UDP listener. Err: %s", err)
	}

	// Set the UDP receive window size
	setUDPRecvBuf(udpLn)

	if conf.LogOutput == nil {
		conf.LogOutput = os.Stderr
	}
	logger := log.New(conf.LogOutput, "", log.LstdFlags)

	// Warn if compression is enabled with bad protocol version
	if conf.EnableCompression && conf.ProtocolVersion < 1 {
		logger.Printf("[WARN] Compression is enabled with an unsupported protocol")
		conf.EnableCompression = false
	}

	m := &Memberlist{
		config:         conf,
		leaveBroadcast: make(chan struct{}, 1),
		udpListener:    udpLn,
		tcpListener:    tcpLn,
		nodeMap:        make(map[string]*nodeState),
		ackHandlers:    make(map[uint32]*ackHandler),
		broadcasts:     &TransmitLimitedQueue{RetransmitMult: conf.RetransmitMult},
		logger:         logger,
	}
	m.broadcasts.NumNodes = func() int { return len(m.nodes) }
	go m.tcpListen()
	go m.udpListen()
	return m, nil
}

// Create will create a new Memberlist using the given configuration.
// This will not connect to any other node (see Join) yet, but will start
// all the listeners to allow other nodes to join this memberlist.
// After creating a Memberlist, the configuration given should not be
// modified by the user anymore.
func Create(conf *Config) (*Memberlist, error) {
	m, err := newMemberlist(conf)
	if err != nil {
		return nil, err
	}
	if err := m.setAlive(); err != nil {
		m.Shutdown()
		return nil, err
	}
	m.schedule()
	return m, nil
}

// Join is used to take an existing Memberlist and attempt to join a cluster
// by contacting all the given hosts and performing a state sync. Initially,
// the Memberlist only contains our own state, so doing this will cause
// remote nodes to become aware of the existence of this node, effectively
// joining the cluster.
//
// This returns the number of hosts successfully contacted and an error if
// none could be reached. If an error is returned, the node did not successfully
// join the cluster.
func (m *Memberlist) Join(existing []string) (int, error) {
	// Attempt to join any of them
	numSuccess := 0
	var retErr error
	for _, exist := range existing {
		addr, port, err := m.resolveAddr(exist)
		if err != nil {
			m.logger.Printf("[WARN] Failed to resolve %s: %v", exist, err)
			retErr = err
			continue
		}

		if err := m.pushPullNode(addr, port, true); err != nil {
			retErr = err
			continue
		}

		numSuccess++
	}

	if numSuccess > 0 {
		retErr = nil
	}

	return numSuccess, retErr
}

// resolveAddr is used to resolve the address into an address,
// port, and error. If no port is given, use the default
func (m *Memberlist) resolveAddr(hostStr string) ([]byte, uint16, error) {
	// Add the port if none
START:
	_, _, err := net.SplitHostPort(hostStr)
	if ae, ok := err.(*net.AddrError); ok && ae.Err == "missing port in address" {
		hostStr = fmt.Sprintf("%s:%d", hostStr, m.config.Port)
		goto START
	}
	if err != nil {
		return nil, 0, err
	}

	// Get the address
	addr, err := net.ResolveTCPAddr("tcp", hostStr)
	if err != nil {
		return nil, 0, err
	}

	// Return IP/Port
	return addr.IP, uint16(addr.Port), nil
}

// setAlive is used to mark this node as being alive. This is the same
// as if we received an alive notification our own network channel for
// ourself.
func (m *Memberlist) setAlive() error {
	// Pick a private IP address
	var ipAddr []byte
	if m.config.BindAddr == "0.0.0.0" {
		// We're not bound to a specific IP, so let's list the interfaces
		// on this machine and use the first private IP we find.
		addresses, err := net.InterfaceAddrs()
		if err != nil {
			return fmt.Errorf("Failed to get interface addresses! Err: %vn", err)
		}

		// Find private IPv4 address
		for _, addr := range addresses {
			ip, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ip.IP.To4() == nil {
				continue
			}
			if !isPrivateIP(ip.IP.String()) {
				continue
			}
			ipAddr = ip.IP
			break
		}

		// Failed to find private IP, error
		if ipAddr == nil {
			return fmt.Errorf("No private IP address found, and explicit IP not provided")
		}
	} else {
		// Use the IP that we're bound to.
		addr := m.tcpListener.Addr().(*net.TCPAddr)
		ipAddr = addr.IP
	}

	// Check if this is a public address without encryption
	addrStr := net.IP(ipAddr).String()
	if !isPrivateIP(addrStr) && !isLoopbackIP(addrStr) && m.config.SecretKey == nil {
		m.logger.Printf("[WARN] Binding to public address without encryption!")
	}

	// Get the node meta data
	var meta []byte
	if m.config.Delegate != nil {
		meta = m.config.Delegate.NodeMeta(metaMaxSize)
		if len(meta) > metaMaxSize {
			panic("Node meta data provided is longer than the limit")
		}
	}

	a := alive{
		Incarnation: m.nextIncarnation(),
		Node:        m.config.Name,
		Addr:        ipAddr,
		Port:        uint16(m.config.Port),
		Meta:        meta,
		Vsn: []uint8{
			ProtocolVersionMin, ProtocolVersionMax, m.config.ProtocolVersion,
			m.config.DelegateProtocolMin, m.config.DelegateProtocolMax,
			m.config.DelegateProtocolVersion,
		},
	}
	m.aliveNode(&a)

	return nil
}

// Members returns a list of all known live nodes. The node structures
// returned must not be modified. If you wish to modify a Node, make a
// copy first.
func (m *Memberlist) Members() []*Node {
	m.nodeLock.RLock()
	defer m.nodeLock.RUnlock()

	nodes := make([]*Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		if n.State != stateDead {
			nodes = append(nodes, &n.Node)
		}
	}

	return nodes
}

// NumMembers returns the number of alive nodes currently known. Between
// the time of calling this and calling Members, the number of alive nodes
// may have changed, so this shouldn't be used to determine how many
// members will be returned by Members.
func (m *Memberlist) NumMembers() (alive int) {
	m.nodeLock.RLock()
	defer m.nodeLock.RUnlock()

	for _, n := range m.nodes {
		if n.State != stateDead {
			alive++
		}
	}

	return
}

// Leave will broadcast a leave message but will not shutdown the background
// listeners, meaning the node will continue participating in gossip and state
// updates.
//
// This will block until the leave message is successfully broadcasted to
// a member of the cluster, if any exist or until a specified timeout
// is reached.
//
// This method is safe to call multiple times, but must not be called
// after the cluster is already shut down.
func (m *Memberlist) Leave(timeout time.Duration) error {
	m.startStopLock.Lock()
	defer m.startStopLock.Unlock()

	if m.shutdown {
		panic("leave after shutdown")
	}

	if !m.leave {
		m.leave = true

		state, ok := m.nodeMap[m.config.Name]
		if !ok {
			m.logger.Println("[WARN] Leave but we're not in the node map.")
			return nil
		}

		d := dead{
			Incarnation: state.Incarnation,
			Node:        state.Name,
		}
		m.deadNode(&d)

		// Check for any other alive node
		anyAlive := false
		for _, n := range m.nodes {
			if n.State != stateDead {
				anyAlive = true
				break
			}
		}

		// Block until the broadcast goes out
		if anyAlive {
			var timeoutCh <-chan time.Time
			if timeout > 0 {
				timeoutCh = time.After(timeout)
			}
			select {
			case <-m.leaveBroadcast:
			case <-timeoutCh:
				return fmt.Errorf("timeout waiting for leave broadcast")
			}
		}
	}

	return nil
}

// ProtocolVersion returns the protocol version currently in use by
// this memberlist.
func (m *Memberlist) ProtocolVersion() uint8 {
	// NOTE: This method exists so that in the future we can control
	// any locking if necessary, if we change the protocol version at
	// runtime, etc.
	return m.config.ProtocolVersion
}

// Shutdown will stop any background maintanence of network activity
// for this memberlist, causing it to appear "dead". A leave message
// will not be broadcasted prior, so the cluster being left will have
// to detect this node's shutdown using probing. If you wish to more
// gracefully exit the cluster, call Leave prior to shutting down.
//
// This method is safe to call multiple times.
func (m *Memberlist) Shutdown() error {
	m.startStopLock.Lock()
	defer m.startStopLock.Unlock()

	if !m.shutdown {
		m.shutdown = true
		m.deschedule()
		m.udpListener.Close()
		m.tcpListener.Close()
	}

	return nil
}
