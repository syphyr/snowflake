/*
Package snowflake_proxy provides functionality for creating, starting, and stopping a snowflake
proxy.

To run a proxy, you must first create a proxy configuration. Unconfigured fields
will be set to the defined defaults.

	proxy := snowflake_proxy.SnowflakeProxy{
		BrokerURL: "https://snowflake-broker.example.com",
		STUNURL: "stun:stun.l.google.com:19302",
		// ...
	}

You may then start and stop the proxy. Stopping the proxy will close existing connections and
the proxy will not poll for more clients.

	go func() {
		err := proxy.Start()
		// handle error
	}

	// ...

	proxy.Stop()
*/
package snowflake_proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"

	"github.com/gorilla/websocket"
	"github.com/pion/transport/v3/stdnet"
	"github.com/pion/webrtc/v4"

	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/constants"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/event"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/namematcher"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/task"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/util"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/websocketconn"
)

const (
	DefaultPollInterval = 5 * time.Second
	DefaultBrokerURL    = "https://snowflake-broker.torproject.net/"
	DefaultNATProbeURL  = "https://snowflake-broker.torproject.net:8443/probe"
	// This is rather a "DefaultDefaultRelayURL"
	DefaultRelayURL  = "wss://snowflake.torproject.net/"
	DefaultSTUNURL   = "stun:stun.l.google.com:19302,stun:stun.voip.blackberry.com:3478"
	DefaultProxyType = "standalone"
)

const (
	// NATUnknown is set if the proxy cannot connect to probetest.
	NATUnknown = "unknown"

	// NATRestricted is set if the proxy times out when connecting to a symmetric NAT.
	NATRestricted = "restricted"

	// NATUnrestricted is set if the proxy successfully connects to a symmetric NAT.
	NATUnrestricted = "unrestricted"
)

const (
	// Amount of time after sending an SDP answer before the proxy assumes the
	// client is not going to connect
	dataChannelTimeout = 20 * time.Second

	// Maximum number of bytes to be read from an HTTP request
	readLimit = 100000

	sessionIDLength = 16
)

const bufferedAmountLowThreshold uint64 = 256 * 1024 // 256 KB

var broker *SignalingServer

var currentNATTypeAccess = &sync.RWMutex{}

// currentNATType describes local network environment.
// Obtain currentNATTypeAccess before access.
var currentNATType = NATUnknown

func getCurrentNATType() string {
	currentNATTypeAccess.RLock()
	defer currentNATTypeAccess.RUnlock()
	return currentNATType
}

func setCurrentNATType(newType string) {
	currentNATTypeAccess.Lock()
	defer currentNATTypeAccess.Unlock()
	currentNATType = newType
}

var (
	tokens *tokens_t
	config webrtc.Configuration
	client http.Client
)

type GeoIP interface {
	GetCountryByAddr(net.IP) (string, bool)
}

// SnowflakeProxy is used to configure an embedded
// Snowflake in another Go application.
// For some more info also see CLI parameter descriptions in README.
type SnowflakeProxy struct {
	// How often to ask the broker for a new client
	PollInterval time.Duration
	// Capacity is the maximum number of clients a Snowflake will serve.
	// Proxies with a capacity of 0 will accept an unlimited number of clients.
	Capacity uint
	// STUNURL is the URLs (comma-separated) of the STUN server the proxy will use
	STUNURL string
	// BrokerURL is the URL of the Snowflake broker
	BrokerURL string
	// KeepLocalAddresses indicates whether local SDP candidates will be sent to the broker
	KeepLocalAddresses bool
	// RelayURL is the default `URL` of the server (relay)
	// that this proxy will forward client connections to,
	// in case the broker itself did not specify the said URL
	RelayURL string
	// OutboundAddress specify an IP address to use as SDP host candidate
	OutboundAddress string
	// EphemeralMinPort and EphemeralMaxPort limit the range of ports that
	// ICE UDP connections may allocate from.
	// When specifying the range, make sure it's at least 2x as wide
	// as the amount of clients that you are hoping to serve concurrently
	// (see the `Capacity` property).
	EphemeralMinPort uint16
	EphemeralMaxPort uint16
	// RelayDomainNamePattern is the pattern specify allowed domain name for relay
	// If the pattern starts with ^ then an exact match is required.
	// The rest of pattern is the suffix of domain name.
	// There is no look ahead assertion when matching domain name suffix,
	// thus the string prepend the suffix does not need to be empty or ends with a dot.
	RelayDomainNamePattern string
	// AllowProxyingToPrivateAddresses determines whether to allow forwarding
	// client connections to private IP addresses.
	// Useful when a Snowflake server (relay) is hosted on the same private network
	// as this proxy.
	AllowProxyingToPrivateAddresses bool
	AllowNonTLSRelay                bool
	// NATProbeURL is the URL of the probe service we use for NAT checks
	NATProbeURL string
	// NATTypeMeasurementInterval is time before NAT type is retested
	NATTypeMeasurementInterval time.Duration
	// ProxyType is the type reported to the broker, if not provided it "standalone" will be used
	ProxyType       string
	EventDispatcher event.SnowflakeEventDispatcher
	shutdown        chan struct{}

	// SummaryInterval is the time interval at which proxy stats will be logged
	SummaryInterval time.Duration

	// GeoIP will be used to detect the country of the clients if provided
	GeoIP GeoIP

	periodicProxyStats *periodicProxyStats
	bytesLogger        bytesLogger
}

// Checks whether an IP address is a remote address for the client
func isRemoteAddress(ip net.IP) bool {
	return !(util.IsLocal(ip) || ip.IsUnspecified() || ip.IsLoopback())
}

func genSessionID() string {
	buf := make([]byte, sessionIDLength)
	_, err := rand.Read(buf)
	if err != nil {
		panic(err.Error())
	}
	return strings.TrimRight(base64.StdEncoding.EncodeToString(buf), "=")
}

func limitedRead(r io.Reader, limit int64) ([]byte, error) {
	p, err := io.ReadAll(&io.LimitedReader{R: r, N: limit + 1})
	if err != nil {
		return p, err
	} else if int64(len(p)) == limit+1 {
		return p[0:limit], io.ErrUnexpectedEOF
	}
	return p, err
}

// SignalingServer keeps track of the SignalingServer in use by the Snowflake
type SignalingServer struct {
	url       *url.URL
	transport http.RoundTripper
}

func newSignalingServer(rawURL string) (*SignalingServer, error) {
	var err error
	s := new(SignalingServer)
	s.url, err = url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid broker url: %s", err)
	}

	s.transport = http.DefaultTransport.(*http.Transport)
	s.transport.(*http.Transport).ResponseHeaderTimeout = 30 * time.Second

	return s, nil
}

// Post sends a POST request to the SignalingServer
func (s *SignalingServer) Post(path string, payload io.Reader) ([]byte, error) {
	req, err := http.NewRequest("POST", path, payload)
	if err != nil {
		return nil, err
	}

	resp, err := s.transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote returned status code %d", resp.StatusCode)
	}

	defer resp.Body.Close()
	return limitedRead(resp.Body, readLimit)
}

// pollOffer communicates the proxy's capabilities with broker
// and retrieves a compatible SDP offer and relay URL.
func (s *SignalingServer) pollOffer(sid string, proxyType string, acceptedRelayPattern string) (*webrtc.SessionDescription, string) {
	brokerPath := s.url.ResolveReference(&url.URL{Path: "proxy"})

	numClients := int((tokens.count() / 8) * 8) // Round down to 8
	currentNATTypeLoaded := getCurrentNATType()
	body, err := messages.EncodeProxyPollRequestWithRelayPrefix(sid, proxyType, currentNATTypeLoaded, numClients, acceptedRelayPattern)
	if err != nil {
		log.Printf("Error encoding poll message: %s", err.Error())
		return nil, ""
	}

	resp, err := s.Post(brokerPath.String(), bytes.NewBuffer(body))
	if err != nil {
		log.Printf("error polling broker: %s", err.Error())
	}

	offer, _, relayURL, err := messages.DecodePollResponseWithRelayURL(resp)
	if err != nil {
		log.Printf("Error reading broker response: %s", err.Error())
		log.Printf("body: %s", resp)
		return nil, ""
	}
	if offer != "" {
		offer, err := util.DeserializeSessionDescription(offer)
		if err != nil {
			log.Printf("Error processing session description: %s", err.Error())
			return nil, ""
		}
		return offer, relayURL
	}
	return nil, ""
}

// sendAnswer encodes an SDP answer, sends it to the broker
// and wait for its response
func (s *SignalingServer) sendAnswer(sid string, pc *webrtc.PeerConnection) error {
	ld := pc.LocalDescription()
	answer, err := util.SerializeSessionDescription(ld)
	if err != nil {
		return err
	}

	body, err := messages.EncodeAnswerRequest(answer, sid)
	if err != nil {
		return err
	}

	brokerPath := s.url.ResolveReference(&url.URL{Path: "answer"})
	resp, err := s.Post(brokerPath.String(), bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("error sending answer to broker: %s", err.Error())
	}

	success, err := messages.DecodeAnswerResponse(resp)
	if err != nil {
		return err
	}
	if !success {
		return fmt.Errorf("broker returned client timeout")
	}

	return nil
}

func copyLoop(c1 io.ReadWriteCloser, c2 io.ReadWriteCloser, shutdown chan struct{}) {
	var once sync.Once
	defer c2.Close()
	defer c1.Close()
	done := make(chan struct{})
	copyer := func(dst io.ReadWriteCloser, src io.ReadWriteCloser) {
		// Experimentally each usage of buffer has been observed to be lower than
		// 2K; io.Copy defaults to 32K.
		// This is probably determined by MTU in the server's `newHTTPHandler`.
		size := 2 * 1024
		buffer := make([]byte, size)
		// Ignore io.ErrClosedPipe because it is likely caused by the
		// termination of copyer in the other direction.
		if _, err := io.CopyBuffer(dst, src, buffer); err != nil && err != io.ErrClosedPipe {
			log.Printf("io.CopyBuffer inside CopyLoop generated an error: %v", err)
		}
		once.Do(func() {
			close(done)
		})
	}

	go copyer(c1, c2)
	go copyer(c2, c1)

	select {
	case <-done:
	case <-shutdown:
	}
	log.Println("copy loop ended")
}

// We pass conn.RemoteAddr() as an additional parameter, rather than calling
// conn.RemoteAddr() inside this function, as a workaround for a hang that
// otherwise occurs inside conn.pc.RemoteDescription() (called by RemoteAddr).
// https://bugs.torproject.org/18628#comment:8
func (sf *SnowflakeProxy) datachannelHandler(conn *webRTCConn, remoteIP net.IP, relayURL string) {
	defer conn.Close()
	defer tokens.ret()

	if relayURL == "" {
		relayURL = sf.RelayURL
	}

	wsConn, err := connectToRelay(relayURL, remoteIP)
	if err != nil {
		log.Print(err)
		return
	}
	defer wsConn.Close()

	copyLoop(conn, wsConn, sf.shutdown)
	log.Printf("datachannelHandler ends")
}

func connectToRelay(relayURL string, remoteIP net.IP) (*websocketconn.Conn, error) {
	u, err := url.Parse(relayURL)
	if err != nil {
		return nil, fmt.Errorf("invalid relay url: %s", err)
	}

	if remoteIP != nil {
		// Encode client IP address in relay URL
		q := u.Query()
		q.Set("client_ip", remoteIP.String())
		u.RawQuery = q.Encode()
	} else {
		log.Printf("no remote address given in websocket")
	}

	ws, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("error dialing relay: %s = %s", u.String(), err)
	}

	wsConn := websocketconn.New(ws)
	log.Printf("Connected to relay: %v", relayURL)
	return wsConn, nil
}

type dataChannelHandlerWithRelayURL struct {
	RelayURL string
	sf       *SnowflakeProxy
}

func (d dataChannelHandlerWithRelayURL) datachannelHandler(conn *webRTCConn, remoteIP net.IP) {
	d.sf.datachannelHandler(conn, remoteIP, d.RelayURL)
}

func (sf *SnowflakeProxy) makeWebRTCAPI() *webrtc.API {
	settingsEngine := webrtc.SettingEngine{}

	if !sf.KeepLocalAddresses {
		settingsEngine.SetIPFilter(func(ip net.IP) (keep bool) {
			// `IsLoopback()` and `IsUnspecified` are likely not neded here,
			// but let's keep them just in case.
			// FYI there is similar code in other files in this project.
			keep = !util.IsLocal(ip) && !ip.IsLoopback() && !ip.IsUnspecified()
			return
		})
	}
	settingsEngine.SetIncludeLoopbackCandidate(sf.KeepLocalAddresses)

	// Use the SetNet setting https://pkg.go.dev/github.com/pion/webrtc/v3#SettingEngine.SetNet
	// to get snowflake working in shadow (where the AF_NETLINK family is not implemented).
	// These two lines of code functionally revert a new change in pion by silently ignoring
	// when net.Interfaces() fails, rather than throwing an error
	vnet, _ := stdnet.NewNet()
	settingsEngine.SetNet(vnet)

	if sf.EphemeralMinPort != 0 && sf.EphemeralMaxPort != 0 {
		err := settingsEngine.SetEphemeralUDPPortRange(sf.EphemeralMinPort, sf.EphemeralMaxPort)
		if err != nil {
			log.Fatal("Invalid port range: min > max")
		}
	}

	if sf.OutboundAddress != "" {
		// replace SDP host candidates with the given IP without validation
		// still have server reflexive candidates to fall back on
		settingsEngine.SetNAT1To1IPs([]string{sf.OutboundAddress}, webrtc.ICECandidateTypeHost)
	}

	settingsEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	settingsEngine.SetDTLSInsecureSkipHelloVerify(true)

	return webrtc.NewAPI(webrtc.WithSettingEngine(settingsEngine))
}

// Create a PeerConnection from an SDP offer. Blocks until the gathering of ICE
// candidates is complete and the answer is available in LocalDescription.
// Installs an OnDataChannel callback that creates a webRTCConn and passes it to
// datachannelHandler.
func (sf *SnowflakeProxy) makePeerConnectionFromOffer(
	sdp *webrtc.SessionDescription,
	config webrtc.Configuration, dataChan chan struct{},
	handler func(conn *webRTCConn, remoteIP net.IP),
) (*webrtc.PeerConnection, error) {
	api := sf.makeWebRTCAPI()
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("accept: NewPeerConnection: %s", err)
	}

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("New Data Channel %s-%d\n", dc.Label(), dc.ID())
		close(dataChan)

		pr, pw := io.Pipe()
		conn := newWebRTCConn(pc, dc, pr, sf.bytesLogger)
		remoteIP := conn.RemoteIP()

		dc.SetBufferedAmountLowThreshold(bufferedAmountLowThreshold)

		dc.OnBufferedAmountLow(func() {
			select {
			case conn.sendMoreCh <- struct{}{}:
			default:
			}
		})

		dc.OnOpen(func() {
			log.Printf("Data Channel %s-%d open\n", dc.Label(), dc.ID())
			sf.EventDispatcher.OnNewSnowflakeEvent(event.EventOnProxyClientConnected{})

			if sf.OutboundAddress != "" {
				selectedCandidatePair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
				if err != nil {
					log.Printf("Warning: couldn't get the selected candidate pair")
				}

				log.Printf("Selected Local Candidate: %s:%d", selectedCandidatePair.Local.Address, selectedCandidatePair.Local.Port)
				if sf.OutboundAddress != selectedCandidatePair.Local.Address {
					log.Printf("Warning: the IP address provided by --outbound-address is not used for establishing peerconnection")
				}
			}
		})
		dc.OnClose(func() {
			// Make sure that the `Write()`s are not blocked any more.
			dc.OnBufferedAmountLow(func() {})
			close(conn.sendMoreCh)

			conn.lock.Lock()
			defer conn.lock.Unlock()
			log.Printf("Data Channel %s-%d close\n", dc.Label(), dc.ID())

			country := ""
			if sf.GeoIP != nil && !reflect.ValueOf(sf.GeoIP).IsNil() && remoteIP != nil {
				country, _ = sf.GeoIP.GetCountryByAddr(remoteIP)
			}
			sf.EventDispatcher.OnNewSnowflakeEvent(event.EventOnProxyConnectionOver{Country: country})

			conn.dc = nil
			dc.Close()
			pw.Close()
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			n, err := pw.Write(msg.Data)
			if err != nil {
				if inErr := pw.CloseWithError(err); inErr != nil {
					log.Printf("close with error generated an error: %v", inErr)
				}

				return
			}

			conn.bytesLogger.AddOutbound(int64(n))

			if n != len(msg.Data) {
				// XXX: Maybe don't panic here and log an error instead?
				panic("short write")
			}
		})

		go handler(conn, remoteIP)
	})
	// As of v3.0.0, pion-webrtc uses trickle ICE by default.
	// We have to wait for candidate gathering to complete
	// before we send the offer
	done := webrtc.GatheringCompletePromise(pc)
	err = pc.SetRemoteDescription(*sdp)
	if err != nil {
		if inerr := pc.Close(); inerr != nil {
			log.Printf("unable to call pc.Close after pc.SetRemoteDescription with error: %v", inerr)
		}
		return nil, fmt.Errorf("accept: SetRemoteDescription: %s", err)
	}

	log.Println("Generating answer...")
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		if inerr := pc.Close(); inerr != nil {
			log.Printf("ICE gathering has generated an error when calling pc.Close: %v", inerr)
		}
		return nil, err
	}

	err = pc.SetLocalDescription(answer)
	if err != nil {
		if err = pc.Close(); err != nil {
			log.Printf("pc.Close after setting local description returned : %v", err)
		}
		return nil, err
	}

	// Wait for ICE candidate gathering to complete,
	// or for whatever we managed to gather before the broker
	// responds with an error to the client offer.
	// See https://gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/-/issues/40230
	select {
	case <-done:
	case <-time.After(constants.BrokerClientTimeout * time.Second * 3 / 4):
		log.Print("ICE gathering is not yet complete, but let's send the answer" +
			" before the client times out")
	}

	log.Printf("Answer: \n\t%s", strings.ReplaceAll(pc.LocalDescription().SDP, "\n", "\n\t"))

	return pc, nil
}

// Create a new PeerConnection. Blocks until the gathering of ICE
// candidates is complete and the answer is available in LocalDescription.
func (sf *SnowflakeProxy) makeNewPeerConnection(
	config webrtc.Configuration, dataChan chan struct{},
) (*webrtc.PeerConnection, error) {
	api := sf.makeWebRTCAPI()
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("accept: NewPeerConnection: %s", err)
	}
	pc.OnConnectionStateChange(func(pcs webrtc.PeerConnectionState) {
		log.Printf("NAT check: WebRTC: OnConnectionStateChange: %v", pcs)
	})

	// Must create a data channel before creating an offer
	// https://github.com/pion/webrtc/wiki/Release-WebRTC@v3.0.0#a-data-channel-is-no-longer-implicitly-created-with-a-peerconnection
	dc, err := pc.CreateDataChannel("test", &webrtc.DataChannelInit{})
	if err != nil {
		log.Printf("CreateDataChannel ERROR: %s", err)
		return nil, err
	}
	dc.OnOpen(func() {
		log.Println("WebRTC: DataChannel.OnOpen")
		close(dataChan)
	})
	dc.OnClose(func() {
		log.Println("WebRTC: DataChannel.OnClose")
		go func() {
			// A hack to make NAT testing more reliable and not mis-identify
			// as "restricted".
			// See https://gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/-/issues/40419#note_3141855.
			// Instead we should just `dc.Close()` without waiting
			// and without a goroutine.
			// (or, perhaps, `dc.Close()` is not needed at all
			// in the OnClose callback?)
			<-time.After(5 * time.Second)

			log.Print("NAT check: WebRTC: dc.Close()")
			dc.Close()
		}()
	})

	offer, err := pc.CreateOffer(nil)
	// TODO: Potentially timeout and retry if ICE isn't working.
	if err != nil {
		log.Println("Failed to prepare offer", err)
		pc.Close()
		return nil, err
	}
	log.Println("Probetest: Created Offer")

	// As of v3.0.0, pion-webrtc uses trickle ICE by default.
	// We have to wait for candidate gathering to complete
	// before we send the offer
	done := webrtc.GatheringCompletePromise(pc)
	// start the gathering of ICE candidates
	err = pc.SetLocalDescription(offer)
	if err != nil {
		log.Println("Failed to apply offer", err)
		pc.Close()
		return nil, err
	}
	log.Println("Probetest: Set local description")

	// Wait for ICE candidate gathering to complete
	<-done

	return pc, nil
}

func (sf *SnowflakeProxy) runSession(sid string) {
	connectedToClient := false
	defer func() {
		if !connectedToClient {
			tokens.ret()
		}
		// Otherwise we'll `tokens.ret()` when the connection finishes.
	}()

	offer, relayURL := broker.pollOffer(sid, sf.ProxyType, sf.RelayDomainNamePattern)
	if offer == nil {
		return
	}
	log.Printf("Received Offer From Broker: \n\t%s", strings.ReplaceAll(offer.SDP, "\n", "\n\t"))

	if relayURL != "" {
		if err := checkIsRelayURLAcceptable(sf.RelayDomainNamePattern, sf.AllowProxyingToPrivateAddresses, sf.AllowNonTLSRelay, relayURL); err != nil {
			log.Printf("bad offer from broker: %v", err)
			return
		}
	}

	dataChan := make(chan struct{})
	dataChannelAdaptor := dataChannelHandlerWithRelayURL{RelayURL: relayURL, sf: sf}
	pc, err := sf.makePeerConnectionFromOffer(offer, config, dataChan, dataChannelAdaptor.datachannelHandler)
	if err != nil {
		log.Printf("error making WebRTC connection: %s", err)
		return
	}

	err = broker.sendAnswer(sid, pc)
	if err != nil {
		log.Printf("error sending answer to client through broker: %s", err)
		if inerr := pc.Close(); inerr != nil {
			log.Printf("error calling pc.Close: %v", inerr)
		}
		return
	}
	// Set a timeout on peerconnection. If the connection state has not
	// advanced to PeerConnectionStateConnected in this time,
	// destroy the peer connection and return the token.
	select {
	case <-dataChan:
		log.Println("Connection successful")
		connectedToClient = true
	case <-time.After(dataChannelTimeout):
		log.Println("Timed out waiting for client to open data channel.")
		sf.EventDispatcher.OnNewSnowflakeEvent(
			event.EventOnProxyConnectionFailed{},
		)
		if err := pc.Close(); err != nil {
			log.Printf("error calling pc.Close: %v", err)
		}
	}
}

// Returns nil if the relayURL is acceptable.
// This is a pure function.
// If the hostname in the `relayURL` is not an IP address
// (but a name instead, e.g. `localhost`),
// this function will _not_ perform a DNS request to figure out
// if the name resolves to a private IP address,
// i.e. the private / public check will effectively be skipped.
func checkIsRelayURLAcceptable(
	allowedHostNamePattern string,
	allowPrivateIPs bool,
	allowNonTLSRelay bool,
	relayURL string,
) error {
	parsedRelayURL, err := url.Parse(relayURL)
	if err != nil {
		return fmt.Errorf("bad Relay URL %w", err)
	}
	if !allowPrivateIPs {
		ip := net.ParseIP(parsedRelayURL.Hostname())
		// Otherwise it's a domain name, or an invalid IP.
		if ip != nil {
			// We should probably use a ready library for this.
			if !isRemoteAddress(ip) {
				return fmt.Errorf("rejected Relay URL: private IPs are not allowed")
			}
		}
	}
	if !allowNonTLSRelay && parsedRelayURL.Scheme != "wss" {
		return fmt.Errorf("rejected Relay URL protocol: non-TLS not allowed")
	}
	// FYI our websocket library also rejects other protocols
	// https://github.com/gorilla/websocket/blob/5e002381133d322c5f1305d171f3bdd07decf229/client.go#L174-L181
	if parsedRelayURL.Scheme != "wss" && parsedRelayURL.Scheme != "ws" {
		return fmt.Errorf("rejected Relay URL protocol: only WebSocket is allowed")
	}
	matcher := namematcher.NewNameMatcher(allowedHostNamePattern)
	if !matcher.IsMember(parsedRelayURL.Hostname()) {
		return fmt.Errorf("rejected Relay URL: hostname does not match allowed pattern \"%v\"", allowedHostNamePattern)
	}
	return nil
}

// Start configures and starts a Snowflake, fully formed and special. Configuration
// values that are unset will default to their corresponding default values.
func (sf *SnowflakeProxy) Start() error {
	var err error

	sf.EventDispatcher.OnNewSnowflakeEvent(event.EventOnProxyStarting{})
	sf.shutdown = make(chan struct{})

	// blank configurations revert to default
	if sf.PollInterval == 0 {
		sf.PollInterval = DefaultPollInterval
	}
	if sf.BrokerURL == "" {
		sf.BrokerURL = DefaultBrokerURL
	}
	if sf.RelayURL == "" {
		sf.RelayURL = DefaultRelayURL
	}
	if sf.STUNURL == "" {
		sf.STUNURL = DefaultSTUNURL
	}
	if sf.NATProbeURL == "" {
		sf.NATProbeURL = DefaultNATProbeURL
	}
	if sf.ProxyType == "" {
		sf.ProxyType = DefaultProxyType
	}
	if sf.EventDispatcher == nil {
		sf.EventDispatcher = event.NewSnowflakeEventDispatcher()
	}

	sf.bytesLogger = newBytesSyncLogger()
	sf.periodicProxyStats = newPeriodicProxyStats(sf.SummaryInterval, sf.EventDispatcher, sf.bytesLogger)
	sf.EventDispatcher.AddSnowflakeEventListener(sf.periodicProxyStats)

	broker, err = newSignalingServer(sf.BrokerURL)
	if err != nil {
		return fmt.Errorf("error configuring broker: %s", err)
	}

	_, err = url.Parse(sf.STUNURL)
	if err != nil {
		return fmt.Errorf("invalid stun url: %s", err)
	}
	_, err = url.Parse(sf.RelayURL)
	if err != nil {
		return fmt.Errorf("invalid default relay url: %s", err)
	}

	if !namematcher.IsValidRule(sf.RelayDomainNamePattern) {
		return fmt.Errorf("invalid relay domain name pattern")
	}

	if sf.EphemeralMaxPort != 0 {
		rangeWidth := sf.EphemeralMaxPort - sf.EphemeralMinPort
		expectedNumConcurrentClients := sf.Capacity
		if sf.Capacity == 0 {
			// Just a guess, since 0 means "unlimited".
			expectedNumConcurrentClients = 10
		}
		// See https://forum.torproject.org/t/remote-returned-status-code-400/15026/9?u=wofwca
		if uint(rangeWidth) < expectedNumConcurrentClients*2 {
			log.Printf(
				"Warning: ephemeral ports range seems narrow (%v-%v) "+
					"for the client capacity (%v). "+
					"Some client connections might fail. "+
					"Please widen the port range, or limit the 'capacity'.",
				sf.EphemeralMinPort,
				sf.EphemeralMaxPort,
				sf.Capacity,
			)
			// Instead of simply printing a warning, we could look into
			// utilizing [SetICEUDPMux](https://pkg.go.dev/github.com/pion/webrtc/v4#SettingEngine.SetICEUDPMux)
			// to multiplex multiple connections over one (or more?) ports.
		}
	}

	config = webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: strings.Split(sf.STUNURL, ","),
			},
		},
	}
	tokens = newTokens(sf.Capacity)

	err = sf.checkNATType(config, sf.NATProbeURL)
	if err != nil {
		// non-fatal error. Log it and continue
		log.Printf(err.Error())
		setCurrentNATType(NATUnknown)
	}
	sf.EventDispatcher.OnNewSnowflakeEvent(event.EventOnCurrentNATTypeDetermined{CurNATType: getCurrentNATType()})

	NatRetestTask := task.Periodic{
		Interval: sf.NATTypeMeasurementInterval,
		Execute: func() error {
			return sf.checkNATType(config, sf.NATProbeURL)
		},
		// Not setting OnError would shut down the periodic task on error by default.
		OnError: func(err error) {
			log.Printf("Periodic probetest failed: %s, retaining current NAT type: %s", err.Error(), getCurrentNATType())
		},
	}

	if sf.NATTypeMeasurementInterval != 0 {
		NatRetestTask.WaitThenStart()
		defer NatRetestTask.Close()
	}

	ticker := time.NewTicker(sf.PollInterval)
	defer ticker.Stop()

	for ; true; <-ticker.C {
		select {
		case <-sf.shutdown:
			return nil
		default:
			tokens.get()
			sessionID := genSessionID()
			sf.runSession(sessionID)
		}
	}
	return nil
}

// Stop closes all existing connections and shuts down the Snowflake.
func (sf *SnowflakeProxy) Stop() {
	close(sf.shutdown)
}

// checkNATType use probetest to determine NAT compatability by
// attempting to connect with a known symmetric NAT. If success,
// it is considered "unrestricted". If timeout it is considered "restricted"
func (sf *SnowflakeProxy) checkNATType(config webrtc.Configuration, probeURL string) error {
	log.Printf("Checking our NAT type, contacting NAT check probe server at \"%v\"...", probeURL)

	probe, err := newSignalingServer(probeURL)
	if err != nil {
		return fmt.Errorf("Error parsing url: %w", err)
	}

	dataChan := make(chan struct{})
	pc, err := sf.makeNewPeerConnection(config, dataChan)
	if err != nil {
		return fmt.Errorf("Error making WebRTC connection: %w", err)
	}
	defer func() {
		if err := pc.Close(); err != nil {
			log.Printf("Probetest: error calling pc.Close: %v", err)
		}
	}()

	offer := pc.LocalDescription()
	log.Printf("Probetest offer: \n\t%s", strings.ReplaceAll(offer.SDP, "\n", "\n\t"))
	sdp, err := util.SerializeSessionDescription(offer)
	if err != nil {
		return fmt.Errorf("Error encoding probe message: %w", err)
	}

	// send offer
	body, err := messages.EncodePollResponse(sdp, true, "")
	if err != nil {
		return fmt.Errorf("Error encoding probe message: %w", err)
	}

	resp, err := probe.Post(probe.url.String(), bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("Error polling probe: %w", err)
	}

	sdp, _, err = messages.DecodeAnswerRequest(resp)
	if err != nil {
		return fmt.Errorf("Error reading probe response: %w", err)
	}

	answer, err := util.DeserializeSessionDescription(sdp)
	if err != nil {
		return fmt.Errorf("Error setting answer: %w", err)
	}
	log.Printf("Probetest answer: \n\t%s", strings.ReplaceAll(answer.SDP, "\n", "\n\t"))

	err = pc.SetRemoteDescription(*answer)
	if err != nil {
		return fmt.Errorf("Error setting answer: %w", err)
	}

	prevNATType := getCurrentNATType()

	log.Printf("Waiting for a test WebRTC connection with NAT check probe server to establish...")
	select {
	case <-dataChan:
		log.Printf(
			"Test WebRTC connection with NAT check probe server established!"+
				" This means our NAT is %v!",
			NATUnrestricted,
		)
		setCurrentNATType(NATUnrestricted)
	case <-time.After(dataChannelTimeout):
		log.Printf(
			"Test WebRTC connection with NAT check probe server timed out."+
				" This means our NAT is %v.",
			NATRestricted,
		)
		setCurrentNATType(NATRestricted)
	}

	log.Printf("NAT Type measurement: %v -> %v\n", prevNATType, getCurrentNATType())

	return nil
}
