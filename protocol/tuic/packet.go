package tuic

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mzz2017/quic-go"
	"github.com/mzz2017/softwind/netproxy"
	"github.com/mzz2017/softwind/pkg/bufferred_conn"
	"github.com/mzz2017/softwind/pkg/fastrand"
	"github.com/mzz2017/softwind/pool"
	"github.com/mzz2017/softwind/protocol"
	"github.com/mzz2017/softwind/protocol/tuic/common"
)

type quicStreamPacketConn struct {
	target string

	connId    uint16
	quicConn  quic.Connection
	inputConn *bufferred_conn.BufferedConn

	udpRelayMode          common.UdpRelayMode
	maxUdpRelayPacketSize int

	deferQuicConnFn func(quicConn quic.Connection, err error)
	closeDeferFn    func()
	writeClosed     *atomic.Bool

	closeOnce sync.Once
	closeErr  error
	closed    bool

	deFragger
}

func (q *quicStreamPacketConn) Close() error {
	q.closeOnce.Do(func() {
		q.closed = true
		q.closeErr = q.close()
	})
	return q.closeErr
}

func (q *quicStreamPacketConn) close() (err error) {
	if q.closeDeferFn != nil {
		defer q.closeDeferFn()
	}
	if q.deferQuicConnFn != nil {
		defer func() {
			q.deferQuicConnFn(q.quicConn, err)
		}()
	}
	if q.inputConn != nil {
		_ = q.inputConn.Close()
		q.inputConn = nil

		buf := pool.GetBuffer()
		defer pool.PutBuffer(buf)
		err = NewDissociate(q.connId).WriteTo(buf)
		if err != nil {
			return
		}
		var stream quic.SendStream
		stream, err = q.quicConn.OpenUniStream()
		if err != nil {
			return
		}
		_, err = buf.WriteTo(stream)
		if err != nil {
			return
		}
		err = stream.Close()
		if err != nil {
			return
		}
	}
	return
}

func (q *quicStreamPacketConn) SetDeadline(t time.Time) error {
	//TODO implement me
	return nil
}

func (q *quicStreamPacketConn) SetReadDeadline(t time.Time) error {
	if q.inputConn != nil {
		return q.inputConn.SetReadDeadline(t)
	}
	return nil
}

func (q *quicStreamPacketConn) SetWriteDeadline(t time.Time) error {
	//TODO implement me
	return nil
}

func (q *quicStreamPacketConn) ReadFrom(p []byte) (n int, addr netip.AddrPort, err error) {
	if q.inputConn != nil {
		for {
			var packet Packet
			packet, err = ReadPacket(q.inputConn)
			if err != nil {
				return
			}
			if packetPtr := q.deFragger.Feed(packet); packetPtr != nil {
				n = copy(p, packet.DATA)
				addr = packetPtr.ADDR.UDPAddr().AddrPort()
				return
			}
		}
	} else {
		err = net.ErrClosed
	}
	return
}

func (q *quicStreamPacketConn) WaitReadFrom() (data []byte, put func(), addr net.Addr, err error) {
	if q.inputConn != nil {
		for {
			var packet Packet
			packet, err = ReadPacket(q.inputConn)
			if err != nil {
				return
			}
			if packetPtr := q.deFragger.Feed(packet); packetPtr != nil {
				data = packetPtr.DATA
				addr = packetPtr.ADDR.UDPAddr()
				return
			}
		}
	} else {
		err = net.ErrClosed
	}
	return
}

func (q *quicStreamPacketConn) WriteTo(p []byte, addr string) (n int, err error) {
	if len(p) > 0xffff { // uint16 max
		return 0, quic.ErrMessageTooLarge(0xffff)
	}
	if q.closed {
		return 0, net.ErrClosed
	}
	if q.writeClosed != nil && q.writeClosed.Load() {
		_ = q.Close()
		return 0, net.ErrClosed
	}
	if q.deferQuicConnFn != nil {
		defer func() {
			q.deferQuicConnFn(q.quicConn, err)
		}()
	}
	buf := pool.GetBuffer()
	defer pool.PutBuffer(buf)
	mdata, err := protocol.ParseMetadata(addr)
	if err != nil {
		return 0, err
	}
	address := NewAddress(&mdata)
	pktId := uint16(fastrand.Uint32())
	packet := NewPacket(q.connId, pktId, 1, 0, uint16(len(p)), address, p)
	switch q.udpRelayMode {
	case common.QUIC:
		err = packet.WriteTo(buf)
		if err != nil {
			return
		}
		var stream quic.SendStream
		stream, err = q.quicConn.OpenUniStream()
		if err != nil {
			return
		}
		defer stream.Close()
		_, err = buf.WriteTo(stream)
		if err != nil {
			return
		}
	default: // native
		if len(p) > q.maxUdpRelayPacketSize {
			err = fragWriteNative(q.quicConn, packet, buf, q.maxUdpRelayPacketSize)
			if err != nil {
				return
			}
		}
		err = packet.WriteTo(buf)
		if err != nil {
			return
		}
		data := buf.Bytes()
		err = q.quicConn.SendMessage(data)

		var tooLarge quic.ErrMessageTooLarge
		if errors.As(err, &tooLarge) {
			err = fragWriteNative(q.quicConn, packet, buf, int(tooLarge)-PacketOverHead)
		}
		if err != nil {
			return
		}
	}
	n = len(p)

	return
}

func (q *quicStreamPacketConn) LocalAddr() net.Addr {
	return q.quicConn.LocalAddr()
}

func (conn *quicStreamPacketConn) Read(b []byte) (n int, err error) {
	n, _, err = conn.ReadFrom(b)
	return n, err
}

func (conn *quicStreamPacketConn) Write(b []byte) (n int, err error) {
	return conn.WriteTo(b, conn.target)
}

var _ netproxy.PacketConn = (*quicStreamPacketConn)(nil)
