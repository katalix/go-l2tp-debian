package l2tp

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"golang.org/x/sys/unix"
)

// Context is a container for a collection of L2TP tunnels and
// their sessions.
type Context struct {
	logger        log.Logger
	tunnelsByName map[string]tunnel
	tunnelsByID   map[ControlConnID]tunnel
	tlock         sync.RWMutex
	dp            DataPlane
	callSerial    uint32
	serialLock    sync.Mutex
	eventHandlers []EventHandler
	evtLock       sync.RWMutex
}

// Tunnel is an interface representing an L2TP tunnel.
type Tunnel interface {
	// NewSession adds a session to a tunnel instance.
	//
	// The name provided must be unique in the parent tunnel.
	NewSession(name string, cfg *SessionConfig) (Session, error)

	// Close closes the tunnel, releasing allocated resources.
	//
	// Any sessions instantiated inside the tunnel are removed.
	Close()
}

type tunnel interface {
	Tunnel
	getName() string
	getCfg() *TunnelConfig
	getDP() DataPlane
	getLogger() log.Logger
	unlinkSession(s session)
}

// Session is an interface representing an L2TP session.
type Session interface {
	// Close closes the session, releasing allocated resources.
	Close()
}

type session interface {
	Session
	getName() string
	getCfg() *SessionConfig
	kill()
}

// DataPlane is an interface for creating tunnel and session
// data plane instances.
type DataPlane interface {
	// NewTunnel creates a new tunnel data plane instance.
	//
	// The localAddress and peerAddress arguments are unix Sockaddr
	// representations of the tunnel local and peer address.
	//
	// fd is the tunnel socket fd, which may be invalid (<0) for tunnel
	// types which don't manage the tunnel socket in userspace.
	//
	// On successful return the dataplane should be fully ready for use.
	NewTunnel(
		tcfg *TunnelConfig,
		localAddress, peerAddress unix.Sockaddr,
		fd int) (TunnelDataPlane, error)

	// NewSession creates a new session data plane instance.
	//
	// tunnelID and peerTunnelID are the L2TP IDs for the parent tunnel
	// of this session (local and peer respectively).
	//
	// On successful return the dataplane should be fully ready for use.
	NewSession(tunnelID, peerTunnelID ControlConnID, scfg *SessionConfig) (SessionDataPlane, error)

	// Close is called to release any resources held by the dataplane instance.
	// It is called when the l2tp Context using the dataplane shuts down.
	Close()
}

// TunnelDataPlane is an interface representing a tunnel data plane.
type TunnelDataPlane interface {
	// Down performs the necessary actions to tear down the data plane.
	// On successful return the dataplane should be fully destroyed.
	Down() error
}

// SessionDataPlane is an interface representing a session data plane.
type SessionDataPlane interface {
	// Down performs the necessary actions to tear down the data plane.
	// On successful return the dataplane should be fully destroyed.
	Down() error
}

// EventHandler is an interface for receiving L2TP-specific events.
type EventHandler interface {
	// HandleEvent is called when an event occurs.
	//
	// HandleEvent will be called from the goroutine of the tunnel or
	// session generating the event.
	//
	// The event passed is a pointer to a type specific to the event
	// which has occurred.  Use type assertions to determine which event
	// is being passed.
	HandleEvent(event interface{})
}

// TunnelUpEvent is passed to registered EventHandler instances when a
// tunnel comes up.  In the case of static or quiescent tunnels, this occurs
// immediately on instantiation of the tunnel.  For dynamic tunnels, this
// occurs on completion of the L2TP control protocol message exchange with
// the peer.
type TunnelUpEvent struct {
	Tunnel                    Tunnel
	Config                    *TunnelConfig
	LocalAddress, PeerAddress unix.Sockaddr
}

// TunnelDownEvent is passed to registered EventHandler instances when a
// tunnel goes down.  In the case of static or quiescent tunnels, this occurs
// immediately on closure of the tunnel.  For dynamic tunnels, this
// occurs on completion of the L2TP control protocol message exchange with
// the peer.
type TunnelDownEvent struct {
	Tunnel                    Tunnel
	Config                    *TunnelConfig
	LocalAddress, PeerAddress unix.Sockaddr
}

// LinuxNetlinkDataPlane is a special sentinel value used to indicate
// that the L2TP context should use the internal Linux kernel data plane
// implementation.
var LinuxNetlinkDataPlane DataPlane = &nullDataPlane{}

// NewContext creates a new L2TP context, which can then be used
// to instantiate tunnel and session instances.
//
// The dataplane interface may be specified as LinuxNetlinkDataPlane,
// in which case an internal implementation of the Linux Kernel
// L2TP data plane is used.  In this case, context creation will
// fail if it is not possible to connect to the kernel L2TP subsystem:
// the kernel must be running the L2TP modules, and the process must
// have appropriate permissions to access them.
//
// If the dataplane is specified as nil, a special "null" data plane
// implementation is used.  This is useful for experimenting with the
// control protocol without requiring root permissions.
//
// Logging is generated using go-kit levels: informational logging
// uses the Info level, while verbose debugging logging uses the
// Debug level.  Error conditions may be logged using the Error level
// depending on the tunnel type.
//
// If a nil logger is passed, all logging is disabled.
func NewContext(dataPlane DataPlane, logger log.Logger) (*Context, error) {

	if logger == nil {
		logger = log.NewNopLogger()
	}

	rand.Seed(time.Now().UnixNano())

	dp, err := initDataPlane(dataPlane)
	if err != nil {
		return nil, fmt.Errorf("failed to initialise data plane: %v", err)
	}

	return &Context{
		logger:        logger,
		tunnelsByName: make(map[string]tunnel),
		tunnelsByID:   make(map[ControlConnID]tunnel),
		dp:            dp,
		callSerial:    rand.Uint32(),
	}, nil
}

// NewDynamicTunnel creates a new dynamic L2TP.
//
// A dynamic L2TP tunnel runs a full RFC2661 (L2TPv2) or
// RFC3931 (L2TPv3) tunnel instance using the control protocol
// for tunnel instantiation and management.
//
// The name provided must be unique in the Context.
//
func (ctx *Context) NewDynamicTunnel(name string, cfg *TunnelConfig) (tunl Tunnel, err error) {

	var sal, sap unix.Sockaddr

	// Must have configuration
	if cfg == nil {
		return nil, fmt.Errorf("invalid nil config")
	}

	// Duplicate the configuration so we don't modify the user's copy
	myCfg := *cfg

	// Must not have name clashes
	if _, ok := ctx.findTunnelByName(name); ok {
		return nil, fmt.Errorf("already have tunnel %q", name)
	}

	// Generate host name if unset
	if myCfg.HostName == "" {
		name, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("failed to look up host name: %v", err)
		}
		myCfg.HostName = name
	}

	// Default StopCCN retransmit timeout if unset.
	// RFC2661 section 5.7 recommends a default of 31s.
	if myCfg.StopCCNTimeout == 0 {
		myCfg.StopCCNTimeout = 31 * time.Second
	}

	// Sanity check the configuration
	if myCfg.Version != ProtocolVersion3 && myCfg.Encap == EncapTypeIP {
		return nil, fmt.Errorf("IP encapsulation only supported for L2TPv3 tunnels")
	}
	if myCfg.Version == ProtocolVersion2 {
		if myCfg.TunnelID > 65535 {
			return nil, fmt.Errorf("L2TPv2 connection ID %v out of range", myCfg.TunnelID)
		}
	}
	if myCfg.PeerTunnelID != 0 {
		return nil, fmt.Errorf("L2TPv2 peer connection ID cannot be specified for dynamic tunnels")
	}
	if myCfg.Peer == "" {
		return nil, fmt.Errorf("must specify peer address for dynamic tunnel")
	}

	// If the tunnel ID in the config is unset we must generate one.
	// If the tunnel ID is set, we must check for collisions.
	// TODO: there is a potential race here if dynamic tunnels are concurrently
	// added -- an ID assigned here isn't actually reserved until the linkTunnel
	// call below.
	if myCfg.TunnelID != 0 {
		// Must not have TID clashes
		if _, ok := ctx.findTunnelByID(myCfg.TunnelID); ok {
			return nil, fmt.Errorf("already have tunnel with TID %q", myCfg.TunnelID)
		}
	} else {
		myCfg.TunnelID, err = ctx.allocTid(myCfg.Version)
		if err != nil {
			return nil, fmt.Errorf("failed to allocate a TID: %q", err)
		}
	}

	// Initialise tunnel address structures
	switch myCfg.Encap {
	case EncapTypeUDP:
		sal, sap, err = newUDPAddressPair(myCfg.Local, myCfg.Peer)
	case EncapTypeIP:
		sal, sap, err = newIPAddressPair(myCfg.Local, myCfg.TunnelID,
			myCfg.Peer, myCfg.PeerTunnelID)
	default:
		err = fmt.Errorf("unrecognised encapsulation type %v", myCfg.Encap)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to initialise tunnel addresses: %v", err)
	}

	t, err := newDynamicTunnel(name, ctx, sal, sap, &myCfg)
	if err != nil {
		return nil, err
	}

	ctx.linkTunnel(t)
	tunl = t

	return
}

// NewQuiescentTunnel creates a new "quiescent" L2TP tunnel.
//
// A quiescent tunnel creates a user space socket for the
// L2TP control plane, but does not run the control protocol
// beyond acknowledging messages and optionally sending HELLO
// messages.
//
// The data plane is established on creation of the tunnel instance.
//
// The name provided must be unique in the Context.
//
// The tunnel configuration must include local and peer addresses
// and local and peer tunnel IDs.
func (ctx *Context) NewQuiescentTunnel(name string, cfg *TunnelConfig) (tunl Tunnel, err error) {

	var sal, sap unix.Sockaddr

	// Must have configuration
	if cfg == nil {
		return nil, fmt.Errorf("invalid nil config")
	}

	// Duplicate the configuration so we don't modify the user's copy
	myCfg := *cfg

	// Must not have name clashes
	if _, ok := ctx.findTunnelByName(name); ok {
		return nil, fmt.Errorf("already have tunnel %q", name)
	}

	// Sanity check the configuration
	if myCfg.Version != ProtocolVersion3 && myCfg.Encap == EncapTypeIP {
		return nil, fmt.Errorf("IP encapsulation only supported for L2TPv3 tunnels")
	}
	if myCfg.Version == ProtocolVersion2 {
		if myCfg.TunnelID == 0 || myCfg.TunnelID > 65535 {
			return nil, fmt.Errorf("L2TPv2 connection ID %v out of range", myCfg.TunnelID)
		} else if myCfg.PeerTunnelID == 0 || myCfg.PeerTunnelID > 65535 {
			return nil, fmt.Errorf("L2TPv2 peer connection ID %v out of range", myCfg.PeerTunnelID)
		}
	} else {
		if myCfg.TunnelID == 0 || myCfg.PeerTunnelID == 0 {
			return nil, fmt.Errorf("L2TPv3 tunnel IDs %v and %v must both be > 0",
				myCfg.TunnelID, myCfg.PeerTunnelID)
		}
	}
	if myCfg.Local == "" {
		return nil, fmt.Errorf("must specify local address for quiescent tunnel")
	}
	if myCfg.Peer == "" {
		return nil, fmt.Errorf("must specify peer address for quiescent tunnel")
	}

	// Must not have TID clashes
	if _, ok := ctx.findTunnelByID(myCfg.TunnelID); ok {
		return nil, fmt.Errorf("already have tunnel with TID %q", myCfg.TunnelID)
	}

	// Initialise tunnel address structures
	switch myCfg.Encap {
	case EncapTypeUDP:
		sal, sap, err = newUDPAddressPair(myCfg.Local, myCfg.Peer)
	case EncapTypeIP:
		sal, sap, err = newIPAddressPair(myCfg.Local, myCfg.TunnelID,
			myCfg.Peer, myCfg.PeerTunnelID)
	default:
		err = fmt.Errorf("unrecognised encapsulation type %v", myCfg.Encap)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to initialise tunnel addresses: %v", err)
	}

	t, err := newQuiescentTunnel(name, ctx, sal, sap, &myCfg)
	if err != nil {
		return nil, err
	}

	ctx.linkTunnel(t)
	tunl = t

	return
}

// NewStaticTunnel creates a new static (unmanaged) L2TP tunnel.
//
// A static tunnel does not run any control protocol
// and instead merely instantiates the data plane in the
// kernel.  This is equivalent to the Linux 'ip l2tp'
// command(s).
//
// Static L2TPv2 tunnels are not practically useful,
// so NewStaticTunnel only supports creation of L2TPv3
// unmanaged tunnel instances.
//
// The name provided must be unique in the Context.
//
// The tunnel configuration must include local and peer addresses
// and local and peer tunnel IDs.
func (ctx *Context) NewStaticTunnel(name string, cfg *TunnelConfig) (tunl Tunnel, err error) {

	var sal, sap unix.Sockaddr

	// Must have configuration
	if cfg == nil {
		return nil, fmt.Errorf("invalid nil config")
	}

	// Duplicate the configuration so we don't modify the user's copy
	myCfg := *cfg

	// Must not have name clashes
	if _, ok := ctx.findTunnelByName(name); ok {
		return nil, fmt.Errorf("already have tunnel %q", name)
	}

	// Sanity check  the configuration
	if myCfg.Version != ProtocolVersion3 {
		return nil, fmt.Errorf("static tunnels can be L2TPv3 only")
	}
	if myCfg.TunnelID == 0 || myCfg.PeerTunnelID == 0 {
		return nil, fmt.Errorf("L2TPv3 tunnel IDs %v and %v must both be > 0",
			myCfg.TunnelID, myCfg.PeerTunnelID)
	}
	if myCfg.Local == "" {
		return nil, fmt.Errorf("must specify local address for static tunnel")
	}
	if myCfg.Peer == "" {
		return nil, fmt.Errorf("must specify peer address for static tunnel")
	}

	// Must not have TID clashes
	if _, ok := ctx.findTunnelByID(myCfg.TunnelID); ok {
		return nil, fmt.Errorf("already have tunnel with TID %q", myCfg.TunnelID)
	}

	// Initialise tunnel address structures
	switch myCfg.Encap {
	case EncapTypeUDP:
		sal, sap, err = newUDPAddressPair(myCfg.Local, myCfg.Peer)
	case EncapTypeIP:
		sal, sap, err = newIPAddressPair(myCfg.Local, myCfg.TunnelID,
			myCfg.Peer, myCfg.PeerTunnelID)
	default:
		err = fmt.Errorf("unrecognised encapsulation type %v", myCfg.Encap)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to initialise tunnel addresses: %v", err)
	}

	t, err := newStaticTunnel(name, ctx, sal, sap, &myCfg)
	if err != nil {
		return nil, err
	}

	ctx.linkTunnel(t)
	tunl = t

	return
}

// RegisterEventHandler adds an event handler to the L2TP context.
//
// On return, the event handler may be called at any time.
//
// The event handler may be called from multiple go routines managed
// by the L2TP context.
func (ctx *Context) RegisterEventHandler(handler EventHandler) {
	ctx.evtLock.Lock()
	defer ctx.evtLock.Unlock()
	ctx.eventHandlers = append(ctx.eventHandlers, handler)
}

// UnregisterEventHandler removes an event handler from the L2TP context.
//
// It must not be called from the context of an event handler callback.
//
// On return the event handler will not be called on further L2TP events.
func (ctx *Context) UnregisterEventHandler(handler EventHandler) {
	ctx.evtLock.Lock()
	defer ctx.evtLock.Unlock()
	for i, hdlr := range ctx.eventHandlers {
		if hdlr == handler {
			ctx.eventHandlers = append(ctx.eventHandlers[:], ctx.eventHandlers[i+1:]...)
			break
		}
	}
}

func (ctx *Context) handleUserEvent(event interface{}) {
	ctx.evtLock.RLock()
	defer ctx.evtLock.RUnlock()
	for _, hdlr := range ctx.eventHandlers {
		hdlr.HandleEvent(event)
	}
}

// Close tears down the context, including all the L2TP tunnels and sessions
// running inside it.
func (ctx *Context) Close() {
	tunnels := []Tunnel{}

	ctx.tlock.Lock()
	for name, tunl := range ctx.tunnelsByName {
		tunnels = append(tunnels, tunl)
		delete(ctx.tunnelsByName, name)
		delete(ctx.tunnelsByID, tunl.getCfg().TunnelID)
	}
	ctx.tlock.Unlock()

	for _, tunl := range tunnels {
		tunl.Close()
	}

	ctx.dp.Close()

}

func (ctx *Context) allocTid(version ProtocolVersion) (ControlConnID, error) {
	for i := 0; i < 10; i++ {
		id, err := generateControlConnID(version)
		if err != nil {
			return 0, fmt.Errorf("failed to generate tunnel ID: %v", err)
		}
		if _, ok := ctx.findTunnelByID(id); !ok {
			return id, nil
		}
	}
	return 0, fmt.Errorf("ID space exhausted")
}

func (ctx *Context) linkTunnel(tunl tunnel) {
	ctx.tlock.Lock()
	defer ctx.tlock.Unlock()
	ctx.tunnelsByName[tunl.getName()] = tunl
	ctx.tunnelsByID[tunl.getCfg().TunnelID] = tunl
}

func (ctx *Context) unlinkTunnel(tunl tunnel) {
	ctx.tlock.Lock()
	defer ctx.tlock.Unlock()
	delete(ctx.tunnelsByName, tunl.getName())
	delete(ctx.tunnelsByID, tunl.getCfg().TunnelID)
}

func (ctx *Context) findTunnelByName(name string) (tunl tunnel, ok bool) {
	ctx.tlock.RLock()
	defer ctx.tlock.RUnlock()
	tunl, ok = ctx.tunnelsByName[name]
	return
}

func (ctx *Context) findTunnelByID(tid ControlConnID) (tunl tunnel, ok bool) {
	ctx.tlock.RLock()
	defer ctx.tlock.RUnlock()
	tunl, ok = ctx.tunnelsByID[tid]
	return
}

func (ctx *Context) allocCallSerial() uint32 {
	ctx.serialLock.Lock()
	defer ctx.serialLock.Unlock()
	ctx.callSerial++
	return ctx.callSerial
}

func newUDPTunnelAddress(address string) (unix.Sockaddr, error) {

	u, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve %v: %v", address, err)
	}

	if b := u.IP.To4(); b != nil {
		return &unix.SockaddrInet4{
			Port: u.Port,
			Addr: [4]byte{b[0], b[1], b[2], b[3]},
		}, nil
	} else if b := u.IP.To16(); b != nil {
		// TODO: SockaddrInet6 has a uint32 ZoneId, while UDPAddr
		// has a Zone string.  How to convert between the two?
		return &unix.SockaddrInet6{
			Port: u.Port,
			Addr: [16]byte{
				b[0], b[1], b[2], b[3],
				b[4], b[5], b[6], b[7],
				b[8], b[9], b[10], b[11],
				b[12], b[13], b[14], b[15],
			},
			// ZoneId
		}, nil
	}

	return nil, fmt.Errorf("unhandled address family")
}

func newUDPAddressPair(local, remote string) (sal, sap unix.Sockaddr, err error) {

	// We expect the peer address to always be set
	sap, err = newUDPTunnelAddress(remote)
	if err != nil {
		return nil, nil, fmt.Errorf("remote address %q: %v", remote, err)
	}

	// The local address may not be set: in this case return
	// a zero-value sockaddr appropriate to the peer address type
	if local != "" {
		sal, err = newUDPTunnelAddress(local)
		if err != nil {
			return nil, nil, fmt.Errorf("local address %q: %v", local, err)
		}
	} else {
		switch sap.(type) {
		case *unix.SockaddrInet4:
			sal = &unix.SockaddrInet4{}
		case *unix.SockaddrInet6:
			sal = &unix.SockaddrInet6{}
		default:
			// should not occur, c.f. newUDPTunnelAddress
			return nil, nil, fmt.Errorf("unhanded address family")
		}
	}
	return
}

func newIPTunnelAddress(address string, ccid ControlConnID) (unix.Sockaddr, error) {

	u, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve %v: %v", address, err)
	}

	if b := u.IP.To4(); b != nil {
		return &unix.SockaddrL2TPIP{
			Addr:   [4]byte{b[0], b[1], b[2], b[3]},
			ConnId: uint32(ccid),
		}, nil
	} else if b := u.IP.To16(); b != nil {
		// TODO: SockaddrInet6 has a uint32 ZoneId, while UDPAddr
		// has a Zone string.  How to convert between the two?
		return &unix.SockaddrL2TPIP6{
			Addr: [16]byte{
				b[0], b[1], b[2], b[3],
				b[4], b[5], b[6], b[7],
				b[8], b[9], b[10], b[11],
				b[12], b[13], b[14], b[15],
			},
			// ZoneId
			ConnId: uint32(ccid),
		}, nil
	}

	return nil, fmt.Errorf("unhandled address family")
}

func newIPAddressPair(local string, ccid ControlConnID, remote string, pccid ControlConnID) (sal, sap unix.Sockaddr, err error) {
	// We expect the peer address to always be set
	sap, err = newIPTunnelAddress(remote, pccid)
	if err != nil {
		return nil, nil, fmt.Errorf("remote address %q: %v", remote, err)
	}

	// The local address may not be set: in this case return
	// a zero-value sockaddr appropriate to the peer address type
	if local != "" {
		sal, err = newIPTunnelAddress(local, ccid)
		if err != nil {
			return nil, nil, fmt.Errorf("local address %q: %v", local, err)
		}
	} else {
		switch sap.(type) {
		case *unix.SockaddrL2TPIP:
			sal = &unix.SockaddrL2TPIP{}
		case *unix.SockaddrL2TPIP6:
			sal = &unix.SockaddrL2TPIP6{}
		default:
			// should not occur, c.f. newIPTunnelAddress
			return nil, nil, fmt.Errorf("unhanded address family")
		}
	}
	return
}

func initDataPlane(dp DataPlane) (DataPlane, error) {
	if dp == nil {
		return &nullDataPlane{}, nil
	} else if dp == LinuxNetlinkDataPlane {
		return newNetlinkDataPlane()
	}
	return dp, nil
}

func generateControlConnID(version ProtocolVersion) (ControlConnID, error) {
	var id ControlConnID
	switch version {
	case ProtocolVersion2:
		id = ControlConnID(uint16(rand.Uint32()))
	case ProtocolVersion3:
		id = ControlConnID(rand.Uint32())
	default:
		return 0, fmt.Errorf("unhandled version %v", version)
	}
	return id, nil
}

// baseTunnel implements base functionality which all tunnel types will need
type baseTunnel struct {
	logger         log.Logger
	name           string
	parent         *Context
	cfg            *TunnelConfig
	sessionLock    sync.RWMutex
	sessionsByName map[string]session
	sessionsByID   map[ControlConnID]session
}

func newBaseTunnel(logger log.Logger, name string, parent *Context, config *TunnelConfig) *baseTunnel {
	return &baseTunnel{
		logger:         logger,
		name:           name,
		parent:         parent,
		cfg:            config,
		sessionsByName: make(map[string]session),
		sessionsByID:   make(map[ControlConnID]session),
	}
}

func (bt *baseTunnel) getName() string {
	return bt.name
}

func (bt *baseTunnel) getCfg() *TunnelConfig {
	return bt.cfg
}

func (bt *baseTunnel) getDP() DataPlane {
	return bt.parent.dp
}

func (bt *baseTunnel) getLogger() log.Logger {
	return bt.logger
}

func (bt *baseTunnel) linkSession(s session) {
	bt.sessionLock.Lock()
	defer bt.sessionLock.Unlock()
	bt.sessionsByName[s.getName()] = s
	bt.sessionsByID[s.getCfg().SessionID] = s
}

func (bt *baseTunnel) unlinkSession(s session) {
	bt.sessionLock.Lock()
	defer bt.sessionLock.Unlock()
	delete(bt.sessionsByName, s.getName())
	delete(bt.sessionsByID, s.getCfg().SessionID)
}

func (bt *baseTunnel) findSessionByName(name string) (s session, ok bool) {
	bt.sessionLock.RLock()
	defer bt.sessionLock.RUnlock()
	s, ok = bt.sessionsByName[name]
	return
}

func (bt *baseTunnel) findSessionByID(id ControlConnID) (s session, ok bool) {
	bt.sessionLock.RLock()
	defer bt.sessionLock.RUnlock()
	s, ok = bt.sessionsByID[id]
	return
}

func (bt *baseTunnel) allSessions() (sessions []session) {
	bt.sessionLock.RLock()
	defer bt.sessionLock.RUnlock()
	for _, s := range bt.sessionsByName {
		sessions = append(sessions, s)
	}
	return
}

// Close all sessions in a tunnel without kicking their FSM instances.
// When a tunnel goes down, StopCCN is sufficient to implicitly terminate
// all session instances running in that tunnel.
func (bt *baseTunnel) closeAllSessions() {
	sessions := []session{}

	bt.sessionLock.Lock()
	for name, s := range bt.sessionsByName {
		sessions = append(sessions, s)
		delete(bt.sessionsByName, name)
		delete(bt.sessionsByID, s.getCfg().SessionID)
	}
	bt.sessionLock.Unlock()

	for _, s := range sessions {
		s.kill()
	}
}

func (bt *baseTunnel) allocSid() (ControlConnID, error) {
	for i := 0; i < 10; i++ {
		id, err := generateControlConnID(bt.cfg.Version)
		if err != nil {
			return 0, fmt.Errorf("failed to generate session ID: %v", err)
		}
		if _, ok := bt.findSessionByID(id); !ok {
			return id, nil
		}
	}
	return 0, fmt.Errorf("ID space exhausted")
}

// baseSession implements base functionality which all session types will need
type baseSession struct {
	logger log.Logger
	name   string
	parent tunnel
	cfg    *SessionConfig
}

func newBaseSession(logger log.Logger, name string, parent tunnel, config *SessionConfig) *baseSession {
	return &baseSession{
		logger: logger,
		name:   name,
		parent: parent,
		cfg:    config,
	}
}

func (bs *baseSession) getName() string {
	return bs.name
}

func (bs *baseSession) getCfg() *SessionConfig {
	return bs.cfg
}
