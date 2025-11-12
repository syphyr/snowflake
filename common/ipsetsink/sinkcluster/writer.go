package sinkcluster

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"sync"
	"time"

	"gitlab.torproject.org/tpo/anti-censorship/pluggable-transports/snowflake/v2/common/ipsetsink"
)

func NewClusterWriter(writers map[string]WriteSyncer, key [32]byte, writeInterval time.Duration) *ClusterWriter {
	sinks := make(map[string]*sink)
	for name, writer := range writers {
		ipsetSink := &sink{
			writer:  writer,
			current: ipsetsink.NewIPSetSink(key[:]),
		}
		sinks[name] = ipsetSink
	}
	c := &ClusterWriter{
		sinks:         sinks,
		lastWriteTime: time.Now(),
		writeInterval: writeInterval,
	}
	return c
}

type sink struct {
	writer  WriteSyncer
	current *ipsetsink.IPSetSink
}

type ClusterWriter struct {
	sinks         map[string]*sink
	lastWriteTime time.Time
	writeInterval time.Duration
	lock          sync.Mutex
}

type WriteSyncer interface {
	Sync() error
	io.Writer
}

func (c *ClusterWriter) WriteIPSetToDisk() {
	currentTime := time.Now()
	for _, sink := range c.sinks {
		data, err := sink.current.Dump()
		if err != nil {
			log.Println("unable able to write ipset to file:", err)
			return
		}
		entry := &SinkEntry{
			RecordingStart: c.lastWriteTime,
			RecordingEnd:   currentTime,
			Recorded:       data,
		}
		jsonData, err := json.Marshal(entry)
		if err != nil {
			log.Println("unable able to write ipset to file:", err)
			return
		}
		jsonData = append(jsonData, byte('\n'))
		_, err = io.Copy(sink.writer, bytes.NewReader(jsonData))
		if err != nil {
			log.Println("unable able to write ipset to file:", err)
			return
		}
		sink.writer.Sync()
		sink.current.Reset()
	}
	c.lastWriteTime = currentTime
}

func (c *ClusterWriter) AddIPToSet(name, ipAddress string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.lastWriteTime.Add(c.writeInterval).Before(time.Now()) {
		c.WriteIPSetToDisk()
	}
	c.sinks[name].current.AddIPToSet(ipAddress)
}
