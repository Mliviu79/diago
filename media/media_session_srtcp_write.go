// SPDX-License-Identifier: MPL-2.0

package media

import (
	"fmt"
	"io"
)

// writeSRTCP protects marshaled RTCP, one packet or a compound of several,
// with the session's SRTP keys and sends it. A session that has keys sends
// all of its RTCP as SRTCP (RFC 3711 section 3.4), whichever call wrote it:
// a compound packet handed to the socket as it was marshaled carries the
// reports of a secure call in the clear.
//
// The caller has found the session keyed.
func (m *MediaSession) writeSRTCP(data []byte) error {
	wbuf := rtpBufPool.Get()
	defer rtpBufPool.Put(wbuf)

	protected, err := m.localCtxSRTP.EncryptRTCP(wbuf.([]byte), data, nil)
	if err != nil {
		return fmt.Errorf("protect RTCP: %w", err)
	}

	// The socket's error is returned as the unprotected write returns it,
	// since callers test it for a timeout.
	n, err := m.WriteRTCPRaw(protected)
	if err != nil {
		return err
	}
	if n != len(protected) {
		return io.ErrShortWrite
	}
	return nil
}
