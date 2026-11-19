// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"

	"github.com/emiago/diago/media"
)

// RTPPortAllocator hands out and takes back local RTP ports for media sessions.
// Set one with MediaConfig.RTPPortAllocator.
type RTPPortAllocator interface {
	// AllocateRTPPort receives the context of the call the port is for: the
	// dialog's own context on a server dialog, and the context of Invite on a
	// client dialog. It returns an even port whose RTCP companion, port+1, the
	// allocator must consider taken as well, as MediaSession binds the pair.
	// An ended context must fail the allocation, with context.Cause(ctx) or an
	// error wrapping it, and allocate nothing. The error surfaces as the media
	// session init failure.
	AllocateRTPPort(ctx context.Context) (int, error)
	// ReleaseRTPPort is called exactly once per successful allocation, from
	// DialogMedia.Close, after the sockets are closed. It must tolerate a port
	// it does not recognise, as it runs on teardown and so on error paths.
	ReleaseRTPPort(port int)
}

// mediaConfForTransport builds the media config a dialog running on tran starts
// from. The transport supplies the media binding and the SRTP mode, the Diago
// wide MediaConfig the rest. Both the server and the client dialog go through
// here, so neither can quietly drop a field the other still carries.
func (dg *Diago) mediaConfForTransport(tran *Transport) MediaConfig {
	return MediaConfig{
		Codecs:       dg.mediaConf.Codecs,
		SecureRTPAlg: dg.mediaConf.SecureRTPAlg,
		// The media reads it when it makes an offer of ours, so it has to
		// reach the config each dialog's media is built from.
		ReInviteNegotiatedCodecs: dg.mediaConf.ReInviteNegotiatedCodecs,
		// A dialog binds through the allocator the caller installed on Diago.
		// Whoever builds the dialog's media reads it from the per dialog
		// config, so a config that does not carry it leaves every dialog on
		// the default port choice while the allocator sits unused.
		RTPPortAllocator: dg.mediaConf.RTPPortAllocator,
		// Resolved here, so every media session a dialog on tran builds
		// starts from the same DTLS config, the Diago wide one when set.
		DTLSConfig: dg.dtlsConfForTransport(tran),
		secureRTP:  tran.MediaSRTP,
		bindIP:     tran.mediaBindIP,
		externalIP: tran.MediaExternalIP,
		dtlsConf:   tran.MediaDTLSConf,
	}
}

// dtlsConfForTransport resolves the DTLS config a dialog on tran starts from.
// A Diago wide DTLSConfig is opt in and wins; otherwise the transport keeps
// supplying its own MediaDTLSConf, which is always a value and so cannot say
// "unset". The transport config is copied so a dialog cannot mutate it.
func (dg *Diago) dtlsConfForTransport(tran *Transport) *media.DTLSConfig {
	if dg.mediaConf.DTLSConfig != nil {
		return dg.mediaConf.DTLSConfig
	}
	conf := tran.MediaDTLSConf
	return &conf
}

// dtlsConfig returns the DTLS config of a media session built from conf, which
// takes it by value: DTLSConfig when it is set, and otherwise the config the
// transport supplied, the zero one for a caller that never touched DTLS.
func (conf MediaConfig) dtlsConfig() media.DTLSConfig {
	if conf.DTLSConfig != nil {
		return *conf.DTLSConfig
	}
	return conf.dtlsConf
}

// allocateRTPPort chooses the local RTP port of one media that is built from
// conf, on ctx, the context of the call the media is for. With no allocator
// installed it returns port 0 and a nil release: 0 leaves the choice to the
// OS or to the media package port range, and the OS takes such a port back
// when its socket closes.
//
// With an allocator, the caller owes release exactly one call: once the
// sockets bound to the port are closed, or at once when nothing was bound.
func (conf MediaConfig) allocateRTPPort(ctx context.Context) (port int, release func(), err error) {
	alloc := conf.RTPPortAllocator
	if alloc == nil {
		return 0, nil, nil
	}
	port, err = alloc.AllocateRTPPort(ctx)
	if err != nil {
		return 0, nil, err
	}
	return port, func() { alloc.ReleaseRTPPort(port) }, nil
}
