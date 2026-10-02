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

	"github.com/gorilla/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/transport/v4/stdnet"
	"github.com/pion/webrtc/v4"

	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/constants"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/covertdtls"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/event"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/namematcher"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/nat"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/task"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/util"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/websocketconn"
)

const (
	DefaultPollInterval    = 5 * time.Second
	DefaultMinPollInterval = 1 * time.Second
	DefaultBrokerURL       = "https://snowflake-broker.torproject.net/"
	DefaultNATProbeURL     = "https://snowflake-broker.torproject.net:8443/probe"
	// This is rather a "DefaultDefaultRelayURL"
	DefaultRelayURL  = "wss://snowflake.torproject.net/"
	DefaultSTUNURL   = "stun:stun.l.google.com:19302,stun:stun.voip.blackberry.com:3478"
	DefaultProxyType = "standalone"
)

const (
	// NATUnknown is set if the proxy cannot connect to probetest.
	NATUnknown = nat.NATUnknown

	// NATRestricted
	// Deprecated: Use NAT3Moderate or NAT3Strict instead.
	NATRestricted = nat.NATRestricted

	// NATUnrestricted
	// Deprecated: Use NAT3Open instead.
	NATUnrestricted = nat.NATUnrestricted

	NAT3Open     = nat.NAT3Open
	NAT3Moderate = nat.NAT3Moderate
	NAT3Strict   = nat.NAT3Strict
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
	// PollInterval is a deprecated, fallback value for how often to ask the broker for a new client.
	// Proxies will dynamically adjust their poll interval based on broker recommendation.
	PollInterval time.Duration
	// MinPollInterval Minimum time interval between when the proxy asks the broker for a client.
	// A proxy will never poll more frequently than the MinPollInterval, regardless of the broker
	// recommended poll interval value.
	MinPollInterval time.Duration
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
	// The name of the network interface to use for ICE candidates. If not specified, all interfaces will be used.
	Interface string
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

	// SummaryInterval is the time interval at which proxy stats will be logged.
	// A duration of 0 will disable periodic summary statistics.
	SummaryInterval time.Duration

	// GeoIP will be used to detect the country of the clients if provided
	GeoIP GeoIP

	// CovertDTLSConfig is used for configuration for randomization or mimicking (Firefox/Chrome browser) of DTLS Client Hello messages.
	CovertDTLSConfig covertdtls.CovertDTLSConfig

	periodicProxyStats *periodicProxyStats
	bytesLogger        bytesLogger

	relayReachable bool
	pollTicker     *time.Ticker
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
	url                *url.URL
	transport          http.RoundTripper
	keepLocalAddresses bool
}

func newSignalingServer(rawURL string, keepLocalAddresses bool) (*SignalingServer, error) {
	var err error
	s := new(SignalingServer)
	s.keepLocalAddresses = keepLocalAddresses
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
func (s *SignalingServer) pollOffer(sid string, proxyType string, acceptedRelayPattern string) (*messages.ProxyPollResponse, error) {
	brokerPath := s.url.ResolveReference(&url.URL{Path: "proxy"})

	numClients := int((tokens.count() / 8) * 8) // Round down to 8
	currentNATTypeLoaded := getCurrentNATType()
	req := messages.ProxyPollRequest{
		Sid:                  sid,
		Type:                 proxyType,
		NAT:                  currentNATTypeLoaded,
		Clients:              numClients,
		AcceptedRelayPattern: &acceptedRelayPattern,
	}
	body, err := req.Encode()
	if err != nil {
		return nil, err
	}

	resp, err := s.Post(brokerPath.String(), bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	pollResponse, err := messages.DecodeProxyPollResponse(resp)
	if err != nil {
		return nil, err
	}
	return pollResponse, nil
}

// sendAnswer encodes an SDP answer, sends it to the broker
// and wait for its response
func (s *SignalingServer) sendAnswer(sid string, pc *webrtc.PeerConnection) error {
	ld := pc.LocalDescription()
	if !s.keepLocalAddresses {
		ld = &webrtc.SessionDescription{
			Type: ld.Type,
			SDP:  util.StripLocalAddresses(ld.SDP),
		}
	}

	answer, err := util.SerializeSessionDescription(ld)
	if err != nil {
		return err
	}

	req := messages.ProxyAnswerRequest{
		Answer: answer,
		Sid:    sid,
	}
	body, err := req.Encode()
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

	settingsEngine.SetIncludeLoopbackCandidate(sf.KeepLocalAddresses)

	if sf.Interface != "" {
		settingsEngine.SetInterfaceFilter(func(name string) bool {
			return name == sf.Interface
		})
	}

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

	if sf.CovertDTLSConfig.Mimic || sf.CovertDTLSConfig.Randomize || len(sf.CovertDTLSConfig.Fingerprint) > 0 {
		err := covertdtls.SetCovertDTLSSettings(&sf.CovertDTLSConfig, &settingsEngine)
		if err != nil {
			log.Fatalf("CovertDTLS ERROR: %s", err)
		}
	}

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

			country := "??"
			if sf.GeoIP != nil && !reflect.ValueOf(sf.GeoIP).IsNil() && remoteIP != nil {
				if result, ok := sf.GeoIP.GetCountryByAddr(remoteIP); ok {
					country = result
				}
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

	pollResponse, err := broker.pollOffer(sid, sf.ProxyType, sf.RelayDomainNamePattern)
	if err != nil {
		log.Printf("Error polling broker: %s", err.Error())
		return
	}
	pollInterval := time.Duration(time.Duration(pollResponse.NextPoll) * time.Millisecond)
	if pollInterval <= 0 {
		pollInterval = sf.PollInterval
	}
	sf.pollTicker.Reset(max(sf.MinPollInterval, pollInterval))
	log.Printf("Poll interval set to %v", max(sf.MinPollInterval, pollInterval))
	go func() {
		if pollResponse.Offer == "" {
			return
		}
		offer, err := util.DeserializeSessionDescription(pollResponse.Offer)
		if err != nil {
			log.Printf("Error deserializing session description: %s", err.Error())
			return
		}
		log.Printf("Received offer from broker: \n\t%s", strings.ReplaceAll(offer.SDP, "\n", "\n\t"))

		if pollResponse.RelayURL != "" {
			if err := checkIsRelayURLAcceptable(sf.RelayDomainNamePattern, sf.AllowProxyingToPrivateAddresses, sf.AllowNonTLSRelay, pollResponse.RelayURL); err != nil {
				log.Printf("bad offer from broker: %v", err)
				return
			}
		}

		dataChan := make(chan struct{})
		dataChannelAdaptor := dataChannelHandlerWithRelayURL{RelayURL: pollResponse.RelayURL, sf: sf}
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
	}()
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
	if sf.MinPollInterval == 0 {
		sf.MinPollInterval = DefaultMinPollInterval
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

	if sf.SummaryInterval > 0 {
		sf.periodicProxyStats = newPeriodicProxyStats(sf.SummaryInterval, sf.EventDispatcher, sf.bytesLogger)
		sf.EventDispatcher.AddSnowflakeEventListener(sf.periodicProxyStats)
	}

	broker, err = newSignalingServer(sf.BrokerURL, sf.KeepLocalAddresses)
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
		log.Printf("Error checking NAT type: %s", err.Error())
		setCurrentNATType(NATUnknown)
	}
	sf.EventDispatcher.OnNewSnowflakeEvent(event.EventOnCurrentNATTypeDetermined{CurNATType: getCurrentNATType()})

	NatRetestTask := task.Periodic{
		Interval: sf.NATTypeMeasurementInterval,
		Execute: func() error {
			prevNATType := getCurrentNATType()
			if err := sf.checkNATType(config, sf.NATProbeURL); err != nil {
				return err
			}
			if prevNATType != getCurrentNATType() {
				sf.EventDispatcher.OnNewSnowflakeEvent(event.EventOnCurrentNATTypeDetermined{CurNATType: getCurrentNATType()})
			}
			return nil
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

	BridgeProbeRetestTask := task.ExpBackoff{
		MaxInterval: 24 * time.Hour,
		MinInterval: 5 * time.Minute,
		Execute: func() error {
			err := sf.checkBridgeReachability()
			sf.relayReachable = err == nil
			return err
		},
		OnError: func(err error) {
			log.Printf("Connection to bridge at %s failed: %s", sf.RelayURL, err.Error())
		},
	}
	BridgeProbeRetestTask.Start()
	defer BridgeProbeRetestTask.Close()

	sf.pollTicker = time.NewTicker(sf.PollInterval)
	defer sf.pollTicker.Stop()

	for ; true; <-sf.pollTicker.C {
		select {
		case <-sf.shutdown:
			return nil
		default:
			if sf.relayReachable {
				tokens.get()
				sessionID := genSessionID()
				sf.runSession(sessionID)
			}
		}
	}
	return nil
}

// Stop closes all existing connections and shuts down the Snowflake.
func (sf *SnowflakeProxy) Stop() {
	close(sf.shutdown)
}

func probeURLWithInteractiveConnectivity(probeURL, mode string) (string, error) {
	u, err := url.Parse(probeURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("InCoSim", mode)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (sf *SnowflakeProxy) checkNATType(config webrtc.Configuration, probeURL string) error {
	log.Printf("Checking our NAT type, contacting NAT check probe server at \"%v\"...", probeURL)

	prevNATType := getCurrentNATType()

	strictProbeHelperURL, err := probeURLWithInteractiveConnectivity(probeURL, "strict")
	if err != nil {
		return fmt.Errorf("unable to constuct strict nat probe helper url: %s", err)
	}

	strictConnectionSuccessful, err := sf.evaluateConnectivityWithHelper(config, strictProbeHelperURL)
	if err != nil {
		return fmt.Errorf("an error occurred during connectivity evaluation with strict connectivity helper: %s", err)
	}

	if strictConnectionSuccessful {
		setCurrentNATType(NAT3Open)
		log.Printf("NAT Type measurement: %v -> %v\n", prevNATType, getCurrentNATType())
		return nil
	}

	moderateProbeHelperURL, err := probeURLWithInteractiveConnectivity(probeURL, "moderate")
	if err != nil {
		return fmt.Errorf("unable to construct moderate nat probe helper url: %s", err)
	}

	moderateConnectionSuccessful, err := sf.evaluateConnectivityWithHelper(config, moderateProbeHelperURL)
	if err != nil {
		return fmt.Errorf("an error occurred during connectivity evaluation with moderate connectivity helper: %s", err)
	}

	if moderateConnectionSuccessful {
		setCurrentNATType(NAT3Moderate)
		log.Printf("NAT Type measurement: %v -> %v\n", prevNATType, getCurrentNATType())
		return nil
	}
	setCurrentNATType(NAT3Strict)
	log.Printf("NAT Type measurement: %v -> %v\n", prevNATType, getCurrentNATType())
	return nil
}

// evaluateConnectivityWithHelper use probetest to determine NAT compatability by
// attempting to connect with a known NAT type. If success,
// it determines its network can connect to given nat type.
func (sf *SnowflakeProxy) evaluateConnectivityWithHelper(config webrtc.Configuration, probeURL string) (bool, error) {
	log.Printf("Checking our NAT type, contacting NAT check probe server at \"%v\"...", probeURL)

	probe, err := newSignalingServer(probeURL, sf.KeepLocalAddresses)
	if err != nil {
		return false, fmt.Errorf("Error parsing url: %w", err)
	}

	dataChan := make(chan struct{})
	pc, err := sf.makeNewPeerConnection(config, dataChan)
	if err != nil {
		return false, fmt.Errorf("Error making WebRTC connection: %w", err)
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
		return false, fmt.Errorf("Error encoding probe message: %w", err)
	}

	// send offer
	pollResp := messages.ProxyPollResponse{
		Status: messages.ProxyClientMatch,
		Offer:  sdp,
	}
	body, err := pollResp.Encode()
	if err != nil {
		return false, fmt.Errorf("Error encoding probe message: %w", err)
	}

	resp, err := probe.Post(probe.url.String(), bytes.NewBuffer(body))
	if err != nil {
		return false, fmt.Errorf("Error polling probe: %w", err)
	}

	req, err := messages.DecodeProxyAnswerRequest(resp)
	if err != nil {
		return false, fmt.Errorf("Error reading probe response: %w", err)
	}

	answer, err := util.DeserializeSessionDescription(req.Answer)
	if err != nil {
		return false, fmt.Errorf("Error setting answer: %w", err)
	}
	log.Printf("Probetest answer: \n\t%s", strings.ReplaceAll(answer.SDP, "\n", "\n\t"))

	err = pc.SetRemoteDescription(*answer)
	if err != nil {
		return false, fmt.Errorf("Error setting answer: %w", err)
	}

	select {
	case <-dataChan:
		return true, nil
	case <-time.After(dataChannelTimeout):
		return false, nil
	}
}

// checkBridgeReachability makes a test connection to DefaultRelayURL to see if the proxy
// is able to make a working connection to the bridge
func (sf *SnowflakeProxy) checkBridgeReachability() error {
	wsConn, err := connectToRelay(sf.RelayURL, nil)
	if err != nil {
		return fmt.Errorf("bridge relay is unreachable: %w", err)
	}

	return wsConn.Close()
}
