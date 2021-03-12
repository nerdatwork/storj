// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package quic

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/lucas-clemente/quic-go"

	"storj.io/common/memory"
	"storj.io/common/peertls/tlsopts"
	"storj.io/common/rpc"
)

// Connector implements a dialer that creates a quic connection.
type Connector struct {
	transferRate memory.Size

	config *quic.Config
}

// NewDefaultConnector instantiates a new instance of Connector.
// If no quic configuration is provided, default value will be used.
func NewDefaultConnector(quicConfig *quic.Config) Connector {
	if quicConfig == nil {
		quicConfig = &quic.Config{
			MaxIdleTimeout: 15 * time.Minute,
		}
	}
	return Connector{
		config: quicConfig,
	}
}

// DialContext creates a quic connection.
func (c Connector) DialContext(ctx context.Context, tlsConfig *tls.Config, address string) (_ rpc.ConnectorConn, err error) {
	defer mon.Task()(&ctx)(&err)

	if tlsConfig == nil {
		return nil, Error.New("tls config is not set")
	}
	tlsConfigCopy := tlsConfig.Clone()
	tlsConfigCopy.NextProtos = []string{tlsopts.StorjApplicationProtocol}

	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	rawConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}

	sess, err := quic.DialContext(ctx, rawConn, udpAddr, udpAddr.IP.String(), tlsConfigCopy, c.config)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	if !HasSufficientUDPReceiveBufferSize(rawConn) {
		return nil, Error.New("failed to increase udp receive buffer size")
	}

	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	conn := &Conn{
		session: sess,
		stream:  stream,
	}

	return &timedConn{
		ConnectorConn: TrackClose(conn),
		rate:          c.transferRate,
	}, nil
}

// SetTransferRate returns a QUIC connector with the given transfer rate.
func (c Connector) SetTransferRate(rate memory.Size) Connector {
	c.transferRate = rate
	return c
}

// TransferRate returns the transfer rate set on the connector.
func (c Connector) TransferRate() memory.Size {
	return c.transferRate
}
