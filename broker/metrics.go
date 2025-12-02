/*
We export metrics in the format specified in our broker spec:
https://gitweb.torproject.org/pluggable-transports/snowflake.git/tree/doc/broker-spec.txt
*/

package main

import (
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gitlab.torproject.org/tpo/anti-censorship/geoip"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/ptutil/safeprom"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/ipsetsink/sinkcluster"
	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/messages"
)

const (
	prometheusNamespace = "snowflake"
	metricsResolution   = 60 * 60 * 24 * time.Second //86400 seconds
)

type Metrics struct {
	logger  *log.Logger
	geoipdb *geoip.Geoip

	ips      *sync.Map // proxy IP addresses we've seen before
	counters *sync.Map // counters for ip-based metrics

	distinctIPWriter *sinkcluster.ClusterWriter

	// counters for country-based metrics
	proxies         *sync.Map // ip-based counts of proxy country codes
	clientHTTPPolls *sync.Map // poll-based counts of client HTTP rendezvous
	clientAMPPolls  *sync.Map // poll-based counts of client AMP cache rendezvous
	clientSQSPolls  *sync.Map // poll-based counts of client SQS rendezvous

	promMetrics *PromMetrics
}

func NewMetrics(metricsLogger *log.Logger) (*Metrics, error) {
	m := new(Metrics)

	m.logger = metricsLogger
	m.promMetrics = initPrometheus()
	m.ips = new(sync.Map)
	m.counters = new(sync.Map)
	m.proxies = new(sync.Map)
	m.clientHTTPPolls = new(sync.Map)
	m.clientAMPPolls = new(sync.Map)
	m.clientSQSPolls = new(sync.Map)

	// Write to log file every day with updated metrics
	go m.logMetrics()

	return m, nil
}

func incrementMapCounter(counters *sync.Map, key string) {
	start := uint64(1)
	val, loaded := counters.LoadOrStore(key, &start)
	if loaded {
		ptr := val.(*uint64)
		atomic.AddUint64(ptr, 1)
	}
}

func (m *Metrics) IncrementCounter(key string) {
	incrementMapCounter(m.counters, key)
}

func (m *Metrics) UpdateProxyStats(addr string, proxyType string, natType string) {

	// perform geolocation of IP address
	ip := net.ParseIP(addr)
	if m.geoipdb == nil {
		return
	}
	country, ok := m.geoipdb.GetCountryByAddr(ip)
	if !ok {
		country = "??"
	}

	// check whether we've seen this proxy ip before
	if _, loaded := m.ips.LoadOrStore(addr, true); !loaded {
		m.IncrementCounter("proxy-total")
		incrementMapCounter(m.proxies, country)
		m.promMetrics.ProxyTotal.With(prometheus.Labels{
			"nat":  natType,
			"type": proxyType,
			"cc":   country,
		}).Inc()
	}

	// update unique IP proxy NAT metrics
	key := fmt.Sprintf("%s-%s", addr, natType)
	if _, loaded := m.ips.LoadOrStore(key, true); !loaded {
		switch natType {
		case NATRestricted:
			m.IncrementCounter("proxy-nat-restricted")
		case NATUnrestricted:
			m.IncrementCounter("proxy-nat-unrestricted")
		default:
			m.IncrementCounter("proxy-nat-unknown")
		}
	}
	// update unique IP proxy type metrics
	key = fmt.Sprintf("%s-%s", addr, proxyType)
	if _, loaded := m.ips.LoadOrStore(key, true); !loaded {
		switch proxyType {
		case "standalone":
			m.IncrementCounter("proxy-standalone")
		case "badge":
			m.IncrementCounter("proxy-badge")
		case "iptproxy":
			m.IncrementCounter("proxy-iptproxy")
		case "webext":
			m.IncrementCounter("proxy-webext")
		}
	}
}

func (m *Metrics) UpdateClientStats(addr string, rendezvousMethod messages.RendezvousMethod, natType, status string) {
	ip := net.ParseIP(addr)
	country := "??"
	if m.geoipdb != nil {
		country_by_addr, ok := m.geoipdb.GetCountryByAddr(ip)
		if ok {
			country = country_by_addr
		}
	}

	switch status {
	case "denied":
		m.IncrementCounter("client-denied")
		if natType == NATUnrestricted {
			m.IncrementCounter("client-unrestricted-denied")
		} else {
			m.IncrementCounter("client-restricted-denied")
		}
	case "matched":
		m.IncrementCounter("client-match")
	case "timeout":
		m.IncrementCounter("client-timeout")
	default:
		log.Printf("Unknown rendezvous status: %s", status)
	}

	switch rendezvousMethod {
	case messages.RendezvousHttp:
		m.IncrementCounter("client-http")
		incrementMapCounter(m.clientHTTPPolls, country)
	case messages.RendezvousAmpCache:
		m.IncrementCounter("client-amp")
		incrementMapCounter(m.clientAMPPolls, country)
	case messages.RendezvousSqs:
		m.IncrementCounter("client-sqs")
		incrementMapCounter(m.clientSQSPolls, country)
	}
	m.promMetrics.ClientPollTotal.With(prometheus.Labels{
		"nat":               natType,
		"status":            status,
		"rendezvous_method": string(rendezvousMethod),
		"cc":                country,
	}).Inc()
}

// Types to facilitate sorting in formatAndClearCountryStats.
type record struct {
	cc    string
	count uint64
}
type records []record

// Implementation of sort.Interface for records. The ordering is lexicographic:
// first by count (descending), then by cc (ascending).
func (r records) Len() int      { return len(r) }
func (r records) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r records) Less(i, j int) bool {
	return r[i].count > r[j].count || (r[i].count == r[j].count && r[i].cc < r[j].cc)
}

// formatAndClearCountryStats takes a map from country codes to counts, and
// returns a formatted string of comma-separated CC=COUNT. Entries are sorted by
// count from largest to smallest. When counts are equal, entries are sorted by
// country code in ascending order.
//
// formatAndClearCountryStats has the side effect of deleting all entries in m.
func formatAndClearCountryStats(m *sync.Map, binned bool) string {
	// Extract entries from the map into a slice of records, binning counts
	// if asked to.
	rs := records{}
	m.Range(func(cc, countPtr any) bool {
		count := *countPtr.(*uint64)
		if binned {
			count = binCount(count)
		}
		rs = append(rs, record{cc: cc.(string), count: count})
		m.Delete(cc)
		return true
	})
	// Sort the records.
	sort.Sort(rs)
	// Format and concatenate.
	var output strings.Builder
	for i, r := range rs {
		if i != 0 {
			output.WriteString(",")
		}
		fmt.Fprintf(&output, "%s=%d", r.cc, r.count)
	}
	return output.String()
}

func (m *Metrics) LoadGeoipDatabases(geoipDB string, geoip6DB string) error {

	// Load geoip databases
	var err error
	log.Println("Loading geoip databases")
	m.geoipdb, err = geoip.New(geoipDB, geoip6DB)
	return err
}

// Logs metrics in intervals specified by metricsResolution
func (m *Metrics) logMetrics() {
	heartbeat := time.Tick(metricsResolution)
	for range heartbeat {
		m.printMetrics()
	}
}

func (m *Metrics) loadAndZero(key string) uint64 {
	count, loaded := m.counters.LoadAndDelete(key)
	if !loaded {
		count = new(uint64)
	}
	ptr := count.(*uint64)
	return *ptr
}

func (m *Metrics) printMetrics() {
	m.logger.Println(
		"snowflake-stats-end",
		time.Now().UTC().Format("2006-01-02 15:04:05"),
		fmt.Sprintf("(%d s)", int(metricsResolution.Seconds())),
	)
	m.logger.Println("snowflake-ips", formatAndClearCountryStats(m.proxies, false))
	m.logger.Printf("snowflake-ips-iptproxy %d\n", m.loadAndZero("proxy-iptproxy"))
	m.logger.Printf("snowflake-ips-standalone %d\n", m.loadAndZero("proxy-standalone"))
	m.logger.Printf("snowflake-ips-webext %d\n", m.loadAndZero("proxy-webext"))
	m.logger.Printf("snowflake-ips-badge %d\n", m.loadAndZero("proxy-badge"))
	m.logger.Println("snowflake-ips-total", m.loadAndZero("proxy-total"))
	m.logger.Println("snowflake-idle-count", binCount(m.loadAndZero("proxy-idle")))
	m.logger.Println("snowflake-proxy-poll-with-relay-url-count", binCount(m.loadAndZero("proxy-poll-with-relay-url")))
	m.logger.Println("snowflake-proxy-poll-without-relay-url-count", binCount(m.loadAndZero("proxy-poll-without-relay-url")))
	m.logger.Println("snowflake-proxy-rejected-for-relay-url-count", binCount(m.loadAndZero("proxy-poll-rejected-relay-url")))

	m.logger.Println("client-denied-count", binCount(m.loadAndZero("client-denied")))
	m.logger.Println("client-restricted-denied-count", binCount(m.loadAndZero("client-restricted-denied")))
	m.logger.Println("client-unrestricted-denied-count", binCount(m.loadAndZero("client-unrestricted-denied")))
	m.logger.Println("client-snowflake-match-count", binCount(m.loadAndZero("client-match")))
	m.logger.Println("client-snowflake-timeout-count", binCount(m.loadAndZero("client-timeout")))

	m.logger.Printf("client-http-count %d\n", binCount(m.loadAndZero("client-http")))
	m.logger.Printf("client-http-ips %s\n", formatAndClearCountryStats(m.clientHTTPPolls, true))
	m.logger.Printf("client-ampcache-count %d\n", binCount(m.loadAndZero("client-amp")))
	m.logger.Printf("client-ampcache-ips %s\n", formatAndClearCountryStats(m.clientAMPPolls, true))
	m.logger.Printf("client-sqs-count %d\n", binCount(m.loadAndZero("client-sqs")))
	m.logger.Printf("client-sqs-ips %s\n", formatAndClearCountryStats(m.clientSQSPolls, true))

	m.logger.Println("snowflake-ips-nat-restricted", m.loadAndZero("proxy-nat-restricted"))
	m.logger.Println("snowflake-ips-nat-unrestricted", m.loadAndZero("proxy-nat-unrestricted"))
	m.logger.Println("snowflake-ips-nat-unknown", m.loadAndZero("proxy-nat-unknown"))

	m.ips.Clear()
}

// binCount rounds count up to the next multiple of 8. Returns 0 on integer
// overflow.
func binCount(count uint64) uint64 {
	return (count + 7) / 8 * 8
}

type PromMetrics struct {
	registry         *prometheus.Registry
	ProxyTotal       *prometheus.CounterVec
	ProxyPollTotal   *safeprom.CounterVec
	ClientPollTotal  *safeprom.CounterVec
	ProxyAnswerTotal *safeprom.CounterVec
	AvailableProxies *prometheus.GaugeVec

	ProxyPollWithRelayURLExtensionTotal    *safeprom.CounterVec
	ProxyPollWithoutRelayURLExtensionTotal *safeprom.CounterVec

	ProxyPollRejectedForRelayURLExtensionTotal *safeprom.CounterVec
}

// Initialize metrics for prometheus exporter
func initPrometheus() *PromMetrics {
	promMetrics := &PromMetrics{}

	promMetrics.registry = prometheus.NewRegistry()

	promMetrics.ProxyTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Name:      "proxy_total",
			Help:      "The number of unique snowflake IPs",
		},
		[]string{"type", "nat", "cc"},
	)

	promMetrics.AvailableProxies = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "available_proxies",
			Help:      "The number of currently available snowflake proxies",
		},
		[]string{"type", "nat"},
	)

	promMetrics.ProxyPollTotal = safeprom.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Name:      "rounded_proxy_poll_total",
			Help:      "The number of snowflake proxy polls, rounded up to a multiple of 8",
		},
		[]string{"nat", "type", "status"},
	)

	promMetrics.ProxyAnswerTotal = safeprom.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Name:      "rounded_proxy_answer_total",
			Help:      "The number of snowflake proxy answers, rounded up to a multiple of 8",
		},
		[]string{"type", "status"},
	)

	promMetrics.ProxyPollWithRelayURLExtensionTotal = safeprom.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Name:      "rounded_proxy_poll_with_relay_url_extension_total",
			Help:      "The number of snowflake proxy polls with Relay URL Extension, rounded up to a multiple of 8",
		},
		[]string{"nat", "type"},
	)

	promMetrics.ProxyPollWithoutRelayURLExtensionTotal = safeprom.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Name:      "rounded_proxy_poll_without_relay_url_extension_total",
			Help:      "The number of snowflake proxy polls without Relay URL Extension, rounded up to a multiple of 8",
		},
		[]string{"nat", "type"},
	)

	promMetrics.ProxyPollRejectedForRelayURLExtensionTotal = safeprom.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Name:      "rounded_proxy_poll_rejected_relay_url_extension_total",
			Help:      "The number of snowflake proxy polls rejected by Relay URL Extension, rounded up to a multiple of 8",
		},
		[]string{"nat", "type"},
	)

	promMetrics.ClientPollTotal = safeprom.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Name:      "rounded_client_poll_total",
			Help:      "The number of snowflake client polls, rounded up to a multiple of 8",
		},
		[]string{"nat", "status", "cc", "rendezvous_method"},
	)

	// We need to register our metrics so they can be exported.
	promMetrics.registry.MustRegister(
		promMetrics.ClientPollTotal, promMetrics.ProxyPollTotal,
		promMetrics.ProxyTotal, promMetrics.ProxyAnswerTotal, promMetrics.AvailableProxies,
		promMetrics.ProxyPollWithRelayURLExtensionTotal,
		promMetrics.ProxyPollWithoutRelayURLExtensionTotal,
		promMetrics.ProxyPollRejectedForRelayURLExtensionTotal,
	)

	return promMetrics
}

func (m *Metrics) RecordIPAddress(ip string, restricted bool, proxyType string) {
	if m.distinctIPWriter != nil {
		if restricted {
			m.distinctIPWriter.AddIPToSet("restricted", ip)
		} else {
			m.distinctIPWriter.AddIPToSet("unrestricted", ip)
		}
		switch proxyType {
		case "standalone":
			m.distinctIPWriter.AddIPToSet("standalone", ip)
		case "badge":
			fallthrough
		case "webext":
			m.distinctIPWriter.AddIPToSet("browser", ip)
		case "iptproxy":
			m.distinctIPWriter.AddIPToSet("mobile", ip)
		default:
			m.distinctIPWriter.AddIPToSet("unknown", ip)
		}
	}
}
