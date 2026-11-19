// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Emir Aganovic

package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/pion/dtls/v3"
	"github.com/pion/ice/v4"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	webrtcsdp "github.com/pion/sdp/v3"
	"github.com/pion/srtp/v3"
	"github.com/pion/stun/v3"
)

// ErrWebRTCICERestart reports that a subsequent SDP changed both remote ICE
// credentials and therefore needs a replacement MediaSessionWebrtc transport.
var ErrWebRTCICERestart = errors.New("remote WebRTC SDP starts a new ICE generation")

// MediaSessionWebrtcConfig contains transport settings which do not belong to
// the codec-level media API. ICEURLs accepts STUN and TURN URLs understood by
// Pion (for example stun:stun.example.org:3478).
type MediaSessionWebrtcConfig struct {
	ICEURLs        []string
	IPFamilies     []ICEIPFamily
	CandidateTypes []ICECandidateType
	PortMin        uint16
	PortMax        uint16
	Timeouts       ICETimeouts

	IncludeLoopback bool
	InterfaceFilter func(interfaceName string) bool
	// IPFilter controls which local addresses may become ICE candidates.
	IPFilter func(ip netip.Addr) bool
	// RemoteIPFilter controls which addresses from remote SDP are accepted.
	RemoteIPFilter func(ip netip.Addr) bool

	// NetworkTypes is kept for source compatibility. Only UDP4 and UDP6 are
	// accepted. New code should use IPFamilies so configuration is not tied to
	// the current ICE implementation.
	//
	// Deprecated: use IPFamilies.
	NetworkTypes []ice.NetworkType

	DTLS DTLSConfig

	// ICE holds the further settings of the ICE agent: see ICEConfig.
	ICE ICEConfig

	// OfferWithoutBundle makes the first offer of the session, the one it
	// writes before any offer of the peer, describe the audio stream with no
	// a=group:BUNDLE line and no a=mid line. The zero value offers the stream
	// under the tag 0, in a BUNDLE group of its own.
	//
	// The SDPs of ours that follow that offer keep its form, a session made
	// by Fork included, until an offer of the peer is committed: from then on
	// they identify the streams as that offer did. An answer always follows
	// its offer, whatever this field holds.
	OfferWithoutBundle bool
}

// MediaSessionWebrtc is the direct ICE + DTLS-SRTP media stack. It intentionally
// does not embed MediaSession: legacy RTP uses two known UDP destinations,
// whereas WebRTC must send every DTLS/SRTP/SRTCP packet through the candidate
// pair selected and consent-checked by ICE.
//
// Signalling lifecycle:
//
//   - Init gathers local candidates (non-trickle ICE).
//   - LocalSDP(false) creates an offer, or RemoteSDP(..., false) consumes one.
//   - RemoteSDP starts ICE as controlling for a local offer and controlled for
//     a local answer. It returns before connectivity checks finish.
//   - Finalize waits for ICE, performs DTLS, verifies the SDP fingerprint and
//     derives the SRTP keys. RTP must not be used before Finalize succeeds.
type MediaSessionWebrtc struct {
	Laddr  string
	Raddr  string
	Codecs []Codec
	Mode   string

	Config MediaSessionWebrtcConfig

	mu                         sync.Mutex
	agent                      *ice.Agent
	localCandidates            []ice.Candidate
	localUfrag                 string
	localPwd                   string
	remoteUfrag                string
	remotePwd                  string
	remoteFingerprintAlgorithm string
	remoteFingerprint          string
	localSetup                 string
	modePreference             string
	codec                      Codec
	filterCodecs               []Codec
	iceConn                    *ice.Conn
	mux                        *webRTCPacketMux
	dtlsConn                   *dtls.Conn
	localCtxSRTP               *srtp.Context
	remoteCtxSRTP              *srtp.Context
	localCtxSRTCP              *srtp.Context
	remoteCtxSRTCP             *srtp.Context
	rtcpReducedSize            bool
	pendingCodecs              []Codec
	pendingMode                string
	pendingRTCPReducedSize     bool
	ready                      bool
	closed                     bool
	writeRTPBuf                []byte
	writeRTCPBuf               []byte
	onICEStateChange           func(ice.ConnectionState)

	webrtcSessionState
}

// Fork creates a fresh WebRTC transport for a locally initiated ICE restart.
// Codec and configuration slices are copied because SDP negotiation mutates the
// session's codec list, while certificates and filters remain immutable config.
func (m *MediaSessionWebrtc) Fork(ctx context.Context) (*MediaSessionWebrtc, error) {
	m.mu.Lock()
	conf := m.Config
	conf.ICEURLs = slices.Clone(conf.ICEURLs)
	conf.IPFamilies = slices.Clone(conf.IPFamilies)
	conf.CandidateTypes = slices.Clone(conf.CandidateTypes)
	conf.NetworkTypes = slices.Clone(conf.NetworkTypes)
	conf.DTLS.Certificates = slices.Clone(conf.DTLS.Certificates)
	conf.DTLS.SRTPProfiles = slices.Clone(conf.DTLS.SRTPProfiles)
	conf.DTLS.EllipticCurves = slices.Clone(conf.DTLS.EllipticCurves)
	codecs := slices.Clone(m.Codecs)
	mode := m.Mode
	modePreference := m.modePreference
	onICEStateChange := m.onICEStateChange
	m.mu.Unlock()

	fork := &MediaSessionWebrtc{Codecs: codecs, Mode: mode, modePreference: modePreference}
	fork.origin = m.sharedOrigin()
	fork.streams.current = m.forkedStreams()
	fork.source = m.sharedSource()
	if err := fork.Init(ctx, conf, onICEStateChange); err != nil {
		return nil, err
	}
	return fork, nil
}

// Init creates the ICE agent and waits for candidate gathering to finish. SIP
// offer/answer has no standard trickle-ICE exchange, so the SDP returned later
// contains a complete candidate set and a=end-of-candidates.
func (m *MediaSessionWebrtc) Init(
	ctx context.Context,
	conf MediaSessionWebrtcConfig,
	onICEStateChange ...func(ice.ConnectionState),
) (err error) {
	if len(onICEStateChange) > 1 {
		return fmt.Errorf("only one ICE state handler can be configured")
	}
	m.mu.Lock()
	if m.agent != nil {
		m.mu.Unlock()
		return fmt.Errorf("webrtc media session is already initialized")
	}
	m.Config = conf
	if len(onICEStateChange) == 1 {
		m.onICEStateChange = onICEStateChange[0]
	}
	m.mu.Unlock()

	if len(conf.DTLS.Certificates) == 0 {
		return fmt.Errorf("webrtc media session requires a DTLS certificate")
	}
	if err := conf.DTLS.checkCertificates(); err != nil {
		return fmt.Errorf("webrtc media session: %w", err)
	}
	if len(m.Codecs) == 0 {
		return fmt.Errorf("webrtc media session requires at least one codec")
	}

	networkTypes, err := webRTCNetworkTypes(conf)
	if err != nil {
		return &ICEError{Phase: ICEPhaseGathering, Err: err}
	}
	candidateTypes, err := webRTCCandidateTypes(conf)
	if err != nil {
		return &ICEError{Phase: ICEPhaseGathering, Err: err}
	}
	timeouts, err := normalizeICETimeouts(conf.Timeouts)
	if err != nil {
		return &ICEError{Phase: ICEPhaseGathering, Err: err}
	}
	opts := []ice.AgentOption{
		ice.WithNetworkTypes(networkTypes),
		ice.WithCandidateTypes(candidateTypes),
		ice.WithDisconnectedTimeout(timeouts.Disconnected),
		ice.WithFailedTimeout(timeouts.Failed),
		ice.WithKeepaliveInterval(timeouts.Keepalive),
	}
	if conf.PortMin != 0 || conf.PortMax != 0 {
		if conf.PortMin == 0 || conf.PortMax < conf.PortMin {
			return &ICEError{
				Phase: ICEPhaseGathering,
				Err:   fmt.Errorf("invalid UDP port range %d-%d", conf.PortMin, conf.PortMax),
			}
		}
		opts = append(opts, ice.WithPortRange(conf.PortMin, conf.PortMax))
	}
	if conf.IncludeLoopback {
		opts = append(opts, ice.WithIncludeLoopback())
	}
	if conf.InterfaceFilter != nil {
		opts = append(opts, ice.WithInterfaceFilter(conf.InterfaceFilter))
	}
	if filter := webRTCIPFilter(conf.IPFilter); filter != nil {
		opts = append(opts, ice.WithIPFilter(filter))
	}
	if filter := webRTCIPFilter(conf.RemoteIPFilter); filter != nil {
		opts = append(opts, ice.WithRemoteIPFilter(filter))
	}
	if len(conf.ICEURLs) > 0 {
		urls := make([]*stun.URI, 0, len(conf.ICEURLs))
		for _, rawURL := range conf.ICEURLs {
			u, err := stun.ParseURI(rawURL)
			if err != nil {
				return &ICEError{
					Phase: ICEPhaseGathering,
					Err:   fmt.Errorf("parse URL %q: %w", rawURL, err),
				}
			}
			urls = append(urls, u)
		}
		opts = append(opts, ice.WithUrls(urls))
	}
	if opts, err = m.appendICEConfigOptions(ctx, opts, conf); err != nil {
		return &ICEError{Phase: ICEPhaseGathering, Err: err}
	}
	defer func() { err = m.releaseUnusedICESocket(err) }()

	agent, err := ice.NewAgentWithOptions(opts...)
	if err != nil {
		return &ICEError{Phase: ICEPhaseGathering, Err: fmt.Errorf("create ICE agent: %w", err)}
	}
	if err = agent.OnConnectionStateChange(func(state ice.ConnectionState) {
		m.notifyICEStateChange(state)
	}); err != nil {
		_ = agent.Close()
		return &ICEError{Phase: ICEPhaseGathering, Err: fmt.Errorf("set ICE state handler: %w", err)}
	}
	gathered := make(chan struct{})
	var gatherOnce sync.Once
	if err = agent.OnCandidate(func(candidate ice.Candidate) {
		if candidate == nil {
			gatherOnce.Do(func() { close(gathered) })
			return
		}
		m.mu.Lock()
		m.localCandidates = append(m.localCandidates, candidate)
		m.mu.Unlock()
	}); err != nil {
		_ = agent.Close()
		return &ICEError{Phase: ICEPhaseGathering, Err: fmt.Errorf("set candidate handler: %w", err)}
	}
	ufrag, pwd, err := agent.GetLocalUserCredentials()
	if err != nil {
		_ = agent.Close()
		return &ICEError{Phase: ICEPhaseGathering, Err: fmt.Errorf("get local credentials: %w", err)}
	}
	m.mu.Lock()
	m.agent = agent
	m.localUfrag = ufrag
	m.localPwd = pwd
	m.mu.Unlock()
	m.notifyICEStateChange(ice.ConnectionStateNew)

	if err = agent.GatherCandidates(); err != nil {
		_ = m.Close()
		return &ICEError{Phase: ICEPhaseGathering, Err: fmt.Errorf("gather candidates: %w", err)}
	}
	select {
	case <-ctx.Done():
		_ = m.Close()
		return &ICEError{Phase: ICEPhaseGathering, Err: ctx.Err()}
	case <-gathered:
	}
	m.mu.Lock()
	count := len(m.localCandidates)
	m.mu.Unlock()
	if count == 0 {
		_ = m.Close()
		return &ICEError{Phase: ICEPhaseGathering, Err: fmt.Errorf("no local candidates matched the configured policy")}
	}
	return nil
}

func (m *MediaSessionWebrtc) notifyICEStateChange(state ice.ConnectionState) {
	m.mu.Lock()
	handler := m.onICEStateChange
	m.mu.Unlock()
	if handler != nil {
		handler(state)
	}
}

func (m *MediaSessionWebrtc) Codec() Codec {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.codec.SampleRate != 0 {
		return m.codec
	}
	codec, _ := CodecAudioFromList(m.Codecs)
	return codec
}

func (m *MediaSessionWebrtc) CommonCodecs() []Codec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.filterCodecs)
}

// LocalSDP returns a complete, non-trickle WebRTC SDP. answered controls the
// DTLS setup attribute: an offer is actpass, unless Config.DTLS.SDPSetupRole
// chooses another value; an answer uses the role selected while parsing the
// remote offer. An answer also repeats the m= transport of that offer.
//
// The body has one m= line for each stream of the offer it answers, or of the
// last offer of the peer the session committed to when the body is an offer
// of ours, in that offer's order: the audio stream, and every other stream
// rejected with port 0 (RFC 3264 sections 6 and 8). A rejected stream keeps
// the a=rtpmap lines the offer gave the formats of its m= line.
//
// A body with more than one m= line declares, in an a=ssrc line of its audio
// section, the source the session sends as (RFC 5576): the SSRC the first
// RTPPacketWriter made over the session writes with and the CNAME of the
// reports of its RTPSessionWebrtc. A body with one m= line declares none, and
// neither does one whose offer declares that SSRC for a source of the peer.
//
// The streams are identified as that offer identified them: each under its
// a=mid tag (RFC 5888 section 9.1), the audio stream alone in the BUNDLE group
// the offer listed it in (RFC 8843 sections 7.3 and 7.3.3), with the header
// extension the offer mapped to its tag (RFC 8843 section 9.1). A stream the
// offer gave no tag has no a=mid line, and a body whose offer listed the audio
// stream in no BUNDLE group has no a=group line. Before any offer of the peer
// the audio stream has the tag 0 in a group of its own, or neither line when
// Config.OfferWithoutBundle is set.
func (m *MediaSessionWebrtc) LocalSDP(_ context.Context, answered bool) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.agent == nil || len(m.localCandidates) == 0 {
		return nil, fmt.Errorf("webrtc media session is not initialized")
	}
	setup := m.localDTLSSetup(answered)
	if answered {
		if m.localSetup == "" {
			return nil, fmt.Errorf("remote WebRTC offer must be parsed before creating an answer")
		}
		setup = m.localSetup
	} else {
		m.modePreference = m.Mode
	}
	codecs := m.Codecs
	if len(m.filterCodecs) > 0 {
		codecs = m.filterCodecs
	}
	mode := m.Mode
	rtcpReducedSize := m.rtcpReducedSize
	if answered && len(m.pendingCodecs) > 0 {
		codecs = m.pendingCodecs
		mode = m.pendingMode
		rtcpReducedSize = m.pendingRTCPReducedSize
	}
	return m.localSDPLocked(codecs, setup, !answered || rtcpReducedSize, mode, m.localRTPProfile(answered), m.localTLSIDFor(answered), m.localStreams(answered))
}

func (m *MediaSessionWebrtc) localSDPLocked(codecs []Codec, setup string, includeRTCPReducedSize bool, mode string, profile string, tlsID string, streams offeredStreams) ([]byte, error) {
	fingerprints, err := m.localFingerprintLines()
	if err != nil {
		return nil, fmt.Errorf("DTLS certificate fingerprint: %w", err)
	}
	candidate := m.localCandidates[0]
	for _, c := range m.localCandidates[1:] {
		if c.Priority() > candidate.Priority() {
			candidate = c
		}
	}
	ip := net.ParseIP(candidate.Address())
	addressType := "IP4"
	connectionIP := "0.0.0.0"
	if ip != nil {
		connectionIP = ip.String()
		if ip.To4() == nil {
			addressType = "IP6"
		}
	}
	if mode == "" {
		mode = sdp.ModeSendrecv
	}

	formats := make([]string, 0, len(codecs))
	originID, originVersion := m.originLocked().next()
	lines := []string{
		"v=0",
		fmt.Sprintf("o=diago %d %d IN %s %s", originID, originVersion, addressType, connectionIP),
		"s=diago",
		"t=0 0",
	}
	lines = append(lines, streams.bundleGroupLines()...)
	lines = append(lines, "a=msid-semantic: WMS diago")
	lines = append(lines, m.iceLiteLines()...)
	for _, codec := range codecs {
		formats = append(formats, strconv.Itoa(int(codec.PayloadType)))
	}
	lines = append(lines,
		fmt.Sprintf("m=audio %d %s %s", candidate.Port(), profile, strings.Join(formats, " ")),
		m.connectionLine(addressType, connectionIP),
	)
	lines = append(lines, streams.bundledAudioLines()...)
	lines = append(lines, "a=rtcp-mux")
	if includeRTCPReducedSize {
		lines = append(lines, "a=rtcp-rsize")
	}
	lines = append(lines,
		"a="+mode,
		"a=ice-ufrag:"+m.localUfrag,
		"a=ice-pwd:"+m.localPwd,
	)
	lines = append(lines, fingerprints...)
	lines = append(lines, dtlsAssociationLines(setup, tlsID)...)
	for _, codec := range codecs {
		channels := ""
		if codec.NumChannels > 1 {
			channels = "/" + strconv.Itoa(codec.NumChannels)
		}
		lines = append(lines, fmt.Sprintf("a=rtpmap:%d %s/%d%s", codec.PayloadType, codec.Name, codec.SampleRate, channels))
		if codec.FMTP != "" {
			lines = append(lines, fmt.Sprintf("a=fmtp:%d %s", codec.PayloadType, codec.FMTP))
		} else if strings.EqualFold(codec.Name, "opus") {
			lines = append(lines, fmt.Sprintf("a=fmtp:%d useinbandfec=0", codec.PayloadType))
		} else if strings.EqualFold(codec.Name, "telephone-event") {
			lines = append(lines, fmt.Sprintf("a=fmtp:%d 0-16", codec.PayloadType))
		}
	}
	for _, c := range m.localCandidates {
		lines = append(lines, "a=candidate:"+c.Marshal())
	}
	lines = append(lines, "a=end-of-candidates", "a=msid:diago audio", "")
	return streams.around(m.withDeclaredSource(streams, []byte(strings.Join(lines, "\r\n")))), nil
}

// RemoteSDP parses an answer when offered is true, otherwise it parses an
// offer. Starting ICE here is non-blocking so a SIP answer can be sent before
// the browser is required to complete connectivity checks.
//
// A description that keeps the ICE credentials of the generation in use and
// asks for a new DTLS association is refused with an error that wraps
// ErrWebRTCICERestartRequired, and leaves the session as it was.
//
// The agent takes the controlling role for an offer of ours and the
// controlled one for the peer's, unless exactly one of the two is a lite
// agent, the peer's being the one whose description carries a=ice-lite: the
// full agent then controls whichever side offered (RFC 8445 section 6.1.1).
//
// A description this session cannot meet is refused with an error that wraps
// ErrNoCommonMedia, when it has no audio stream or carries it over a transport
// outside the RTP profile family, or ErrNoCommonCodec, when none of its audio
// formats is one of ours. An a=candidate line the ICE stack cannot read is
// left out and the candidates of the other lines are used; a description left
// with no candidate is refused with an *ICEError that matches
// ErrICEConnection. The candidates are those of the audio section, or, when
// it lists none, of the sections bundled with it under its ICE credentials.
func (m *MediaSessionWebrtc) RemoteSDP(_ context.Context, body []byte, offered bool) error {
	var desc webrtcsdp.SessionDescription
	if err := desc.Unmarshal(body); err != nil {
		return fmt.Errorf("parse remote WebRTC SDP: %w", err)
	}
	md := webRTCAudioStream(&desc)
	if md == nil {
		return fmt.Errorf("remote WebRTC SDP has no audio media description: %w", ErrNoCommonMedia)
	}
	proto := strings.Join(md.MediaName.Protos, "/")
	prof, ok := parseRTPProfile(proto)
	if !ok {
		return fmt.Errorf("unsupported WebRTC audio transport %q: %w", proto, ErrNoCommonMedia)
	}
	if _, ok := webRTCAttribute(desc.Attributes, md.Attributes, "rtcp-mux"); !ok {
		return fmt.Errorf("remote WebRTC SDP requires rtcp-mux")
	}
	_, remoteRTCPReducedSize := webRTCAttribute(desc.Attributes, md.Attributes, "rtcp-rsize")
	remoteRTCPReducedSize = remoteRTCPReducedSize && prof.feedback
	remoteUfrag, ok := webRTCAttribute(desc.Attributes, md.Attributes, "ice-ufrag")
	if !ok || remoteUfrag == "" {
		return fmt.Errorf("remote WebRTC SDP has no ICE username fragment")
	}
	remotePwd, ok := webRTCAttribute(desc.Attributes, md.Attributes, "ice-pwd")
	if !ok || remotePwd == "" {
		return fmt.Errorf("remote WebRTC SDP has no ICE password")
	}
	setup, ok := webRTCAttribute(desc.Attributes, md.Attributes, "setup")
	if !ok {
		return fmt.Errorf("remote WebRTC SDP has no DTLS setup role")
	}
	fingerprints, err := webRTCFingerprints(desc.Attributes, md.Attributes)
	if err != nil {
		return err
	}
	tlsID := webRTCTLSID(desc.Attributes, md.Attributes)

	remoteCodecs, err := webRTCAudioCodecs(&desc, md)
	if err != nil {
		return err
	}
	m.mu.Lock()
	localCodecs := slices.Clone(m.Codecs)
	m.mu.Unlock()
	common := commonWebRTCCodecs(remoteCodecs, localCodecs)
	if len(common) == 0 {
		return fmt.Errorf("remote has no supported audio codec, remote=%v local=%v: %w", remoteCodecs, localCodecs, ErrNoCommonCodec)
	}

	remoteCandidates := make([]ice.Candidate, 0, 4)
	for _, attr := range webRTCCandidateAttributes(&desc, md) {
		if attr.Key != "candidate" {
			continue
		}
		candidate, candidateErr := ice.UnmarshalCandidate(attr.Value)
		if candidateErr != nil {
			skipRemoteCandidate(attr.Value, candidateErr)
			continue
		}
		if candidate.Component() == ice.ComponentRTP {
			remoteCandidates = append(remoteCandidates, candidate)
		}
	}
	if len(remoteCandidates) == 0 {
		return &ICEError{
			Phase: ICEPhaseConnection,
			Err:   fmt.Errorf("remote WebRTC SDP has no component-1 candidates; trickle ICE is not supported"),
		}
	}

	remoteMode := sdp.ModeSendrecv
	for _, direction := range []string{sdp.ModeSendrecv, sdp.ModeSendonly, sdp.ModeRecvonly, "inactive"} {
		if _, exists := webRTCAttribute(desc.Attributes, md.Attributes, direction); exists {
			remoteMode = direction
			break
		}
	}
	remoteSetup := strings.ToLower(setup)
	localSetup, err := m.dtlsSetupAgainst(setup, offered)
	if err != nil {
		return err
	}
	localDTLSClient := localSetup == dtlsSetupActive

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.agent == nil {
		return fmt.Errorf("webrtc media session is not initialized")
	}
	if m.iceConn != nil {
		ufragChanged := remoteUfrag != m.remoteUfrag
		pwdChanged := remotePwd != m.remotePwd
		if ufragChanged != pwdChanged {
			return fmt.Errorf("remote ICE restart must change both username fragment and password")
		}
		if ufragChanged {
			return ErrWebRTCICERestart
		}
		if !m.continuesRemoteFingerprints(fingerprints) {
			return fmt.Errorf("remote DTLS fingerprint changed without an ICE restart: %w", ErrWebRTCICERestartRequired)
		}
		if !offered && remoteSetup == "actpass" {
			localSetup = m.localSetup
		}
		if localSetup != m.localSetup {
			return fmt.Errorf("remote DTLS role changed without an ICE restart: %w", ErrWebRTCICERestartRequired)
		}
		if err = m.continueDTLSAssociation(tlsID); err != nil {
			return err
		}
		// A subsequent offer/answer with unchanged ICE credentials continues the
		// current ICE and DTLS transports. Stage only media-level state so a SIP
		// failure can leave the active stream untouched.
		m.pendingCodecs = slices.Clone(common)
		localPreference := m.modePreference
		if localPreference == "" {
			localPreference = m.Mode
		}
		m.pendingMode = negotiateMediaDirection(remoteMode, localPreference)
		m.pendingRTCPReducedSize = remoteRTCPReducedSize
		m.offerAccepted(proto, offered)
		m.stageOfferedStreams(webRTCOfferedStreams(&desc, md), offered)
		return nil
	}
	for _, candidate := range remoteCandidates {
		if err = m.agent.AddRemoteCandidate(candidate); err != nil {
			return &ICEError{Phase: ICEPhaseConnection, Err: fmt.Errorf("add remote candidate: %w", err)}
		}
	}
	m.remoteUfrag = remoteUfrag
	m.remotePwd = remotePwd
	_, m.remoteICELite = webRTCAttribute(desc.Attributes, md.Attributes, "ice-lite")
	m.remoteFingerprintAlgorithm = fingerprints[0].Algorithm
	m.remoteFingerprint = fingerprints[0].Value
	m.recordDTLSAssociation(localSetup, fingerprints, tlsID, offered)
	m.rtcpReducedSize = remoteRTCPReducedSize
	m.offerAccepted(proto, offered)
	m.keepOfferedStreams(webRTCOfferedStreams(&desc, md), offered)
	m.filterCodecs = common
	m.Codecs = slices.Clone(common)
	m.codec, _ = CodecAudioFromList(common)
	if m.modePreference == "" {
		m.modePreference = m.Mode
	}
	m.Mode = negotiateMediaDirection(remoteMode, m.modePreference)

	var conn *ice.Conn
	if m.iceControlling(offered) {
		// The offerer is the controlling ICE agent and nominates the pair.
		conn, err = m.agent.StartDial(remoteUfrag, remotePwd)
	} else {
		// The answerer is controlled. For actpass offers we choose active so
		// the answer can start the DTLS handshake without another round trip.
		conn, err = m.agent.StartAccept(remoteUfrag, remotePwd)
	}
	if err != nil {
		return &ICEError{Phase: ICEPhaseConnection, Err: fmt.Errorf("start connectivity checks: %w", err)}
	}
	m.iceConn = conn
	m.mux = newWebRTCPacketMux(conn)

	m.localSetup = localSetup
	dtlsConf := m.Config.DTLS.ToLibConf(fingerprints)
	dtlsTr := m.dtlsTransport()
	if localDTLSClient {
		m.dtlsConn, err = dtls.Client(dtlsTr, conn.RemoteAddr(), dtlsConf)
	} else {
		m.dtlsConn, err = dtls.Server(dtlsTr, conn.RemoteAddr(), dtlsConf)
	}
	if err != nil {
		return fmt.Errorf("create DTLS transport: %w", err)
	}

	return nil
}

// Finalize completes ICE first and DTLS second. This order is the central
// WebRTC layering rule: ICE selects and maintains the network path; DTLS and
// all encrypted media then travel only over that path.
//
// Each of the two waits for the peer and ends with an error when ctx is done
// first, or at the latest after ICEConnectTimeout and DTLSHandshakeTimeout. A
// negotiation FinalizeWithin left running is waited for in place of a new one.
func (m *MediaSessionWebrtc) Finalize(ctx context.Context) error {
	return m.finalize(ctx)
}

// negotiate is the negotiation Finalize runs. capped bounds its wait for the
// connectivity checks by ICEConnectTimeout and its handshake by
// DTLSHandshakeTimeout; without it only ctx ends them.
func (m *MediaSessionWebrtc) negotiate(ctx context.Context, capped bool) error {
	m.mu.Lock()
	if m.ready {
		if len(m.pendingCodecs) > 0 {
			m.filterCodecs = m.pendingCodecs
			m.Codecs = slices.Clone(m.pendingCodecs)
			m.codec, _ = CodecAudioFromList(m.pendingCodecs)
			m.Mode = m.pendingMode
			m.rtcpReducedSize = m.pendingRTCPReducedSize
			m.pendingCodecs = nil
			m.pendingMode = ""
			m.pendingRTCPReducedSize = false
			m.keepModePreference()
			m.commitOfferedStreams()
		}
		m.mu.Unlock()
		return nil
	}
	agent := m.agent
	dtlsConn := m.dtlsConn
	m.mu.Unlock()
	if agent == nil || dtlsConn == nil {
		return fmt.Errorf("remote WebRTC SDP is not configured")
	}
	if err := negotiationStep(ctx, capped, ICEConnectTimeout, agent.AwaitConnect); err != nil {
		return &ICEError{Phase: ICEPhaseConnection, Err: err}
	}
	if err := negotiationStep(ctx, capped, DTLSHandshakeTimeout, dtlsConn.HandshakeContext); err != nil {
		return fmt.Errorf("DTLS handshake: %w", err)
	}
	if err := m.retireDTLS(dtlsConn); err != nil {
		return err
	}
	state, ok := dtlsConn.ConnectionState()
	if !ok {
		return fmt.Errorf("get DTLS connection state")
	}
	profile, ok := dtlsConn.SelectedSRTPProtectionProfile()
	if !ok {
		return fmt.Errorf("DTLS peer did not negotiate an SRTP profile")
	}
	p := srtp.ProtectionProfile(profile)
	keyLen, err := p.KeyLen()
	if err != nil {
		return fmt.Errorf("get SRTP key length: %w", err)
	}
	saltLen, err := p.SaltLen()
	if err != nil {
		return fmt.Errorf("get SRTP salt length: %w", err)
	}
	material, err := state.ExportKeyingMaterial("EXTRACTOR-dtls_srtp", nil, 2*(keyLen+saltLen))
	if err != nil {
		return fmt.Errorf("export DTLS-SRTP keying material: %w", err)
	}
	clientKey := material[:keyLen]
	serverKey := material[keyLen : 2*keyLen]
	clientSalt := material[2*keyLen : 2*keyLen+saltLen]
	serverSalt := material[2*keyLen+saltLen:]
	// DTLS role, not ICE role, determines which half of the exporter is used.
	if m.localSetup == "passive" {
		clientKey, serverKey = serverKey, clientKey
		clientSalt, serverSalt = serverSalt, clientSalt
	}
	localContext, err := srtp.CreateContext(clientKey, clientSalt, p)
	if err != nil {
		return fmt.Errorf("create local SRTP context: %w", err)
	}
	remoteContext, err := srtp.CreateContext(serverKey, serverSalt, p)
	if err != nil {
		return fmt.Errorf("create remote SRTP context: %w", err)
	}
	localRTCPContext, err := srtp.CreateContext(clientKey, clientSalt, p)
	if err != nil {
		return fmt.Errorf("create local SRTCP context: %w", err)
	}
	remoteRTCPContext, err := srtp.CreateContext(serverKey, serverSalt, p)
	if err != nil {
		return fmt.Errorf("create remote SRTCP context: %w", err)
	}
	pair, _ := agent.GetSelectedCandidatePair()
	m.mu.Lock()
	m.localCtxSRTP = localContext
	m.remoteCtxSRTP = remoteContext
	m.localCtxSRTCP = localRTCPContext
	m.remoteCtxSRTCP = remoteRTCPContext
	m.ready = true
	if pair != nil {
		m.Laddr = pair.Local.Address()
		m.Raddr = pair.Remote.Address()
	}
	m.mu.Unlock()
	return nil
}

// Rollback drops a staged same-ICE-generation media update. It does not touch
// the active ICE, DTLS, SRTP or SRTCP transports.
func (m *MediaSessionWebrtc) Rollback() {
	m.mu.Lock()
	m.pendingCodecs = nil
	m.pendingMode = ""
	m.pendingRTCPReducedSize = false
	m.restoreModePreference()
	m.dropStagedStreams()
	m.mu.Unlock()
}

func (m *MediaSessionWebrtc) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	mux := m.mux
	agent := m.agent
	m.mu.Unlock()
	// A negotiation left running reads and writes the mux and the agent
	// closed below, so it ends first.
	m.leftRunning.stop()
	var muxErr, agentErr error
	if mux != nil {
		muxErr = mux.Close()
	}
	if agent != nil {
		agentErr = agent.Close()
	}
	return errors.Join(muxErr, agentErr, m.closeICESocket())
}

// StartRTP clears the read deadline StopRTP set, for the same values of rw.
func (m *MediaSessionWebrtc) StartRTP(rw int8) error { return m.startRTP(rw) }

// StopRTP sets the deadline of the session's RTP reads to duration from now,
// when rw has bit 1 set or is zero, and does nothing for any other rw. As on
// a MediaSession, no duration stops the reads at once; StartRTP is what
// clears the deadline. SetRTPDeadline also reaches the write side.
func (m *MediaSessionWebrtc) StopRTP(rw int8, duration time.Duration) error {
	m.mu.Lock()
	mux := m.mux
	m.mu.Unlock()
	if mux == nil {
		return fmt.Errorf("WebRTC RTP transport is not initialized")
	}
	deadline := time.Now().Add(duration)
	if rw&1 != 0 || rw == 0 {
		return m.setRTPDeadline(mux, 1, deadline)
	}
	return nil
}

// ReadRTP reads and decrypts the next RTP packet of the peer into pkt.
//
// A packet that is not the peer's media is discarded and the next one is read:
// one the SRTP context refuses (RFC 3711 section 3.3), and every packet that
// arrives before Finalize has keyed the session (RFC 5764 section 5.1). A read
// on a session that is not keyed therefore waits for its keys, and returns
// like any other read when the session is closed or a deadline StopRTP set
// passes. Only a session without a remote description, which has nothing to
// read from, returns an error at once.
func (m *MediaSessionWebrtc) ReadRTP(buf []byte, pkt *rtp.Packet) (int, error) {
	if len(buf) < RTPBufSize {
		return 0, io.ErrShortBuffer
	}
	mux := m.packetPath().mux
	if mux == nil {
		return 0, fmt.Errorf("WebRTC media is not finalized")
	}
	for {
		n, from, err := mux.rtp.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		path := m.packetPath()
		if path.remoteSRTP == nil {
			logUnkeyedRTP(from, n)
			continue
		}
		decrypted, err := path.remoteSRTP.DecryptRTP(buf, buf[:n], &pkt.Header)
		if err != nil {
			m.discardUnauthenticRTP(from, n, err)
			continue
		}
		n = len(decrypted)
		if err = rtpUnmarshalPayload(decrypted, pkt); err != nil {
			return n, fmt.Errorf("unmarshal WebRTC RTP: %w", err)
		}
		if path.mode == sdp.ModeSendonly || path.mode == "inactive" {
			return 0, nil
		}
		return n, nil
	}
}

func (m *MediaSessionWebrtc) ReadRTPRaw(buf []byte) (int, error) {
	mux := m.packetPath().mux
	if mux == nil {
		return 0, fmt.Errorf("WebRTC RTP transport is not initialized")
	}
	n, _, err := mux.rtp.ReadFrom(buf)
	return n, err
}

// WriteRTP encrypts pkt and sends it to the peer. On a session Finalize has
// not keyed it sends nothing and returns an error that wraps ErrDTLSNotKeyed.
//
// On a session whose peer offered the audio stream in a BUNDLE group with the
// MID header extension mapped, the packet sent carries the stream's tag in
// that extension (RFC 8843 sections 9.2 and 15.2); pkt itself is not changed.
func (m *MediaSessionWebrtc) WriteRTP(pkt *rtp.Packet) error {
	if m.leftRunning.unkeyed.Load() {
		return ErrDTLSNotKeyed
	}
	path := m.packetPath()
	if path.mux == nil || path.localSRTP == nil {
		return fmt.Errorf("WebRTC media is not finalized: %w", ErrDTLSNotKeyed)
	}
	if path.mode == sdp.ModeRecvonly || path.mode == "inactive" {
		return nil
	}

	if m.writeRTPBuf == nil {
		m.writeRTPBuf = make([]byte, RTPBufSize)
	}
	buf := m.writeRTPBuf
	pkt = path.mid.tagged(pkt)
	n, err := pkt.MarshalTo(buf)
	if err == nil {
		var data []byte
		data, err = path.localSRTP.EncryptRTP(buf, buf[:n], &pkt.Header)
		if err == nil {
			n, err = path.mux.rtp.WriteTo(data, nil)
			if err == nil && n != len(data) {
				err = io.ErrShortWrite
			}
		}
	}
	if err != nil {
		return fmt.Errorf("write WebRTC SRTP: %w", err)
	}
	return nil
}

// ReadRTCP reads and decrypts the next RTCP datagram of the peer into pkts and
// returns the number of packets it held. Like ReadRTP, it discards a datagram
// the SRTCP context refuses (RFC 3711 section 3.4) and every datagram that
// arrives before Finalize has keyed the session, and reads the next one.
func (m *MediaSessionWebrtc) ReadRTCP(buf []byte, pkts []rtcp.Packet) (int, error) {
	mux := m.packetPath().mux
	if mux == nil {
		return 0, fmt.Errorf("WebRTC media is not finalized")
	}
	for {
		n, from, err := mux.rtcp.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		path := m.packetPath()
		if path.remoteSRTCP == nil {
			logUnkeyedRTCP(from, n)
			continue
		}
		data, err := path.remoteSRTCP.DecryptRTCP(buf, buf[:n], nil)
		if err != nil {
			m.discardUnauthenticRTCP(from, n, err)
			continue
		}
		return RTCPUnmarshal(data, pkts)
	}
}

// WriteRTCP encrypts pkt and sends it to the peer. On a session Finalize has
// not keyed it sends nothing and returns an error that wraps ErrDTLSNotKeyed.
func (m *MediaSessionWebrtc) WriteRTCP(pkt rtcp.Packet) error {
	if m.leftRunning.unkeyed.Load() {
		return ErrDTLSNotKeyed
	}
	data, err := pkt.Marshal()
	if err != nil {
		return err
	}

	path := m.packetPath()
	if path.mux == nil || path.localSRTCP == nil {
		return fmt.Errorf("WebRTC media is not finalized: %w", ErrDTLSNotKeyed)
	}

	needed := len(data) + 64
	if cap(m.writeRTCPBuf) < needed {
		m.writeRTCPBuf = make([]byte, needed)
	}
	buf := m.writeRTCPBuf[:needed]
	data, err = path.localSRTCP.EncryptRTCP(buf, data, nil)
	if err != nil {
		return fmt.Errorf("encrypt WebRTC SRTCP: %w", err)
	}
	m.writeRTCPBuf = data[:cap(data)]
	n, err := path.mux.rtcp.WriteTo(data, nil)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func findWebRTCAudio(desc *webrtcsdp.SessionDescription) *webrtcsdp.MediaDescription {
	for _, md := range desc.MediaDescriptions {
		if md.MediaName.Media == "audio" && md.MediaName.Port.Value != 0 {
			return md
		}
	}
	return nil
}

func webRTCAttribute(session, media []webrtcsdp.Attribute, key string) (string, bool) {
	for _, attr := range media {
		if attr.Key == key {
			return attr.Value, true
		}
	}
	for _, attr := range session {
		if attr.Key == key {
			return attr.Value, true
		}
	}
	return "", false
}

func webRTCAudioCodecs(desc *webrtcsdp.SessionDescription, md *webrtcsdp.MediaDescription) ([]Codec, error) {
	attrs := make([]string, 0, len(desc.Attributes)+len(md.Attributes))
	for _, attr := range desc.Attributes {
		attrs = append(attrs, attr.String())
	}
	for _, attr := range md.Attributes {
		attrs = append(attrs, attr.String())
	}
	codecs := make([]Codec, len(md.MediaName.Formats))
	n, err := CodecsFromSDPRead(md.MediaName.Formats, attrs, codecs)
	if err != nil {
		return nil, fmt.Errorf("parse remote WebRTC codecs: %w", err)
	}
	return codecs[:n], nil
}

func commonWebRTCCodecs(remote, local []Codec) []Codec {
	common := make([]Codec, 0, len(remote))
	for _, rc := range remote {
		for _, lc := range local {
			if !strings.EqualFold(rc.Name, lc.Name) || rc.SampleRate != lc.SampleRate || rc.NumChannels != lc.NumChannels {
				continue
			}
			// The answer must echo the offer's payload type. Keep local packet
			// duration/capability data but use the negotiated remote PT.
			lc.PayloadType = rc.PayloadType
			if lc.FMTP == "" {
				lc.FMTP = rc.FMTP
			}
			common = append(common, lc)
			break
		}
	}
	return common
}
