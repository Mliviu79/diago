// SPDX-License-Identifier: MPL-2.0

package media

import (
	"os"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/pion/rtcp"
	"github.com/stretchr/testify/require"
)

// TestDTLSWriteRTCPsEncrypts pins that the compound RTCP WriteRTCPs sends on a
// keyed DTLS-SRTP session goes out as SRTCP, as WriteRTCP's does: RTP and RTCP
// go over a DTLS-SRTP session protected with SRTP (RFC 5764 section 5.1). The
// datagram is read off the peer's socket and must differ from the plaintext
// and decrypt with the peer's keys. While a handshake FinalizeWithin left running has not keyed
// the session, WriteRTCPs is refused like WriteRTCP.
func TestDTLSWriteRTCPsEncrypts(t *testing.T) {
	pkts := []rtcp.Packet{
		&rtcp.SenderReport{SSRC: 0x5eed0000, NTPTime: 1 << 40, RTPTime: 160, PacketCount: 1, OctetCount: 160},
		&rtcp.ReceiverReport{SSRC: 0x5eed0000},
	}
	plain, err := rtcp.Marshal(pkts)
	require.NoError(t, err)

	t.Run("keyed", func(t *testing.T) {
		offerer, answerer, _, _ := newEstablishedDTLSPair(t, nil)
		require.NoError(t, offerer.WriteRTCPs(pkts))

		buf := make([]byte, RTPBufSize)
		require.NoError(t, answerer.rtcpConn.SetReadDeadline(time.Now().Add(5*time.Second)))
		n, _, err := answerer.rtcpConn.ReadFrom(buf)
		require.NoError(t, err)
		wire := buf[:n]
		require.NotEqual(t, plain, wire[:min(len(wire), len(plain))], "the RTCP went out in plaintext")

		decrypted, err := answerer.remoteCtxSRTP.DecryptRTCP(nil, wire, nil)
		require.NoError(t, err, "the RTCP on the wire is not SRTCP under the association's keys")
		require.Equal(t, plain, decrypted)
	})

	t.Run("handshake left running", func(t *testing.T) {
		offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
		answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
		answerer.DTLSConf.SDPSetupRole = func(bool) string { return "passive" }
		negotiateDTLS(t, offerer, answerer, nil)

		require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 100*time.Millisecond), ErrFinalizeInProgress)
		require.ErrorIs(t, answerer.WriteRTCPs(pkts), ErrDTLSNotKeyed)

		require.NoError(t, offerer.rtcpConn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
		_, _, err := offerer.rtcpConn.ReadFrom(make([]byte, RTPBufSize))
		require.True(t, os.IsTimeout(err), "RTCP went out before the handshake: %v", err)
	})
}
