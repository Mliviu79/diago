// SPDX-License-Identifier: MPL-2.0

package media

import (
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRTPSessionMonitorOutlastsTheWaitForDTLSKeys pins that the RTCP monitor of
// a DTLS-SRTP session started before its handshake goes on reporting once the
// session is keyed. A caller's media has its monitor from the answer on, and
// is keyed only when the answer is acknowledged, so a report interval can pass
// in between. The session refuses to send a report it cannot protect; the
// monitor's writer ended on that refusal, and the call then sent no RTCP at
// all.
func TestRTPSessionMonitorOutlastsTheWaitForDTLSKeys(t *testing.T) {
	offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
	answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
	negotiateDTLS(t, offerer, answerer, nil)

	rtpSess := NewRTPSession(offerer)
	rtpSess.writeStats = RTPWriteStats{
		SSRC:                0x5eed0001,
		lastPacketTime:      time.Now(),
		lastPacketTimestamp: 1,
		sampleRate:          CodecAudioUlaw.SampleRate,
	}
	// The test hands the writer each report interval, so it knows the writer
	// took one: a writer that has ended takes no more.
	ticks := make(chan time.Time)
	rtpSess.rtcpTicker.Stop()
	rtpSess.rtcpTicker = &time.Ticker{C: ticks}
	require.NoError(t, rtpSess.MonitorBackground())
	t.Cleanup(func() { assert.NoError(t, rtpSess.MonitorClose()) })

	tick := func(what string) {
		t.Helper()
		select {
		case ticks <- time.Now():
		case <-time.After(5 * time.Second):
			t.Fatalf("the RTCP writer took no report interval %s", what)
		}
	}
	tick("before the session was keyed")
	tick("after an interval that passed without keys")

	finalizeBoth(t, offerer, answerer)
	tick("once the session was keyed")

	require.NoError(t, answerer.rtcpConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, RTPBufSize)
	n, _, err := answerer.rtcpConn.ReadFrom(buf)
	require.NoError(t, err, "no report reached the peer once the session was keyed")
	require.NotZero(t, n)
}
