// SPDX-License-Identifier: MPL-2.0

package media

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// dtlsTestPacket is an RTP packet whose payload is easy to find on the wire.
func dtlsTestPacket(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    CodecAudioUlaw.PayloadType,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 160,
			SSRC:           0x5eed0000,
		},
		Payload: []byte{0xd5, 0x5a, 0xd5, 0x5a, 0xd5, 0x5a, 0xd5, 0x5a},
	}
}

// requireSRTPBetween writes one packet from one session and requires the other
// to read and decrypt it.
func requireSRTPBetween(t *testing.T, from, to *MediaSession, seq uint16) {
	t.Helper()
	pkt := dtlsTestPacket(seq)
	require.NoError(t, from.WriteRTP(pkt))
	require.NoError(t, to.StopRTP(1, 5*time.Second))
	got := rtp.Packet{}
	_, err := to.ReadRTP(make([]byte, RTPBufSize), &got)
	require.NoError(t, err, "the media did not arrive and decrypt")
	require.NoError(t, to.StartRTP(1))
	require.Equal(t, seq, got.SequenceNumber)
	require.Equal(t, pkt.Payload, got.Payload)
}

// TestDTLSReadDiscardsPlaintext pins that a DTLS-SRTP session never hands its
// reader media that is not protected with the keys of its handshake. Its media
// is SRTP, which is not processed before the handshake completes (RFC 5764
// section 5.1), so plaintext RTP or RTCP on its sockets is not the peer's:
// anyone can send a datagram to a port named in the SDP. Before the handshake has keyed the session there is
// nothing the media could be checked against, and afterwards what fails
// authentication is discarded. Either way ReadRTP and ReadRTCP go on reading,
// and deliver the peer's protected media once it comes.
func TestDTLSReadDiscardsPlaintext(t *testing.T) {
	plainRTP, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: CodecAudioUlaw.PayloadType, SequenceNumber: 7, Timestamp: 1120, SSRC: 0xbad0bad0},
		Payload: []byte{1, 2, 3, 4, 5, 6, 7, 8},
	}).Marshal()
	require.NoError(t, err)
	plainRTCP, err := rtcp.Marshal([]rtcp.Packet{&rtcp.ReceiverReport{SSRC: 0xbad0bad0}})
	require.NoError(t, err)

	for _, keyed := range []bool{false, true} {
		t.Run(fmt.Sprintf("keyed=%t", keyed), func(t *testing.T) {
			offerer := newDTLSRoleSession(t, testdata.ClientCertificate())
			answerer := newDTLSRoleSession(t, testdata.ServerCertificate())
			require.NoError(t, answerer.RemoteSDP(offerer.LocalSDP(false), false))
			require.NoError(t, offerer.RemoteSDP(answerer.LocalSDP(true), true))
			if keyed {
				requireDTLSHandshake(t, offerer, answerer)
			}

			attacker, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			require.NoError(t, err)
			t.Cleanup(func() { _ = attacker.Close() })
			rtpAddr := answerer.rtpConn.LocalAddr().(*net.UDPAddr)
			rtcpAddr := answerer.rtcpConn.LocalAddr().(*net.UDPAddr)
			_, err = attacker.WriteTo(plainRTP, rtpAddr)
			require.NoError(t, err)
			_, err = attacker.WriteTo(plainRTCP, rtcpAddr)
			require.NoError(t, err)

			require.NoError(t, answerer.rtpConn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
			got := rtp.Packet{}
			n, err := answerer.ReadRTP(make([]byte, RTPBufSize), &got)
			require.True(t, os.IsTimeout(err), "ReadRTP must discard the plaintext packet and read on until its deadline: n=%d err=%v ssrc=%x", n, err, got.SSRC)
			require.NoError(t, answerer.rtpConn.SetReadDeadline(time.Time{}))

			require.NoError(t, answerer.rtcpConn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
			pkts := make([]rtcp.Packet, 5)
			n, err = answerer.ReadRTCP(make([]byte, RTPBufSize), pkts)
			require.True(t, os.IsTimeout(err), "ReadRTCP must discard the plaintext packet and read on until its deadline: n=%d err=%v", n, err)
			require.NoError(t, answerer.rtcpConn.SetReadDeadline(time.Time{}))

			if !keyed {
				requireDTLSHandshake(t, offerer, answerer)
			}
			// The discards left the session reading: the peer's media arrives.
			requireSRTPBetween(t, offerer, answerer, 30)
			require.NoError(t, offerer.WriteRTCP(&rtcp.ReceiverReport{SSRC: 0x5eed0000}))
			require.NoError(t, answerer.rtcpConn.SetReadDeadline(time.Now().Add(5*time.Second)))
			n, err = answerer.ReadRTCP(make([]byte, RTPBufSize), pkts)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			require.Equal(t, uint32(0x5eed0000), pkts[0].(*rtcp.ReceiverReport).SSRC)
		})
	}
}
