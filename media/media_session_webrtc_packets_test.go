// SPDX-License-Identifier: MPL-2.0

package media

import (
	"bytes"
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webrtcSRTPPeer is the far end of a MediaSessionWebrtc assembled over a UDP
// pair: the socket the session's mux reads from and writes to, and the SRTP
// contexts that protect what the session accepts and open what it sends.
type webrtcSRTPPeer struct {
	conn net.Conn

	// sendSRTP and sendSRTCP encrypt what the session's remote contexts
	// decrypt, recvSRTP decrypts what its local context encrypts.
	sendSRTP  *srtp.Context
	sendSRTCP *srtp.Context
	recvSRTP  *srtp.Context

	// keys are the contexts the session takes when keyWebrtcSession keys it.
	keys webrtcPacketPath
}

// newWebrtcSRTPPeers returns a WebRTC session whose packet mux runs over one
// end of a UDP pair, and the peer at the other end. The session has a mux, as
// after RemoteSDP, and no keys: keyWebrtcSession keys it as Finalize does.
//
// The keys are built directly and no agent or handshake runs, as in
// srtpPeers: what a packet path does with a datagram needs a mux and SRTP
// contexts and nothing else, and a fixed key makes a failure reproducible.
func newWebrtcSRTPPeers(t *testing.T) (*MediaSessionWebrtc, *webrtcSRTPPeer) {
	t.Helper()

	local, remote := udpConnPair(t)
	sess := newMuxedWebrtcSession(t, local)
	sess.Codecs = []Codec{CodecAudioUlaw}
	sess.Mode = sdp.ModeSendrecv

	newContext := func(keyByte byte) *srtp.Context {
		ctx, err := srtp.CreateContext(bytes.Repeat([]byte{keyByte}, 16), bytes.Repeat([]byte{keyByte + 1}, 14), srtp.ProtectionProfileAes128CmHmacSha1_80)
		require.NoError(t, err)
		return ctx
	}
	// One key per direction, and a context of its own for each use of a key,
	// as Finalize builds them.
	const peerKey, sessionKey = 0x2a, 0x4c
	peer := &webrtcSRTPPeer{
		conn:      remote,
		sendSRTP:  newContext(peerKey),
		sendSRTCP: newContext(peerKey),
		recvSRTP:  newContext(sessionKey),
		keys: webrtcPacketPath{
			localSRTP:   newContext(sessionKey),
			remoteSRTP:  newContext(peerKey),
			localSRTCP:  newContext(sessionKey),
			remoteSRTCP: newContext(peerKey),
		},
	}
	return sess, peer
}

// keyWebrtcSession gives the session the SRTP contexts of peer and marks it
// negotiated, under the session's lock, as Finalize does once the handshake
// has exported the keys.
func keyWebrtcSession(sess *MediaSessionWebrtc, peer *webrtcSRTPPeer) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.localCtxSRTP = peer.keys.localSRTP
	sess.remoteCtxSRTP = peer.keys.remoteSRTP
	sess.localCtxSRTCP = peer.keys.localSRTCP
	sess.remoteCtxSRTCP = peer.keys.remoteSRTCP
	sess.ready = true
}

// send writes one datagram from the peer to the session.
func (p *webrtcSRTPPeer) send(t *testing.T, datagram []byte) {
	t.Helper()
	n, err := p.conn.Write(datagram)
	require.NoError(t, err)
	require.Equal(t, len(datagram), n)
}

// sendRTCP writes pkt from the peer to the session, protected as SRTCP.
func (p *webrtcSRTPPeer) sendRTCP(t *testing.T, pkt rtcp.Packet) {
	t.Helper()
	raw, err := pkt.Marshal()
	require.NoError(t, err)
	enc, err := p.sendSRTCP.EncryptRTCP(nil, raw, nil)
	require.NoError(t, err)
	p.send(t, enc)
}

// webrtcRTPPacket is an RTP packet of the peer's stream.
func webrtcRTPPacket(seq uint16, payload string) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    CodecAudioUlaw.PayloadType,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 160,
			SSRC:           0x11223344,
		},
		Payload: []byte(payload),
	}
}

// plainRTCP is an RTCP receiver report with no SRTCP protection, as anyone can
// send it to a port named in an SDP.
func plainRTCP(t *testing.T, ssrc uint32) []byte {
	t.Helper()
	raw, err := (&rtcp.ReceiverReport{SSRC: ssrc}).Marshal()
	require.NoError(t, err)
	return raw
}

// requireReceiverReport requires pkt to be the receiver report of ssrc.
func requireReceiverReport(t *testing.T, pkt rtcp.Packet, ssrc uint32) {
	t.Helper()
	report, ok := pkt.(*rtcp.ReceiverReport)
	require.True(t, ok, "read %T, want a receiver report", pkt)
	require.Equal(t, ssrc, report.SSRC)
}

// TestWebrtcReadRTPDiscardsPacketFailingSRTPAuth is
// TestReadRTPDiscardsPacketFailingSRTPAuth on the WebRTC session: the one
// unprotected packet a peer sends before its own SRTP context is armed must
// not reach the caller at all. ReadRTP skips it and returns the next packet
// that authenticates (RFC 3711 section 3.3). Returned as a read error, it
// stops the reader above for the rest of the call.
func TestWebrtcReadRTPDiscardsPacketFailingSRTPAuth(t *testing.T) {
	sess, peer := newWebrtcSRTPPeers(t)
	keyWebrtcSession(sess, peer)

	want := webrtcRTPPacket(4243, "the call must survive the stray packet")
	peer.send(t, asteriskPlainRTP(t))
	peer.send(t, encryptFor(t, peer.sendSRTP, want))

	require.NoError(t, sess.StopRTP(1, 5*time.Second))
	var got rtp.Packet
	n, err := sess.ReadRTP(make([]byte, RTPBufSize), &got)

	require.NoError(t, err, "a packet failing SRTP auth must be discarded, not surfaced as a read error")
	assert.Equal(t, want.Payload, got.Payload, "ReadRTP must return the next packet that authenticates")
	assert.Positive(t, n, "the discarded packet must not be reported as the read length")
	assert.EqualValues(t, 1, sess.srtpAuthFailures.Load(), "the discard must be counted exactly once")
}

// TestWebrtcReadRTPDiscardsEveryFailingPacket pins that the discard is a loop
// and not an allowance of one packet: several unprotected packets, or a host
// spraying the port, do not stall the reader either.
func TestWebrtcReadRTPDiscardsEveryFailingPacket(t *testing.T) {
	sess, peer := newWebrtcSRTPPeers(t)
	keyWebrtcSession(sess, peer)

	want := webrtcRTPPacket(9000, "still here")
	for range 3 {
		peer.send(t, asteriskPlainRTP(t))
	}
	peer.send(t, encryptFor(t, peer.sendSRTP, want))

	require.NoError(t, sess.StopRTP(1, 5*time.Second))
	var got rtp.Packet
	_, err := sess.ReadRTP(make([]byte, RTPBufSize), &got)

	require.NoError(t, err, "the discard must loop: any single failing packet stopping the read is the bug")
	assert.Equal(t, want.Payload, got.Payload)
	assert.EqualValues(t, 3, sess.srtpAuthFailures.Load(), "every discarded packet must be counted")
}

// TestWebrtcReadRTPPropagatesTransportError pins the other half of the
// distinction, so that discarding cannot become a loop that spins on a dead
// transport or holds a reader that was told to stop: a closed session and a
// passed deadline end the read, keyed or not.
func TestWebrtcReadRTPPropagatesTransportError(t *testing.T) {
	for _, keyed := range []bool{true, false} {
		name := "keyed"
		if !keyed {
			name = "awaiting_keys"
		}
		t.Run(name+"/closed", func(t *testing.T) {
			sess, peer := newWebrtcSRTPPeers(t)
			if keyed {
				keyWebrtcSession(sess, peer)
			}

			readErr := make(chan error, 1)
			go func() {
				var got rtp.Packet
				_, err := sess.ReadRTP(make([]byte, RTPBufSize), &got)
				readErr <- err
			}()
			require.NoError(t, sess.Close())

			select {
			case err := <-readErr:
				require.Error(t, err, "a closed session has nothing left to read")
			case <-time.After(5 * time.Second):
				t.Fatal("ReadRTP did not return when its session was closed")
			}
			assert.Zero(t, sess.srtpAuthFailures.Load(), "a transport error is not an authentication failure")
		})
		t.Run(name+"/deadline", func(t *testing.T) {
			sess, peer := newWebrtcSRTPPeers(t)
			if keyed {
				keyWebrtcSession(sess, peer)
			}

			require.NoError(t, sess.StopRTP(1, time.Nanosecond))
			var got rtp.Packet
			_, err := sess.ReadRTP(make([]byte, RTPBufSize), &got)
			require.True(t, os.IsTimeout(err), "ReadRTP must return the passed deadline: %v", err)

			pkts := make([]rtcp.Packet, 5)
			require.NoError(t, sess.mux.rtcp.SetReadDeadline(time.Now()))
			_, err = sess.ReadRTCP(make([]byte, RTPBufSize), pkts)
			require.True(t, os.IsTimeout(err), "ReadRTCP must return the passed deadline: %v", err)
		})
	}
}

// TestWebrtcReadDiscardsPlaintext is TestDTLSReadDiscardsPlaintext on the
// WebRTC session, which never hands its reader media that is not protected
// with the keys of its handshake. Its media is SRTP, which is not processed
// before the handshake completes (RFC 5764 section 5.1): before the session is
// keyed there is nothing a packet could be checked against, and afterwards
// what fails authentication is discarded. Either way ReadRTP and ReadRTCP go
// on reading, and deliver the peer's protected media once it comes.
func TestWebrtcReadDiscardsPlaintext(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		name := "keyed"
		if !keyed {
			name = "awaiting_keys"
		}
		t.Run(name, func(t *testing.T) {
			sess, peer := newWebrtcSRTPPeers(t)
			if keyed {
				keyWebrtcSession(sess, peer)
			}

			peer.send(t, asteriskPlainRTP(t))
			peer.send(t, plainRTCP(t, 0xbad0bad0))

			require.NoError(t, sess.StopRTP(1, 300*time.Millisecond))
			got := rtp.Packet{}
			n, err := sess.ReadRTP(make([]byte, RTPBufSize), &got)
			require.True(t, os.IsTimeout(err), "ReadRTP must discard the plaintext packet and read on until its deadline: n=%d err=%v ssrc=%x", n, err, got.SSRC)
			require.NoError(t, sess.StartRTP(1))

			require.NoError(t, sess.mux.rtcp.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
			pkts := make([]rtcp.Packet, 5)
			n, err = sess.ReadRTCP(make([]byte, RTPBufSize), pkts)
			require.True(t, os.IsTimeout(err), "ReadRTCP must discard the plaintext packet and read on until its deadline: n=%d err=%v", n, err)

			if !keyed {
				require.Zero(t, sess.srtpAuthFailures.Load(), "a session without keys has nothing to authenticate against")
				keyWebrtcSession(sess, peer)
			}

			// The discards left the session reading: the peer's media arrives.
			want := webrtcRTPPacket(30, "protected")
			peer.send(t, encryptFor(t, peer.sendSRTP, want))
			require.NoError(t, sess.StopRTP(1, 5*time.Second))
			_, err = sess.ReadRTP(make([]byte, RTPBufSize), &got)
			require.NoError(t, err)
			require.Equal(t, want.Payload, got.Payload)

			peer.sendRTCP(t, &rtcp.ReceiverReport{SSRC: 0x5eed0000})
			require.NoError(t, sess.mux.rtcp.SetReadDeadline(time.Now().Add(5*time.Second)))
			n, err = sess.ReadRTCP(make([]byte, RTPBufSize), pkts)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			requireReceiverReport(t, pkts[0], 0x5eed0000)
		})
	}
}

// TestWebrtcReadBeforeFinalizeWaitsForKeys pins what a read does on a session
// that has its remote description and no keys yet, as when a leg is bridged
// ahead of the negotiation that keys it: it neither fails nor delivers
// anything, and returns the peer's first protected packet once Finalize has
// keyed the session. The reader runs while Finalize keys the session, so the
// race detector also holds the two apart.
func TestWebrtcReadBeforeFinalizeWaitsForKeys(t *testing.T) {
	offerer, answerer := newNegotiatedWebrtcPair(t, "")

	// The bound of the read, should the media never come.
	require.NoError(t, answerer.StopRTP(1, 40*time.Second))
	type read struct {
		pkt rtp.Packet
		err error
	}
	reading := make(chan struct{})
	done := make(chan read, 1)
	go func() {
		var r read
		close(reading)
		_, r.err = answerer.ReadRTP(make([]byte, RTPBufSize), &r.pkt)
		done <- r
	}()
	<-reading

	requireBothFinalize(t, offerer, answerer, 40*time.Second, "the negotiation did not complete under a waiting reader")
	want := dtlsTestPacket(7)
	require.NoError(t, offerer.WriteRTP(want))

	select {
	case r := <-done:
		require.NoError(t, r.err, "a read that began before the keys must wait for them")
		require.Equal(t, want.Payload, r.pkt.Payload)
		require.Equal(t, want.SequenceNumber, r.pkt.SequenceNumber)
	case <-time.After(45 * time.Second):
		t.Fatal("the read that began before Finalize did not return")
	}
}

// TestWebrtcWriteBeforeKeyedIsRefused pins that a session no handshake has
// keyed sends no media and says so with ErrDTLSNotKeyed: its media is
// protected with SRTP and nothing else, and SRTP does not start before the
// handshake completes (RFC 5764 section 5.1). Once keyed, the same session
// sends protected media.
func TestWebrtcWriteBeforeKeyedIsRefused(t *testing.T) {
	t.Run("no_remote_description", func(t *testing.T) {
		sess := &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}, Mode: sdp.ModeSendrecv}
		require.ErrorIs(t, sess.WriteRTP(webrtcRTPPacket(1, "early")), ErrDTLSNotKeyed)
		require.ErrorIs(t, sess.WriteRTCP(&rtcp.ReceiverReport{SSRC: 1}), ErrDTLSNotKeyed)
	})

	t.Run("awaiting_keys", func(t *testing.T) {
		sess, peer := newWebrtcSRTPPeers(t)

		require.ErrorIs(t, sess.WriteRTP(webrtcRTPPacket(1, "early")), ErrDTLSNotKeyed)
		require.ErrorIs(t, sess.WriteRTCP(&rtcp.ReceiverReport{SSRC: 1}), ErrDTLSNotKeyed)

		buf := make([]byte, RTPBufSize)
		require.NoError(t, peer.conn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
		n, err := peer.conn.Read(buf)
		require.True(t, os.IsTimeout(err), "media went out before the session was keyed: %d bytes, err=%v", n, err)

		keyWebrtcSession(sess, peer)
		want := webrtcRTPPacket(2, "protected")
		require.NoError(t, sess.WriteRTP(want))
		require.NoError(t, peer.conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		n, err = peer.conn.Read(buf)
		require.NoError(t, err)
		require.False(t, bytes.Contains(buf[:n], want.Payload), "the media went out unprotected")
		got := rtp.Packet{}
		decrypted, err := peer.recvSRTP.DecryptRTP(nil, buf[:n], &got.Header)
		require.NoError(t, err)
		require.NoError(t, got.Unmarshal(decrypted))
		require.Equal(t, want.Payload, got.Payload)
	})
}

// TestRTPSessionWebrtcMonitorSurvivesUnauthenticRTCP pins that one RTCP
// datagram the SRTCP context refuses does not end RTCP for the call. SRTCP
// follows SRTP (RFC 3711 section 3.4), so the datagram is discarded, and the
// monitor goes on to read the peer's next report. Returned as a read error, it
// stopped the monitor's reader, and with it reports in both directions.
func TestRTPSessionWebrtcMonitorSurvivesUnauthenticRTCP(t *testing.T) {
	sess, peer := newWebrtcSRTPPeers(t)
	keyWebrtcSession(sess, peer)

	rtpSess := NewRTPSessionWebrtc(sess)
	reports := make(chan rtcp.Packet, 8)
	rtpSess.OnReadRTCP(func(pkt rtcp.Packet, _ RTPReadStats) {
		select {
		case reports <- pkt:
		default:
		}
	})
	require.NoError(t, rtpSess.MonitorBackground())
	t.Cleanup(func() { _ = rtpSess.MonitorClose() })

	peer.send(t, plainRTCP(t, 0xbad0bad0))
	peer.sendRTCP(t, &rtcp.ReceiverReport{SSRC: 0x5eed0000})

	select {
	case pkt := <-reports:
		// The unprotected report must not have reached the monitor.
		requireReceiverReport(t, pkt, 0x5eed0000)
	case <-time.After(5 * time.Second):
		t.Fatal("the monitor read no report after the datagram that failed SRTCP authentication")
	}
}

// pacedWrites calls write once a millisecond with a rising sequence number
// until ctx ends, and returns the first error write returned.
func pacedWrites(ctx context.Context, write func(seq uint16) error) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for seq := uint16(1); ; seq++ {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		if err := write(seq); err != nil {
			return err
		}
	}
}

// TestWebrtcPacketPathsDuringSubsequentUpdate reads and writes media on a
// negotiated session while subsequent descriptions of the same ICE generation
// are staged and committed on it, as a hold and a resume do on a live call.
// The packet paths run on the application's goroutines and Finalize commits
// the direction and the codecs on the session's: the race detector fails the
// test where a packet path reads what the commit writes without the session's
// lock.
func TestWebrtcPacketPathsDuringSubsequentUpdate(t *testing.T) {
	ctx := t.Context()
	offerer, answerer := newNegotiatedWebrtcPair(t, "")
	requireBothFinalize(t, offerer, answerer, 40*time.Second, "the negotiation did not complete")

	// A subsequent offer of the same ICE generation and DTLS association,
	// send-only for a hold and as it is for the resume.
	resume, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.Contains(t, string(resume), "a=sendrecv\r\n")
	hold := bytes.Replace(resume, []byte("a=sendrecv\r\n"), []byte("a=sendonly\r\n"), 1)

	rtpSess := NewRTPSessionWebrtc(answerer)
	runCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	// The passed deadline is what returns the reader from a read no packet
	// ends.
	defer func() {
		stop()
		_ = answerer.StopRTP(1, time.Nanosecond)
	}()

	writeErrs := make(chan error, 2)
	wg.Go(func() {
		writeErrs <- pacedWrites(runCtx, func(seq uint16) error { return offerer.WriteRTP(dtlsTestPacket(seq)) })
	})
	wg.Go(func() {
		writeErrs <- pacedWrites(runCtx, func(seq uint16) error { return rtpSess.WriteRTP(dtlsTestPacket(seq)) })
	})

	type reads struct {
		count int
		err   error
	}
	// The reader says once that media flows and then nothing until it ends,
	// so that nothing but the session orders its reads and the updates below.
	flowing := make(chan struct{})
	readDone := make(chan reads, 1)
	wg.Go(func() {
		var r reads
		buf := make([]byte, RTPBufSize)
		for {
			pkt := rtp.Packet{}
			_, err := rtpSess.ReadRTP(buf, &pkt)
			if runCtx.Err() != nil {
				readDone <- r
				return
			}
			if err != nil {
				r.err = err
				readDone <- r
				return
			}
			r.count++
			if r.count == 1 {
				close(flowing)
			}
		}
	})

	select {
	case <-flowing:
	case <-time.After(10 * time.Second):
		t.Fatal("no media arrived at the session")
	}

	// The updates are spread over the media, a few packets apart.
	const updates = 40
	spread := time.NewTicker(3 * time.Millisecond)
	defer spread.Stop()
	for i := range updates {
		<-spread.C
		body, want := hold, sdp.ModeRecvonly
		if i%2 == 1 {
			body, want = resume, sdp.ModeSendrecv
		}
		require.NoError(t, answerer.RemoteSDP(ctx, body, false))
		require.NoError(t, answerer.Finalize(ctx))
		require.Equal(t, want, answerer.packetPath().mode)
	}

	stop()
	require.NoError(t, answerer.StopRTP(1, time.Nanosecond))
	wg.Wait()
	for range 2 {
		require.NoError(t, <-writeErrs)
	}
	r := <-readDone
	require.NoError(t, r.err)
	require.Greater(t, r.count, 1, "no media was read while the updates were committed")
}
