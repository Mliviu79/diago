// SPDX-License-Identifier: MPL-2.0

package media

import (
	"errors"
	"time"
)

// errWebRTCNoRTPTransport is what a deadline call returns on a session that
// has no remote description yet, and so no packet mux to set a deadline on.
// StopRTP returns an error of the same text.
var errWebRTCNoRTPTransport = errors.New("WebRTC RTP transport is not initialized")

// rtpDeadlineMux returns the packet mux the session's RTP deadlines are set
// on. A session has one from its first remote description to its end, and
// shares it with no other session: a Fork runs a new ICE generation on a mux
// of its own, which is why the deadlines of the session it replaces have to
// be carried to it.
func (m *MediaSessionWebrtc) rtpDeadlineMux() (*webRTCPacketMux, error) {
	m.mu.Lock()
	mux := m.mux
	m.mu.Unlock()
	if mux == nil {
		return nil, errWebRTCNoRTPTransport
	}
	return mux, nil
}

// setRTPDeadline records t as the deadline asked for and sets it on the RTP
// stream of mux, on the side MediaSession.StopRTP sets one for rw: the read
// side when bit 1 is set, the write side when only bit 2 is, and both when
// neither is. The record stays locked until the stream has the deadline, so
// the two never disagree about what was asked for last.
func (m *MediaSessionWebrtc) setRTPDeadline(mux *webRTCPacketMux, rw int8, t time.Time) error {
	rec := &m.deadlines
	rec.mu.Lock()
	defer rec.mu.Unlock()
	switch {
	case rw&1 > 0:
		rec.read = t
		return mux.rtp.SetReadDeadline(t)
	case rw&2 > 0:
		rec.write = t
		return mux.rtp.SetWriteDeadline(t)
	default:
		rec.read, rec.write = t, t
		return mux.rtp.SetDeadline(t)
	}
}

// startRTP is StartRTP: it clears the read deadline for the values of rw
// StopRTP sets one for, bit 1 or zero, and does nothing for any other.
func (m *MediaSessionWebrtc) startRTP(rw int8) error {
	mux, err := m.rtpDeadlineMux()
	if err != nil {
		return err
	}
	if rw&1 == 0 && rw != 0 {
		return nil
	}
	return m.setRTPDeadline(mux, 1, time.Time{})
}

// SetRTPDeadline sets t as the deadline of the session's RTP reads when rw
// has bit 1 set, of its writes when rw has bit 2 set and not bit 1, and of
// both when rw has neither. The zero time clears it.
//
// A read that waits when t passes returns a timeout error, and so does every
// read made after it, until the deadline is moved or cleared. A write made
// after t sends nothing and returns an error. That error is a timeout only
// where the transport reports one: the ICE agent reports a write its socket
// refused as a write of no bytes, which WriteRTP returns as io.ErrShortWrite.
//
// It is the one call that reaches the write side: StopRTP and StartRTP set
// and clear a read deadline only, whatever rw they are given, so a write
// deadline set here is cleared here. The session writes everything over the
// one candidate pair ICE selected, and the write deadline is that pair's: it
// also stops the RTCP, DTLS and ICE packets sent while it has passed, and it
// reaches no socket while ICE has not selected a pair.
//
// On a session without a remote description, which has no transport yet, it
// returns an error, as StopRTP does.
func (m *MediaSessionWebrtc) SetRTPDeadline(rw int8, t time.Time) error {
	mux, err := m.rtpDeadlineMux()
	if err != nil {
		return err
	}
	return m.setRTPDeadline(mux, rw, t)
}

// CarryRTPDeadlines takes over the RTP read and write deadlines StopRTP,
// StartRTP and SetRTPDeadline last set on from, a session this one replaces,
// and sets them on this session's transport. A deadline set to end a read or
// a write then also ends the one that carries on over this session, as a read
// of RTPPacketReader does once the reader is moved, and a read that was
// stopped stays stopped.
//
// It is called once this session has its remote description, without which
// it has no transport and CarryRTPDeadlines returns an error, and before the
// session is put in from's place, so that a read taken up on it finds the
// deadlines here. The two sessions run on transports of their own: a deadline
// set through from afterwards is not this session's.
func (m *MediaSessionWebrtc) CarryRTPDeadlines(from *MediaSessionWebrtc) error {
	if from == m {
		return nil
	}
	mux, err := m.rtpDeadlineMux()
	if err != nil {
		return err
	}

	rec := &from.deadlines
	rec.mu.Lock()
	read, write := rec.read, rec.write
	rec.mu.Unlock()

	own := &m.deadlines
	own.mu.Lock()
	defer own.mu.Unlock()
	own.read, own.write = read, write
	return errors.Join(mux.rtp.SetReadDeadline(read), mux.rtp.SetWriteDeadline(write))
}

// interruptRTPRead ends a read waiting on the session's RTP stream with a
// read deadline that has passed. The deadline is not recorded: it is not one
// the application asked for, and restoreRTPReadDeadline puts the recorded one
// back in its place before the stream is read again.
func (m *MediaSessionWebrtc) interruptRTPRead() error {
	mux, err := m.rtpDeadlineMux()
	if err != nil {
		return err
	}
	return mux.rtp.SetReadDeadline(time.Now())
}

// restoreRTPReadDeadline sets the RTP stream's read deadline to the one
// StopRTP, StartRTP or SetRTPDeadline last asked for, which is none when none
// of them did.
func (m *MediaSessionWebrtc) restoreRTPReadDeadline() error {
	mux, err := m.rtpDeadlineMux()
	if err != nil {
		return err
	}
	rec := &m.deadlines
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return mux.rtp.SetReadDeadline(rec.read)
}
