package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/theodorsm/covert-dtls/pkg/fingerprints"

	"gitlab.torproject.org/tpo/anti-censorship/geoip"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/ptutil/safelog"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/covertdtls"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/event"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/version"
	sf "gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/proxy/lib"
)

const minPollInterval = 2 * time.Second

func main() {
	pollInterval := flag.Duration("poll-interval", sf.DefaultPollInterval,
		fmt.Sprint("a deprecated fallback value for often to ask the broker for a new client. Proxies will dynamically change their poll interval based on broker recommendation. Minumum value is ", minPollInterval, ". Valid time units are \"ms\", \"s\", \"m\", \"h\"."))
	capacity := flag.Uint("capacity", 0, "maximum concurrent clients (default is to accept an unlimited number of clients)")
	stunURL := flag.String("stun", sf.DefaultSTUNURL, "Comma-separated STUN server `URL`s that this proxy will use will use to, among some other things, determine its public IP address")
	logFilename := flag.String("log", "", "log `filename`. If not specified, logs will be output to stderr (console).")
	rawBrokerURL := flag.String("broker", sf.DefaultBrokerURL, "The `URL` of the broker server that the proxy will be using to find clients")
	unsafeLogging := flag.Bool("unsafe-logging", false, "keep IP addresses and other sensitive info in the logs")
	logLocalTime := flag.Bool("log-local-time", false, "Use local time for logging (default: UTC)")
	keepLocalAddresses := flag.Bool("keep-local-addresses", false, "keep local LAN address ICE candidates.\nThis is usually pointless because Snowflake clients don't usually reside on the same local network as the proxy.")
	defaultRelayURL := flag.String("relay", sf.DefaultRelayURL, "The default `URL` of the server (relay) that this proxy will forward client connections to, in case the broker itself did not specify the said URL")
	probeURL := flag.String("nat-probe-server", sf.DefaultNATProbeURL, "The `URL` of the server that this proxy will use to check its network NAT type.\nDetermining NAT type helps to understand whether this proxy is compatible with certain clients' NAT")
	iface := flag.String("interface", "", "Set network interface to use for peer connections with clients. If not specified, all interfaces will be used.\nThis option is an alternative and more robust way of setting --outbound-address and should be preferred to that option.")
	outboundAddress := flag.String("outbound-address", "", "Prefer the given `address` as the outbound address for peer connections with clients.\nFor bridges and relays running snowflake-proxy on the same device, an alternate IP address should be set for snowflake-proxy to mitigate its IP being censored when the bridge or relay IP is censored.")
	allowedRelayHostNamePattern := flag.String("allowed-relay-hostname-pattern", "snowflake.torproject.net$", "this proxy will only be allowed to forward client connections to relays (servers) whose URL matches this pattern.\nNote that a pattern \"example.com$\" will match \"subdomain.example.com\" as well as \"other-domain-example.com\".\nIn order to only match \"example.com\", prefix the pattern with \"^\": \"^example.com$\"")
	allowProxyingToPrivateAddresses := flag.Bool("allow-proxying-to-private-addresses", false, "allow forwarding client connections to private IP addresses.\nUseful when a Snowflake server (relay) is hosted on the same private network as this proxy.")
	allowNonTLSRelay := flag.Bool("allow-non-tls-relay", false, "allow this proxy to pass client's data to the relay in an unencrypted form.\nThis is only useful if the relay doesn't support encryption, e.g. for testing / development purposes.")
	NATTypeMeasurementInterval := flag.Duration("nat-retest-interval", time.Hour*24,
		"the time interval between NAT type is retests (see \"nat-probe-server\"). 0s disables retest. Valid time units are \"s\", \"m\", \"h\".")
	summaryInterval := flag.Duration("summary-interval", time.Hour,
		"the time interval between summary log outputs, 0s disables summaries. Valid time units are \"s\", \"m\", \"h\".")
	disableStatsLogger := flag.Bool("disable-stats-logger", false, "disable the exposing mechanism for stats using logs")
	enableMetrics := flag.Bool("metrics", false, "enable the exposing mechanism for stats using metrics")
	metricsAddress := flag.String("metrics-address", "localhost", "set listen `address` for metrics service")
	metricsPort := flag.Int("metrics-port", 9999, "set port for the metrics service")
	verboseLogging := flag.Bool("verbose", false, "increase log verbosity")
	ephemeralPortsRangeFlag := flag.String("ephemeral-ports-range", "", "Set the `range` of ports used for client connections (format:\"<min>:<max>\").\nUseful in conjunction with port forwarding, in order to make the proxy NAT type \"unrestricted\".\nIf omitted, the ports will be chosen automatically from a wide range.\nWhen specifying the range, make sure it's at least 2x as wide as the amount of clients that you are hoping to serve concurrently (see the \"capacity\" flag).")
	geoipDatabase := flag.String("geoipdb", "/usr/share/tor/geoip", "path to correctly formatted geoip database mapping IPv4 address ranges to country codes")
	geoip6Database := flag.String("geoip6db", "/usr/share/tor/geoip6", "path to correctly formatted geoip database mapping IPv6 address ranges to country codes")
	versionFlag := flag.Bool("version", false, "display version info to stderr and quit")
	covertDTLSConfig := flag.String("covertdtls-config", "randomizemimic", "Configuration of DTLS mimicking and randomization: mimic, randomize, randomizemimic, disable. Using randomizemimic is recommended for best stability and provides good fingerprint-resistance against block-listing. Using randomize is less stable, but provides better fingerprint-resistance against allow-listing. Use disable to turn this feature off.")
	covertDTLSfingerprint := flag.String("covertdtls-fingerprint", "", "Mimicking of a raw DTLS fingerprint")

	var ephemeralPortsRange []uint16 = []uint16{0, 0}

	flag.Parse()

	if *versionFlag {
		fmt.Fprintf(os.Stderr, "snowflake-proxy %s", version.ConstructResult())
		os.Exit(0)
	}

	if *pollInterval < minPollInterval {
		log.Fatalf("poll-interval must be >= %v", minPollInterval)
	}

	if *outboundAddress != "" && *keepLocalAddresses {
		log.Fatal("Cannot keep local address candidates when outbound address is specified")
	}

	eventLogger := event.NewSnowflakeEventDispatcher()

	if *ephemeralPortsRangeFlag != "" {
		ephemeralPortsRangeParts := strings.Split(*ephemeralPortsRangeFlag, ":")
		if len(ephemeralPortsRangeParts) == 2 {
			ephemeralMinPort, err := strconv.ParseUint(ephemeralPortsRangeParts[0], 10, 16)
			if err != nil {
				log.Fatal(err)
			}

			ephemeralMaxPort, err := strconv.ParseUint(ephemeralPortsRangeParts[1], 10, 16)
			if err != nil {
				log.Fatal(err)
			}

			if ephemeralMinPort == 0 || ephemeralMaxPort == 0 {
				log.Fatal("Ephemeral port cannot be zero")
			}
			if ephemeralMinPort > ephemeralMaxPort {
				log.Fatal("Invalid port range: min > max")
			}

			ephemeralPortsRange = []uint16{uint16(ephemeralMinPort), uint16(ephemeralMaxPort)}
		} else {
			log.Fatalf("Bad range port format: %v", *ephemeralPortsRangeFlag)
		}
	}

	gip, err := geoip.New(*geoipDatabase, *geoip6Database)
	if *enableMetrics && err != nil {
		// The geoip DB is only used for metrics, let's only report the error if enabled
		log.Println("Error loading geoip db for country based metrics:", err)
	}

	var cDTLSconfig covertdtls.CovertDTLSConfig

	if *covertDTLSConfig != "" {
		cDTLSconfig, err = covertdtls.ParseCovertDTLSConfigString(*covertDTLSConfig)
		if err != nil {
			log.Println("Error parsing covertdtls-config, feature has been disabled:", err)
		}
	}
	if *covertDTLSfingerprint != "" {
		cDTLSconfig.Fingerprint = fingerprints.ClientHelloFingerprint(*covertDTLSfingerprint)
	}

	proxy := sf.SnowflakeProxy{
		PollInterval:       *pollInterval,
		Capacity:           uint(*capacity),
		STUNURL:            *stunURL,
		BrokerURL:          *rawBrokerURL,
		KeepLocalAddresses: *keepLocalAddresses,
		RelayURL:           *defaultRelayURL,
		NATProbeURL:        *probeURL,
		Interface:          *iface,
		OutboundAddress:    *outboundAddress,
		EphemeralMinPort:   ephemeralPortsRange[0],
		EphemeralMaxPort:   ephemeralPortsRange[1],

		NATTypeMeasurementInterval: *NATTypeMeasurementInterval,
		EventDispatcher:            eventLogger,

		RelayDomainNamePattern:          *allowedRelayHostNamePattern,
		AllowProxyingToPrivateAddresses: *allowProxyingToPrivateAddresses,
		AllowNonTLSRelay:                *allowNonTLSRelay,

		SummaryInterval: *summaryInterval,
		GeoIP:           gip,

		CovertDTLSConfig: cDTLSconfig,
	}

	var logOutput = io.Discard
	var eventlogOutput io.Writer = os.Stderr

	loggerFlags := log.LstdFlags

	if !*logLocalTime {
		loggerFlags |= log.LUTC
	}

	log.SetFlags(loggerFlags)

	if *verboseLogging {
		logOutput = os.Stderr
	}

	if *logFilename != "" {
		f, err := os.OpenFile(*logFilename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		if *verboseLogging {
			logOutput = io.MultiWriter(logOutput, f)
		}
		eventlogOutput = io.MultiWriter(eventlogOutput, f)
	}

	if *unsafeLogging {
		log.SetOutput(logOutput)
	} else {
		log.SetOutput(&safelog.LogScrubber{Output: logOutput})
	}

	proxyEventLogger := sf.NewProxyEventLogger(eventlogOutput, *disableStatsLogger)
	eventLogger.AddSnowflakeEventListener(proxyEventLogger)

	if *enableMetrics {
		metrics := sf.NewMetrics()

		err := metrics.Start(net.JoinHostPort(*metricsAddress, strconv.Itoa(*metricsPort)))
		if err != nil {
			log.Fatalf("could not enable metrics: %v", err)
		}

		eventLogger.AddSnowflakeEventListener(sf.NewEventMetrics(metrics))
	}

	log.Printf("snowflake-proxy %s\n", version.GetVersion())

	err = proxy.Start()
	if err != nil {
		log.Fatal(err)
	}
}
