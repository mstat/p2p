package ptp

import (
	"bytes"
	"fmt"
	bencode "github.com/jackpal/bencode-go"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"
)

type OperatingMode int
type DHTState int

const (
	MODE_CLIENT OperatingMode = 1
	MODE_CP     OperatingMode = 2
)

const (
	D_CONNECTING   DHTState = 0 + iota
	D_RECONNECTING DHTState = 1
	D_OPERATING    DHTState = 2
)

type DHTClient struct {
	Routers          string
	FailedRouters    []string
	Connection       []*net.UDPConn
	NetworkHash      string
	NetworkPeers     []string
	P2PPort          int
	LastCatch        []string
	ID               string
	Peers            []PeerIP
	Forwarders       []Forwarder
	ProxyBlacklist   []*net.UDPAddr
	ResponseHandlers map[string]DHTResponseCallback
	Mode             OperatingMode
	Shutdown         bool
	IPList           []net.IP
	State            DHTState
	IP               net.IP
	Network          *net.IPNet
	DataChannel      chan []byte
	CommandChannel   chan []byte
	Listeners        int
	PeerChannel      chan []PeerIP
	ProxyChannel     chan Forwarder
	LastDHTPing      time.Time
	RemovePeerChan   chan string
	ForwardersLock   sync.Mutex // To avoid multiple read-write
}

type Forwarder struct {
	Addr          *net.UDPAddr
	DestinationID string
}

type PeerIP struct {
	ID  string
	Ips []*net.UDPAddr
}

type DHTResponseCallback func(data DHTMessage, conn *net.UDPConn)

func (dht *DHTClient) DHTClientConfig() *DHTClient {
	return &DHTClient{
		Routers: "dht1.subut.ai:6881",
		//Routers:     "dht1.subut.ai:6881,dht2.subut.ai:6881,dht3.subut.ai:6881,dht4.subut.ai:6881,dht5.subut.ai:6881",
		NetworkHash: "",
	}
}

// AddConnection adds new UDP Connection reference onto list of DHT node connections
func (dht *DHTClient) AddConnection(connections []*net.UDPConn, conn *net.UDPConn) []*net.UDPConn {
	n := len(connections)
	if n == cap(connections) {
		newSlice := make([]*net.UDPConn, len(connections), 2*len(connections)+1)
		copy(newSlice, connections)
		connections = newSlice
	}
	connections = connections[0 : n+1]
	connections[n] = conn
	return connections
}

func (dht *DHTClient) Handshake(conn *net.UDPConn) error {
	// Handshake
	var req DHTMessage
	req.Id = "0"
	req.Query = PACKET_VERSION
	req.Command = CMD_CONN
	// TODO: rename Port to something more clear
	req.Arguments = fmt.Sprintf("%d", dht.P2PPort)
	req.Payload = dht.NetworkHash
	for _, ip := range dht.IPList {
		req.Arguments = req.Arguments + "|" + ip.String()
	}
	var b bytes.Buffer
	if err := bencode.Marshal(&b, req); err != nil {
		Log(ERROR, "Failed to Marshal bencode %v", err)
		conn.Close()
		return err
	}
	// TODO: Optimize types here
	msg := b.String()
	if dht.Shutdown {
		return nil
	}
	_, err := conn.Write([]byte(msg))
	if err != nil {
		Log(ERROR, "Failed to send packet: %v", err)
		conn.Close()
		return err
	}
	return nil
}

// ConnectAndHandshake sends an initial packet to a DHT bootstrap node
func (dht *DHTClient) ConnectAndHandshake(router string, ips []net.IP) (*net.UDPConn, error) {
	dht.State = D_CONNECTING
	Log(INFO, "Connecting to a router %s", router)
	addr, err := net.ResolveUDPAddr("udp", router)
	if err != nil {
		Log(ERROR, "Failed to resolve discovery service address: %v", err)
		return nil, err
	}

	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		Log(ERROR, "Failed to establish connection to discovery service: %v", err)
		return nil, err
	}

	Log(INFO, "Ready to peer discovery via %s [%s]", router, conn.RemoteAddr().String())

	err = dht.Handshake(conn)

	return conn, err
}

// Extracts DHTMessage from received packet
func (dht *DHTClient) Extract(b []byte) (response DHTMessage, err error) {
	defer func() {
		if x := recover(); x != nil {
			Log(ERROR, "Bencode Unmarshal failed %q, %v", string(b), x)
		}
	}()
	if e2 := bencode.Unmarshal(bytes.NewBuffer(b), &response); e2 == nil {
		err = nil
		return
	} else {
		Log(DEBUG, "Received from peer: %v %q", response, e2)
		return response, e2
	}
}

// Returns a bencoded representation of a DHTMessage
func (dht *DHTClient) Compose(command, id, query, arguments string) string {
	var req DHTMessage
	// Command is mandatory
	req.Command = command
	// Defaults
	req.Id = "0"
	req.Query = "0"
	if id != "" {
		req.Id = id
	}
	if query != "" {
		req.Query = query
	}
	req.Arguments = arguments
	return dht.EncodeRequest(req)
}

func (dht *DHTClient) EncodeRequest(req DHTMessage) string {
	if req.Command == "" {
		return ""
	}
	var b bytes.Buffer
	if err := bencode.Marshal(&b, req); err != nil {
		Log(ERROR, "Failed to Marshal bencode %v", err)
		return ""
	}
	return b.String()
}

// After receiving a list of peers from DHT we will parse the list
// and add every new peer into list of peers
func (dht *DHTClient) UpdateLastCatch(catch string) {
	peers := strings.Split(catch, ",")
	for _, p := range peers {
		if p == "" {
			continue
		}
		var found bool = false
		for _, catchedPeer := range dht.LastCatch {
			if p == catchedPeer {
				found = true
			}
		}
		if !found {
			dht.LastCatch = append(dht.LastCatch, p)
		}
	}
}

// This function sends a request to DHT bootstrap node with ID of
// target node we want to connect to
func (dht *DHTClient) RequestPeerIPs(id string) {
	msg := dht.Compose(CMD_NODE, dht.ID, id, "")
	for _, conn := range dht.Connection {
		if dht.Shutdown {
			continue
		}
		_, err := conn.Write([]byte(msg))
		if err != nil {
			Log(ERROR, "Failed to send 'node' request to %s: %v", conn.RemoteAddr().String(), err)
		}
	}
}

// UpdatePeers sends "find" request to a DHT Bootstrap node, so it can respond
// with a list of peers that we can connect to
// This method should be called periodically in case any new peers was discovered
func (dht *DHTClient) UpdatePeers() {
	for {
		if dht.Shutdown {
			break
		}
		dht.SendUpdateRequest()
		// Just in case do an update
		time.Sleep(5 * time.Minute)
	}
}

func (dht *DHTClient) SendUpdateRequest() {
	msg := dht.Compose(CMD_FIND, dht.ID, dht.NetworkHash, "")
	for _, conn := range dht.Connection {
		if dht.Shutdown {
			continue
		}
		Log(DEBUG, "Updating peers from %s", conn.RemoteAddr().String())
		_, err := conn.Write([]byte(msg))
		if err != nil {
			Log(ERROR, "Failed to send 'find' request to %s: %v", conn.RemoteAddr().String(), err)
		}
	}
}

// Listens for packets received from DHT bootstrap node
// Every packet is unmarshaled and turned into Request structure
// which we should analyze and respond
func (dht *DHTClient) ListenDHT(conn *net.UDPConn) {
	defer conn.Close()
	Log(INFO, "Bootstraping via %s", conn.RemoteAddr().String())
	dht.Listeners++
	var failCounter = 0
	for {
		if dht.Shutdown {
			Log(INFO, "Closing DHT Connection to %s", conn.RemoteAddr().String())
			conn.Close()
			for i, c := range dht.Connection {
				if c.RemoteAddr().String() == conn.RemoteAddr().String() {
					dht.Connection = append(dht.Connection[:i], dht.Connection[i+1:]...)
				}
			}
			break
		}
		var buf [512]byte
		_, _, err := conn.ReadFromUDP(buf[0:])
		if err != nil {
			Log(DEBUG, "Failed to read from Discovery Service: %v", err)
			failCounter++
		} else {
			failCounter = 0
			data, err := dht.Extract(buf[:512])
			if err != nil {
				Log(ERROR, "Failed to extract a message received from discovery service: %v", err)
			} else {
				callback, exists := dht.ResponseHandlers[data.Command]
				if exists {
					Log(TRACE, "DHT Received %v", data)
					callback(data, conn)
				} else {
					Log(DEBUG, "Unsupported packet type received from DHT: %s", data.Command)
				}
			}
		}
		if failCounter > 1000 {
			Log(ERROR, "Multiple errors reading from DHT")
			break
		}
	}
	dht.Listeners--
}

func (dht *DHTClient) HandleConn(data DHTMessage, conn *net.UDPConn) {
	if dht.State != D_CONNECTING && dht.State != D_RECONNECTING {
		return
	}
	if data.Id == "" {
		Log(ERROR, "Empty ID was received")
		return
	}
	if data.Id == "0" {
		Log(ERROR, "Empty ID were received. Stopping")
		return
	}
	dht.State = D_OPERATING
	dht.ID = data.Id
	Log(INFO, "Received connection confirmation from router %s",
		conn.RemoteAddr().String())
	Log(INFO, "Received personal ID for this session: %s", data.Id)
	// Send a hash within FIND command
	// Afterwards application should wait for response from DHT
	// with list of clients. This may not happen if this client is the
	// first connected node.
	/*
		msg := dht.Compose(CMD_FIND, dht.ID, dht.NetworkHash, "")
		if dht.Shutdown {
			return
		}
		_, err := conn.Write([]byte(msg))
		if err != nil {
			Log(ERROR, "Failed to send 'find' request: %v", err)
		} else {
			Log(INFO, "Received connection confirmation from router %s",
				conn.RemoteAddr().String())
			Log(INFO, "Received personal ID for this session: %s", data.Id)
		}
	*/
}

func (dht *DHTClient) HandlePing(data DHTMessage, conn *net.UDPConn) {
	Log(TRACE, "Ping message from DHT")
	dht.LastDHTPing = time.Now()
	msg := dht.Compose(CMD_PING, dht.ID, "", "")
	_, err := conn.Write([]byte(msg))
	if err != nil {
		Log(ERROR, "Failed to send 'ping' packet: %v", err)
	}
}

func (dht *DHTClient) HandleFind(data DHTMessage, conn *net.UDPConn) {
	// This means we've received a list of nodes we can connect to
	if data.Arguments != "" {
		ids := strings.Split(data.Arguments, ",")
		if len(ids) == 0 {
			Log(ERROR, "Malformed list of peers received")
		} else {
			// Go over list of received peer IDs and look if we know
			// anything about them. Add every new peer into list of peers
			for _, id := range ids {
				var found bool = false
				for _, peer := range dht.Peers {
					if peer.ID == id {
						found = true
					}
				}
				if !found {
					var p PeerIP
					p.ID = id
					dht.Peers = append(dht.Peers, p)
				}
			}
			for i, peer := range dht.Peers {
				var found bool = false
				for _, id := range ids {
					if peer.ID == id {
						found = true
					}
				}
				if !found {
					Log(INFO, "Removing")
					dht.Peers = append(dht.Peers[:i], dht.Peers[i+1:]...)
				}
			}
			dht.PeerChannel <- dht.Peers
			Log(DEBUG, "Received peers from %s: %s", conn.RemoteAddr().String(), data.Arguments)
			dht.UpdateLastCatch(data.Arguments)
		}
	} else {
		dht.Peers = dht.Peers[:0]
	}
}

func (dht *DHTClient) HandleRegCp(data DHTMessage, conn *net.UDPConn) {
	Log(INFO, "Control peer has been registered in Service Discovery Peer")
	// We've received a registration confirmation message from DHT bootstrap node
}

func (dht *DHTClient) HandleNode(data DHTMessage, conn *net.UDPConn) {
	// We've received an IPs associated with target node
	Log(DEBUG, "Received IPs from %s: %v", data.Id, data.Arguments)
	for i, peer := range dht.Peers {
		if peer.ID == data.Id {
			ips := strings.Split(data.Arguments, "|")
			var list []*net.UDPAddr
			for _, addr := range ips {
				if addr == "" {
					continue
				}
				ip, err := net.ResolveUDPAddr("udp", addr)
				if err != nil {
					Log(ERROR, "Failed to resolve address of peer: %v", err)
					continue
				}
				list = append(list, ip)
			}
			dht.Peers[i].Ips = list
		}
	}
}

func (dht *DHTClient) NotifyPeerAboutProxy(id string) {
	Log(INFO, "Notifying %s about proxy", id)

}

func (dht *DHTClient) HandleCp(data DHTMessage, conn *net.UDPConn) {
	// We've received information about proxy
	if data.Query == "0" || data.Query == "" {
		return
	}
	Log(INFO, "Received forwarder %s", data.Query)
	addr, err := net.ResolveUDPAddr("udp", data.Query)
	if err != nil {
		Log(ERROR, "Received invalid forwarder: %v", err)
		return
	}
	var fwd Forwarder
	fwd.Addr = addr
	fwd.DestinationID = data.Arguments
	dht.ProxyChannel <- fwd
	found := false
	for _, f := range dht.Forwarders {
		if f.Addr.String() == fwd.Addr.String() && f.DestinationID == fwd.DestinationID {
			found = true
		}
	}
	if !found {
		dht.Forwarders = append(dht.Forwarders, fwd)
	}
	/*
		msg := dht.Compose(CMD_NOTIFY, dht.ID, dht.ID, data.Id)
		for _, conn := range dht.Connection {
			if dht.Shutdown {
				continue
			}
			_, err := conn.Write([]byte(msg))
			if err != nil {
				Log(ERROR, "Failed to send 'node' request to %s: %v", conn.RemoteAddr().String(), err)
			}
		}
	*/

	/*
		Log(INFO, "Received control peer %s. Saving", data.Arguments)
		var found bool = false
		for _, fwd := range dht.Forwarders {
			if fwd.Addr.String() == data.Arguments && fwd.DestinationID == data.Id {
				found = true
			}
		}
		if !found {
			var fwd Forwarder
			a, err := net.ResolveUDPAddr("udp", data.Arguments)
			if err != nil {
				Log(ERROR, "Failed to resolve UDP Address for proxy %s", data.Arguments)
			} else {
				fwd.Addr = a
				fwd.DestinationID = data.Id
				dht.Forwarders = append(dht.Forwarders, fwd)
				Log(DEBUG, "Control peer has been added to the list of forwarders")
				Log(DEBUG, "Sending notify request back to the DHT")
				msg := dht.Compose(CMD_NOTIFY, dht.ID, dht.ID, data.Id)
				for _, conn := range dht.Connection {
					if dht.Shutdown {
						continue
					}
					_, err := conn.Write([]byte(msg))
					if err != nil {
						Log(ERROR, "Failed to send 'node' request to %s: %v", conn.RemoteAddr().String(), err)
					}
				}
			}
		}
	*/
}

func (dht *DHTClient) HandleNotify(data DHTMessage, conn *net.UDPConn) {
	// Notify means we should ask DHT bootstrap node for a control peer
	// in order to connect to a node that can't reach us
	// TODO: Fix this
	var l []*net.UDPAddr
	dht.RequestControlPeer(data.Id, l)
}

func (dht *DHTClient) HandleStop(data DHTMessage, conn *net.UDPConn) {
	if data.Arguments != "" {
		// We need to stop particular peer by changing it's state to
		// P_DISCONNECT
		Log(INFO, "Stop command for %s", data.Arguments)
		dht.RemovePeerChan <- data.Arguments
	} else {
		conn.Close()
	}
}

func (dht *DHTClient) HandleDHCP(data DHTMessage, conn *net.UDPConn) {
	if data.Arguments == "ok" {
		Log(INFO, "DHCP Registration confirmed")
		return
	} else {
		Log(INFO, "Received DHCP Information")
	}
	ip, ipnet, err := net.ParseCIDR(data.Arguments)
	if err != nil {
		Log(ERROR, "Failed to parse received DHCP packet: %v", err)
		return
	}
	Log(INFO, "Saving IP/Net data: %s", ip)
	dht.IP = ip
	dht.Network = ipnet
}

func (dht *DHTClient) HandleUnknown(data DHTMessage, conn *net.UDPConn) {
	Log(WARNING, "DHT server refuses our identity")
	if dht.State == D_CONNECTING || dht.State == D_RECONNECTING {
		time.Sleep(3 * time.Second)
	}
	dht.State = D_RECONNECTING
	Log(INFO, "Restoring connection to a DHT bootstrap node")
	err := dht.Handshake(conn)
	if err != nil {
		Log(ERROR, "Failed to send new handshake packet")
	}
}

func (dht *DHTClient) HandleError(data DHTMessage, conn *net.UDPConn) {
	e, exists := ErrorList[ErrorType(data.Arguments)]
	if !exists {
		Log(ERROR, "Unknown error were received from DHT: %s", data.Arguments)
	} else {
		Log(ERROR, "DHT returned error: %s", e.Error())
	}
}

// This method initializes DHT by splitting list of routers and connect to each one
func (dht *DHTClient) Initialize(config *DHTClient, ips []net.IP, peerChan chan []PeerIP, proxyChan chan Forwarder) *DHTClient {
	dht.RemovePeerChan = make(chan string)
	dht = config
	dht.PeerChannel = peerChan
	dht.ProxyChannel = proxyChan
	routers := strings.Split(dht.Routers, ",")
	dht.FailedRouters = make([]string, len(routers))
	dht.ResponseHandlers = make(map[string]DHTResponseCallback)
	if dht.Mode != MODE_CP && dht.Mode != MODE_CLIENT {
		dht.Mode = MODE_CLIENT
	}
	if dht.Mode == MODE_CLIENT {
		Log(INFO, "DHT operating in CLIENT mode")
		dht.ResponseHandlers[CMD_NODE] = dht.HandleNode
		dht.ResponseHandlers[CMD_CP] = dht.HandleCp
		dht.ResponseHandlers[CMD_NOTIFY] = dht.HandleNotify
		dht.ResponseHandlers[CMD_STOP] = dht.HandleStop
	} else {
		Log(INFO, "DHT operating in CONTROL PEER mode")
		dht.ResponseHandlers[CMD_REGCP] = dht.HandleRegCp
	}
	dht.ResponseHandlers[CMD_DHCP] = dht.HandleDHCP
	dht.ResponseHandlers[CMD_FIND] = dht.HandleFind
	dht.ResponseHandlers[CMD_CONN] = dht.HandleConn
	dht.ResponseHandlers[CMD_PING] = dht.HandlePing
	dht.ResponseHandlers[CMD_UNKNOWN] = dht.HandleUnknown
	dht.ResponseHandlers[CMD_ERROR] = dht.HandleError
	dht.IPList = ips
	var connected int = 0
	for _, router := range routers {
		conn, err := dht.ConnectAndHandshake(router, dht.IPList)
		if err != nil || conn == nil {
			Log(ERROR, "Failed to handshake with a DHT Server: %v", err)
			dht.FailedRouters[0] = router
		} else {
			Log(INFO, "Handshaked. Starting listener")
			dht.Connection = append(dht.Connection, conn)
			connected += 1
			go dht.ListenDHT(conn)
		}
	}
	started := time.Now()
	period := time.Duration(time.Second * 3)
	for len(dht.ID) != 36 {
		time.Sleep(time.Microsecond * 100)
		passed := time.Since(started)
		if passed > period {
			break
		}
	}
	dht.LastDHTPing = time.Now()
	if connected == 0 {
		return nil
	} else {
		return dht
	}
}

// This method register control peer on a Bootstrap node
func (dht *DHTClient) RegisterControlPeer() {
	for len(dht.ID) != 36 {
		time.Sleep(1 * time.Second)
	}
	var req DHTMessage
	var err error
	req.Id = dht.ID
	req.Query = "0"
	req.Command = CMD_REGCP
	req.Arguments = fmt.Sprintf("%d", dht.P2PPort)
	var b bytes.Buffer
	if err := bencode.Marshal(&b, req); err != nil {
		Log(ERROR, "Failed to Marshal bencode %v", err)
		return
	}
	// TODO: Optimize types here
	msg := b.String()
	for _, conn := range dht.Connection {
		if dht.Shutdown {
			continue
		}
		_, err = conn.Write([]byte(msg))
		if err != nil {
			Log(ERROR, "Failed to send packet: %v", err)
			conn.Close()
			return
		}
	}
}

// This method request a new control peer for particular host
func (dht *DHTClient) RequestControlPeer(id string, omit []*net.UDPAddr) {
	var req DHTMessage
	var err error
	req.Id = dht.ID
	req.Query = ""
	// Collect list of failed forwarders
	for _, fwd := range omit {
		req.Query += fwd.String() + "|"
	}
	req.Command = CMD_CP
	req.Arguments = id
	var b bytes.Buffer
	if err := bencode.Marshal(&b, req); err != nil {
		Log(ERROR, "Failed to Marshal bencode %v", err)
		return
	}
	msg := b.String()
	// TODO: Move sending to a separate method
	for _, conn := range dht.Connection {
		if dht.Shutdown {
			continue
		}
		_, err = conn.Write([]byte(msg))
		if err != nil {
			Log(ERROR, "Failed to send packet: %v", err)
			conn.Close()
			return
		}
	}
}

func (dht *DHTClient) ReportControlPeerLoad(amount int) {
	var req DHTMessage
	req.Id = dht.ID
	req.Command = CMD_LOAD
	req.Arguments = fmt.Sprintf("%d", amount)
	var b bytes.Buffer
	if err := bencode.Marshal(&b, req); err != nil {
		Log(ERROR, "Failed to Marshal bencode %v", err)
		return
	}
	dht.Send(b.String())
}

func (dht *DHTClient) Send(msg string) bool {
	for _, conn := range dht.Connection {
		if dht.Shutdown {
			continue
		}
		_, err := conn.Write([]byte(msg))
		if err != nil {
			Log(ERROR, "Failed to send DHT packet: %v", err)
			return false
		}
	}
	return true
}

// Request an IP from DHT. DHT Server will understand empty query field
// and send IP in response
func (dht *DHTClient) RequestIP() {
	Log(INFO, "Sending DHCP request")
	req := dht.Compose(CMD_DHCP, dht.ID, "", "")
	dht.Send(req)
}

// Notify DHT about configured IP and netmask
func (dht *DHTClient) SendIP(ip string, mask string) {
	Log(INFO, "Sending DHCP information")
	req := dht.Compose(CMD_DHCP, dht.ID, ip, mask)
	dht.Send(req)
}

func (dht *DHTClient) Stop() {
	dht.Shutdown = true
	var req DHTMessage
	req.Id = dht.ID
	req.Command = CMD_STOP
	req.Arguments = "0"
	var b bytes.Buffer
	if err := bencode.Marshal(&b, req); err != nil {
		Log(ERROR, "Failed to Marshal bencode %v", err)
		return
	}
	msg := b.String()
	for _, conn := range dht.Connection {
		conn.Write([]byte(msg))
	}
}

func (dht *DHTClient) ReadFromDHT() {
	for !dht.Shutdown {

	}
}

func (dht *DHTClient) ReadData() []byte {
	buf := <-dht.DataChannel
	Log(INFO, "READ")
	return buf
}

func (dht *DHTClient) WriteData([]byte) {

}

// 2.0
func (dht *DHTClient) ReadPeers() {

}

func (dht *DHTClient) BlacklistForwarder(addr *net.UDPAddr) {
	dht.ForwardersLock.Lock()
	// Remove it from list of cached forwarders
	for i, fwd := range dht.Forwarders {
		if fwd.Addr.String() == addr.String() {
			dht.Forwarders = append(dht.Forwarders[:i], dht.Forwarders[i+1:]...)
			break
		}
	}
	found := false
	for _, fwd := range dht.ProxyBlacklist {
		if fwd.String() == addr.String() {
			found = true
		}
	}
	if !found {
		dht.ProxyBlacklist = append(dht.ProxyBlacklist, addr)
	}
	dht.ForwardersLock.Unlock()
	runtime.Gosched()
}

func (dht *DHTClient) CleanForwarderBlacklist() {
	Log(DEBUG, "Cleaning forwarders blacklist")
	dht.ProxyBlacklist = dht.ProxyBlacklist[:0]
}
