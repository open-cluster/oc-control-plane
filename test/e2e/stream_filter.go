package e2e

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"google.golang.org/protobuf/proto"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

type acknowledgementProbe struct {
	mu          sync.Mutex
	armed       bool
	target      string
	dropped     chan string
	redelivered chan relayv1.ResultAck_Disposition
}

func (p *acknowledgementProbe) arm() (<-chan string, <-chan relayv1.ResultAck_Disposition) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = true
	p.target = ""
	p.dropped = make(chan string, 1)
	p.redelivered = make(chan relayv1.ResultAck_Disposition, 1)
	return p.dropped, p.redelivered
}

func (p *acknowledgementProbe) drop(acknowledgement *relayv1.ResultAck) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.armed {
		return false
	}
	if p.target == "" && acknowledgement.GetDisposition() == relayv1.ResultAck_DISPOSITION_RECORDED {
		p.target = acknowledgement.GetJobId()
		p.dropped <- p.target
		return true
	}
	if acknowledgement.GetJobId() == p.target {
		p.redelivered <- acknowledgement.GetDisposition()
		p.armed = false
	}
	return false
}

type acknowledgementBody struct {
	upstream io.ReadCloser
	probe    *acknowledgementProbe
	pending  []byte
}

func (b *acknowledgementBody) Read(destination []byte) (int, error) {
	for len(b.pending) == 0 {
		header := make([]byte, 5)
		if _, err := io.ReadFull(b.upstream, header); err != nil {
			return 0, err
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length > 16<<20 {
			return 0, errors.New("relay response exceeded the bounded gRPC frame size")
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(b.upstream, payload); err != nil {
			return 0, err
		}
		message := &relayv1.ControlToRelay{}
		if header[0] == 0 && proto.Unmarshal(payload, message) == nil &&
			message.GetResultAck() != nil && b.probe.drop(message.GetResultAck()) {
			continue
		}
		b.pending = bytes.Join([][]byte{header, payload}, nil)
	}
	copied := copy(destination, b.pending)
	b.pending = b.pending[copied:]
	return copied, nil
}

func (b *acknowledgementBody) Close() error { return b.upstream.Close() }
