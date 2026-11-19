// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testMIDExtmap = "urn:ietf:params:rtp-hdrext:sdes:mid"

// webrtcOfferWithExtmap returns offer, an SDP with one audio section, with the
// a=extmap lines media in that section and the lines session at the session
// level.
func webrtcOfferWithExtmap(t *testing.T, offer []byte, session, media []string) []byte {
	t.Helper()
	body := string(offer)
	for _, l := range session {
		require.Equal(t, 1, strings.Count(body, "t=0 0\r\n"), "the offer\n%s", body)
		body = strings.Replace(body, "t=0 0\r\n", "t=0 0\r\n"+l+"\r\n", 1)
	}
	for _, l := range media {
		require.Equal(t, 1, strings.Count(body, "a=rtcp-mux\r\n"), "the offer\n%s", body)
		body = strings.Replace(body, "a=rtcp-mux\r\n", l+"\r\na=rtcp-mux\r\n", 1)
	}
	return []byte(body)
}

// TestMediaSessionWebrtcEnablesTheOfferedMIDExtension pins when the SDPs of a
// session enable the RTP header extension that carries the tag of its audio
// stream. RFC 8843 section 9.1 has it "in each bundled RTP-based "m=" section
// in every offer and answer", and RFC 8285 section 7 lets an answer enable
// only what the offer mapped, under the offer's identifier. So it is enabled
// for an audio stream offered in a BUNDLE group with the extension mapped, in
// the answer and in the offers of ours that follow, and for no other.
func TestMediaSessionWebrtcEnablesTheOfferedMIDExtension(t *testing.T) {
	const bundled = "a=group:BUNDLE audio"
	type row struct {
		name string
		// group is the a=group line of the offer, whose audio stream has the
		// tag audio.
		group string
		// session and media are the a=extmap lines of the offer at the
		// session level and in its audio section.
		session, media []string
		// want is the a=extmap lines of our SDPs.
		want []string
	}
	rows := []row{
		{
			name:  "mapped_in_the_audio_section",
			group: bundled,
			media: []string{"a=extmap:3 " + testMIDExtmap},
			want:  []string{"a=extmap:3 " + testMIDExtmap},
		},
		{
			name:    "mapped_at_the_session_level",
			group:   bundled,
			session: []string{"a=extmap:3 " + testMIDExtmap},
			want:    []string{"a=extmap:3 " + testMIDExtmap},
		},
		{
			name:  "mapped_both_ways_in_words",
			group: bundled,
			media: []string{"a=extmap:3/sendrecv " + testMIDExtmap},
			want:  []string{"a=extmap:3 " + testMIDExtmap},
		},
		{
			name:  "beside_extensions_we_do_not_use",
			group: bundled,
			media: []string{"a=extmap:1 urn:ietf:params:rtp-hdrext:ssrc-audio-level", "a=extmap:9 " + testMIDExtmap, "a=extmap:10 urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id"},
			want:  []string{"a=extmap:9 " + testMIDExtmap},
		},
		{
			name:  "under_an_identifier_of_the_two-byte_form",
			group: bundled,
			media: []string{"a=extmap:200 " + testMIDExtmap},
			want:  []string{"a=extmap:200 " + testMIDExtmap},
		},
		{name: "not_mapped", group: bundled, media: []string{"a=extmap:1 urn:ietf:params:rtp-hdrext:ssrc-audio-level"}},
		{name: "none_mapped", group: bundled},
		{name: "mapped_for_the_peer_to_send_alone", group: bundled, media: []string{"a=extmap:3/sendonly " + testMIDExtmap}},
		{name: "mapped_for_the_peer_to_receive_alone", group: bundled, media: []string{"a=extmap:3/recvonly " + testMIDExtmap}},
		{name: "mapped_inactive", group: bundled, media: []string{"a=extmap:3/inactive " + testMIDExtmap}},
		{name: "mapped_to_the_padding_identifier", group: bundled, media: []string{"a=extmap:0 " + testMIDExtmap}},
		{name: "mapped_to_an_identifier_to_remap", group: bundled, media: []string{"a=extmap:4096 " + testMIDExtmap}},
		{name: "an_audio_stream_in_no_group", media: []string{"a=extmap:3 " + testMIDExtmap}},
		{name: "an_audio_stream_outside_the_group", group: "a=group:BUNDLE other", media: []string{"a=extmap:3 " + testMIDExtmap}},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ctx := t.Context()
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
			alone, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			require.Empty(t, sdpLines(alone, "a=extmap:"), "an offer of ours before any of the peer's\n%s", alone)
			offer := webrtcOfferWithExtmap(t, webrtcTaggedOffer(t, alone, r.group, "audio", nil, nil), r.session, r.media)

			require.NoError(t, answerer.RemoteSDP(ctx, offer, false), "applying the offer\n%s", offer)
			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			assert.Equal(t, r.want, sdpLines(answer, "a=extmap:"), "the answer\n%s", answer)
			lines := streamsSDPLines(answer)
			if len(r.want) > 0 {
				assert.Greater(t, slices.Index(lines, r.want[0]), slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "m=audio ") }),
					"the extension is mapped in the audio section\n%s", answer)
			}

			ours, err := answerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			assert.Equal(t, r.want, sdpLines(ours, "a=extmap:"), "our offer\n%s", ours)
		})
	}
}

// rfc3550Extension is the profile of a header extension that is not of the
// forms RFC 8285 defines: RFC 3550 section 5.3.1 leaves it to the application.
const rfc3550Extension = 0x1234

// TestMIDExtensionTagsPackets pins the header a packet is sent with on a
// session whose peer mapped the MID extension: the caller's header and the tag
// as an extension element in the one-byte form of RFC 8285, or the two-byte
// form when the identifier or the tag needs it. The caller's packet is left as
// it is, and one whose own header extension cannot take the element is sent as
// the caller made it.
func TestMIDExtensionTagsPackets(t *testing.T) {
	longTag := strings.Repeat("t", maxOneByteExtensionLen+1)
	withExtension := func(profile uint16, id uint8, payload []byte) func(t *testing.T, h *rtp.Header) {
		return func(t *testing.T, h *rtp.Header) {
			t.Helper()
			h.Extension = true
			h.ExtensionProfile = profile
			require.NoError(t, h.SetExtension(id, payload))
		}
	}
	for _, tc := range []struct {
		name string
		ext  midExtension
		// prepare, when set, gives the caller's packet a header extension.
		prepare func(t *testing.T, h *rtp.Header)
		// wantProfile is the extension profile of the packet sent, or 0 for
		// a packet sent as the caller made it.
		wantProfile uint16
	}{
		{name: "one-byte_form", ext: midExtension{id: 3, tag: []byte("audio")}, wantProfile: rtp.ExtensionProfileOneByte},
		{name: "one-byte_form_at_its_bounds", ext: midExtension{id: 14, tag: []byte(longTag[:16])}, wantProfile: rtp.ExtensionProfileOneByte},
		{name: "two-byte_form_for_the_identifier", ext: midExtension{id: 15, tag: []byte("a")}, wantProfile: rtp.ExtensionProfileTwoByte},
		{name: "two-byte_form_for_the_tag", ext: midExtension{id: 3, tag: []byte(longTag)}, wantProfile: rtp.ExtensionProfileTwoByte},
		{
			name:        "beside_an_element_of_the_caller",
			ext:         midExtension{id: 3, tag: []byte("audio")},
			prepare:     withExtension(rtp.ExtensionProfileOneByte, 5, []byte{0x80}),
			wantProfile: rtp.ExtensionProfileOneByte,
		},
		{
			name:        "beside_a_two-byte_element_of_the_caller",
			ext:         midExtension{id: 3, tag: []byte("audio")},
			prepare:     withExtension(rtp.ExtensionProfileTwoByte, 5, []byte{0x80}),
			wantProfile: rtp.ExtensionProfileTwoByte,
		},
		{
			name:    "not_beside_a_one-byte_element_when_the_tag_needs_two",
			ext:     midExtension{id: 3, tag: []byte(longTag)},
			prepare: withExtension(rtp.ExtensionProfileOneByte, 5, []byte{0x80}),
		},
		{
			name:    "not_in_an_extension_of_the_application",
			ext:     midExtension{id: 3, tag: []byte("audio")},
			prepare: withExtension(rfc3550Extension, 0, []byte{1, 2, 3, 4}),
		},
		{name: "no_extension_mapped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkt := dtlsTestPacket(7)
			if tc.prepare != nil {
				tc.prepare(t, &pkt.Header)
			}
			before, err := pkt.Marshal()
			require.NoError(t, err)

			sent := tc.ext.tagged(pkt)

			after, err := pkt.Marshal()
			require.NoError(t, err)
			assert.Equal(t, before, after, "the caller's packet")
			if tc.wantProfile == 0 {
				assert.Same(t, pkt, sent, "the packet is sent as the caller made it")
				return
			}
			wire, err := sent.Marshal()
			require.NoError(t, err)
			var got rtp.Packet
			require.NoError(t, got.Unmarshal(wire))
			assert.Equal(t, tc.wantProfile, got.ExtensionProfile, "the extension profile")
			assert.Equal(t, tc.ext.tag, got.GetExtension(tc.ext.id), "the tag sent")
			assert.Equal(t, pkt.Payload, got.Payload, "the payload")
			assert.Equal(t, pkt.SequenceNumber, got.SequenceNumber)
			assert.Equal(t, pkt.SSRC, got.SSRC)
			if tc.prepare != nil {
				assert.Equal(t, []byte{0x80}, got.GetExtension(5), "the caller's element")
			}
		})
	}
}

// TestMediaSessionWebrtcSendsTheTagOfItsBundledStream negotiates two sessions
// on an offer that bundles the audio stream and maps the MID extension, and
// reads what the answerer sends: every RTP packet carries the tag the offer
// gave the stream (RFC 8843 sections 9.2 and 15.2), also after a re-offer of
// ours, and the source description of its RTCP reports carries it as an SDES
// MID item (section 15.1). The offering session of the test sends its packets
// as they are until it has received an offer that maps the extension, which
// our re-offer is.
func TestMediaSessionWebrtcSendsTheTagOfItsBundledStream(t *testing.T) {
	const id = 3
	offerer, answerer, _, answer := newEstablishedWebrtcPair(t, func(offer string) string {
		tagged := webrtcTaggedOffer(t, []byte(offer), "a=group:BUNDLE audio", "audio", nil, nil)
		return string(webrtcOfferWithExtmap(t, tagged, nil, []string{"a=extmap:" + strconv.Itoa(id) + " " + testMIDExtmap}))
	})
	require.Equal(t, []string{"a=extmap:" + strconv.Itoa(id) + " " + testMIDExtmap}, sdpLines(answer, "a=extmap:"), "the answer\n%s", answer)

	read := func(t *testing.T, from, to *MediaSessionWebrtc, seq uint16) rtp.Packet {
		t.Helper()
		pkt := dtlsTestPacket(seq)
		require.NoError(t, from.WriteRTP(pkt))
		assert.False(t, pkt.Header.Extension, "the caller's packet is left as it is")
		require.NoError(t, to.StopRTP(1, 5*time.Second))
		got := rtp.Packet{}
		_, err := to.ReadRTP(make([]byte, RTPBufSize), &got)
		require.NoError(t, err, "the media did not arrive and decrypt")
		require.NoError(t, to.StartRTP(1))
		require.Equal(t, seq, got.SequenceNumber)
		require.Equal(t, pkt.Payload, got.Payload)
		return got
	}

	for seq := uint16(1); seq <= 3; seq++ {
		got := read(t, answerer, offerer, seq)
		assert.Equal(t, []byte("audio"), got.GetExtension(id), "the tag of packet %d", seq)
	}
	got := read(t, offerer, answerer, 4)
	assert.False(t, got.Header.Extension, "a session that received no offer tags nothing")

	sd := rtcp.NewCNAMESourceDescription(0x5eed0000, "cname")
	identified := []rtcp.SourceDescriptionChunk{{
		Source: 0x5eed0000,
		Items: []rtcp.SourceDescriptionItem{
			{Type: rtcp.SDESCNAME, Text: "cname"},
			{Type: sdesMID, Text: "audio"},
		},
	}}
	assert.Equal(t, identified, answerer.identifiedSource(sd).Chunks, "the source description sent")
	assert.Equal(t, rtcp.NewCNAMESourceDescription(0x5eed0000, "cname"), sd, "the session's own description is left as it is")
	assert.Same(t, sd, offerer.identifiedSource(sd), "a session that received no offer adds nothing")

	reoffer, err := answerer.ReOffer("")
	require.NoError(t, err)
	assert.Equal(t, []string{"a=extmap:" + strconv.Itoa(id) + " " + testMIDExtmap}, sdpLines(reoffer, "a=extmap:"), "our re-offer\n%s", reoffer)
	exchangeWebrtcReOffer(t, answerer, offerer, reoffer)
	got = read(t, answerer, offerer, 5)
	assert.Equal(t, []byte("audio"), got.GetExtension(id), "the tag after our re-offer")
	// The re-offer is the first offer its receiver was given.
	got = read(t, offerer, answerer, 6)
	assert.Equal(t, []byte("audio"), got.GetExtension(id), "the tag its receiver sends")
	assert.Equal(t, identified, offerer.identifiedSource(sd).Chunks, "the source description its receiver sends")
}

// TestMediaSessionWebrtcTagsItsMediaForAPionPeer answers a Pion PeerConnection
// that offers its audio stream in a BUNDLE group with the MID extension
// mapped, as a browser stack does, and reads on the Pion side what the session
// sends: RTP packets whose header extension carries the tag Pion gave the
// stream, and RTCP reports whose source description carries it too.
func TestMediaSessionWebrtcTagsItsMediaForAPionPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	peer := newPionPeer(t, testMIDExtmap)
	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "browser")
	require.NoError(t, err)
	_, err = peer.AddTrack(localTrack)
	require.NoError(t, err)
	type remoteMedia struct {
		track    *webrtc.TrackRemote
		receiver *webrtc.RTPReceiver
	}
	remote := make(chan remoteMedia, 1)
	peer.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		remote <- remoteMedia{track: track, receiver: receiver}
	})
	offer := pionOffer(ctx, t, peer)
	tags := streamTagsOf(offer)
	require.Len(t, tags, 1, "the offer\n%s", offer)
	require.Equal(t, []string{"a=group:BUNDLE " + tags[0]}, sdpLines(offer, "a=group:"), "the offer\n%s", offer)
	extmap := sdpLines(offer, "a=extmap:")
	require.Len(t, extmap, 1, "the offer\n%s", offer)
	require.True(t, strings.HasSuffix(extmap[0], " "+testMIDExtmap), "the offer\n%s", offer)
	id, err := strconv.ParseUint(strings.TrimPrefix(strings.Fields(extmap[0])[0], "a=extmap:"), 10, 8)
	require.NoError(t, err, extmap[0])

	session := newPionAnswerer(ctx, t)
	require.NoError(t, session.RemoteSDP(ctx, offer, false), "the offer\n%s", offer)
	answer, err := session.LocalSDP(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, extmap, sdpLines(answer, "a=extmap:"), "the answer\n%s", answer)
	assert.Equal(t, tags, streamTagsOf(answer), "the answer\n%s", answer)
	require.NoError(t, peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}),
		"the answer\n%s", answer)

	rtpSession := NewRTPSessionWebrtc(session)
	rtpSession.RTCPReportInterval = 25 * time.Millisecond
	packetWriter := NewRTPPacketWriter(rtpSession, session.Codec())
	require.NoError(t, session.Finalize(ctx))
	require.NoError(t, rtpSession.MonitorBackground())
	defer func() {
		if err := rtpSession.MonitorClose(); err != nil {
			t.Errorf("closing the RTCP monitors: %v", err)
		}
	}()

	payload := []byte("tagged-audio")
	_, err = packetWriter.WriteSamples(payload, CodecAudioUlaw.SampleTimestamp(), true, CodecAudioUlaw.PayloadType)
	require.NoError(t, err)
	var media remoteMedia
	select {
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	case media = <-remote:
	}
	pkt, _, err := media.track.ReadRTP()
	require.NoError(t, err)
	assert.Equal(t, payload, pkt.Payload)
	assert.Equal(t, []byte(tags[0]), pkt.GetExtension(uint8(id)), "the tag of the packet Pion received")

	type rtcpRead struct {
		packets []rtcp.Packet
		err     error
	}
	reports := make(chan rtcpRead, 1)
	go func() {
		packets, _, readErr := media.receiver.ReadRTCP()
		reports <- rtcpRead{packets: packets, err: readErr}
	}()
	select {
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	case report := <-reports:
		require.NoError(t, report.err)
		var items []rtcp.SourceDescriptionItem
		for _, p := range report.packets {
			if sd, ok := p.(*rtcp.SourceDescription); ok {
				for _, chunk := range sd.Chunks {
					items = append(items, chunk.Items...)
				}
			}
		}
		assert.Contains(t, items, rtcp.SourceDescriptionItem{Type: sdesMID, Text: tags[0]}, "the source description Pion received")
	}
}
