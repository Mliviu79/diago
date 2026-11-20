package diago

import (
	"fmt"

	"github.com/emiago/sipgo/sip"
)

// referrerAOR returns the Referred-By URI of a REFER this side sends within a
// dialog whose local party header is local: the URI of our From on a dialog we
// started, of our To on one we answered. RFC 3892 section 2 has the referrer
// provide "a Referred-By header with its SIP address-of-record", section 3
// makes it "a SIP URI representing the identity of the referrer", and every
// example in section 7 carries the REFER's From URI. It is never the peer and
// never a Contact: a Contact names a device, not the referrer's identity. The
// header parameters of local, its tag among them, are not part of the URI and
// are left out, and so are any URI headers, which an address-of-record does
// not carry.
func referrerAOR(local *sip.Uri) (sip.Uri, error) {
	if local == nil {
		return sip.Uri{}, fmt.Errorf("refer: dialog has no local address")
	}
	aor := *local.Clone()
	aor.Headers = nil
	return aor, nil
}

// referrerAOR returns our address-of-record on a dialog we started: the URI of
// the From of our INVITE.
func (d *DialogClientSession) referrerAOR() (sip.Uri, error) {
	from := d.InviteRequest.From()
	if from == nil {
		return referrerAOR(nil)
	}
	return referrerAOR(&from.Address)
}

// referrerAOR returns our address-of-record on a dialog we answered: the URI
// of the To we answered with, which RFC 3261 section 8.2.6.2 makes the URI of
// the INVITE's To.
func (d *DialogServerSession) referrerAOR() (sip.Uri, error) {
	to := d.InviteRequest.To()
	if to == nil {
		return referrerAOR(nil)
	}
	return referrerAOR(&to.Address)
}
