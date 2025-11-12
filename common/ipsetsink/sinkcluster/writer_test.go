package sinkcluster

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

type writerStub struct {
	io.Writer
}

func (w writerStub) Sync() error {
	return nil
}

func TestSinkWriter(t *testing.T) {

	Convey("Context", t, func() {
		buffer := bytes.NewBuffer(nil)
		writerStubInst := &writerStub{buffer}
		var key [32]byte
		if n, err := rand.Read(key[:]); (n < 32) || (err != nil) {
			panic(err)
		}
		clusterWriter := NewClusterWriter(map[string]WriteSyncer{
			"demo": writerStubInst,
		}, key, time.Minute)
		clusterWriter.AddIPToSet("demo", "1")
		clusterWriter.WriteIPSetToDisk()
		So(buffer.Bytes(), ShouldNotBeNil)
	})
}
