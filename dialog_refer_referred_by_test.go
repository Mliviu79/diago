package diago

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referRecorder answers every REFER 403, so each Refer form returns as soon
// as the REFER's final response arrives, and keeps the Referred-By values the
// REFERs carried. Every other request is answered 200.
type referRecorder struct {
	mu         sync.Mutex
	referredBy [][]string
}

func (r *referRecorder) respond(req *sip.Request) *sip.Response {
	if req.Method != sip.REFER {
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	}
	var values []string
	for _, h := range req.GetHeaders("Referred-By") {
		values = append(values, h.Value())
	}
	r.mu.Lock()
	r.referredBy = append(r.referredBy, values)
	r.mu.Unlock()
	return sip.NewResponseFromRequest(req, sip.StatusForbidden, "Forbidden", nil)
}

// last returns the Referred-By values of the last REFER recorded.
func (r *referRecorder) last(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.referredBy, "no REFER was sent")
	return r.referredBy[len(r.referredBy)-1]
}

// A referForm is one Refer form of a session. refer sends a REFER for referTo
// with the extra headers given and returns the status of the REFER's final
// response.
type referForm struct {
	name  string
	refer func(ctx context.Context, referTo sip.Uri, headers ...sip.Header) (int, error)
}

// refusedStatus returns the status of the final response err reports for a
// refused REFER.
func refusedStatus(err error) (int, error) {
	var resErr sipgo.ErrDialogResponse
	if !errors.As(err, &resErr) {
		return 0, err
	}
	return resErr.Res.StatusCode, nil
}

// observedStatus returns the status of the final response a refused
// ReferAndObserve attempt recorded.
func observedStatus(obs ReferObservation, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	if obs.End != ReferEndRefused {
		return 0, fmt.Errorf("refer: the attempt ended %s, not refused", obs.End)
	}
	return obs.Response.Status, nil
}

func clientReferForms(d *DialogClientSession) []referForm {
	return []referForm{
		{name: "Refer", refer: func(ctx context.Context, referTo sip.Uri, headers ...sip.Header) (int, error) {
			return refusedStatus(d.Refer(ctx, referTo, headers...))
		}},
		{name: "ReferOptions", refer: func(ctx context.Context, referTo sip.Uri, headers ...sip.Header) (int, error) {
			return refusedStatus(d.ReferOptions(ctx, referTo, ReferClientOptions{Headers: headers}))
		}},
		{name: "ReferAndWait", refer: func(ctx context.Context, referTo sip.Uri, headers ...sip.Header) (int, error) {
			return refusedStatus(d.ReferAndWait(ctx, referTo, headers...))
		}},
		{name: "ReferAndObserve", refer: func(ctx context.Context, referTo sip.Uri, _ ...sip.Header) (int, error) {
			return observedStatus(d.ReferAndObserve(ctx, referTo, ReferObserveOptions{}))
		}},
	}
}

func serverReferForms(d *DialogServerSession) []referForm {
	return []referForm{
		{name: "Refer", refer: func(ctx context.Context, referTo sip.Uri, headers ...sip.Header) (int, error) {
			return refusedStatus(d.Refer(ctx, referTo, headers...))
		}},
		{name: "ReferOptions", refer: func(ctx context.Context, referTo sip.Uri, headers ...sip.Header) (int, error) {
			return refusedStatus(d.ReferOptions(ctx, referTo, ReferServerOptions{Headers: headers}))
		}},
		{name: "ReferAndWait", refer: func(ctx context.Context, referTo sip.Uri, headers ...sip.Header) (int, error) {
			return refusedStatus(d.ReferAndWait(ctx, referTo, headers...))
		}},
		{name: "ReferAndObserve", refer: func(ctx context.Context, referTo sip.Uri, _ ...sip.Header) (int, error) {
			return observedStatus(d.ReferAndObserve(ctx, referTo, ReferObserveOptions{}))
		}},
	}
}

// requireRefused fails unless a Refer form reported the REFER's 403.
func requireRefused(t *testing.T, status int, err error) {
	t.Helper()
	require.NoError(t, err)
	assert.Equal(t, sip.StatusForbidden, status)
}

// requireReferredByAOR fails unless values is one Referred-By naming aor:
// the URI alone, without the tag or any other parameter of the header it
// came from, and not contact.
func requireReferredByAOR(t *testing.T, values []string, aor, contact sip.Uri) {
	t.Helper()
	require.Len(t, values, 1, "the REFER carries one Referred-By")

	var got sip.Uri
	params := sip.NewParams()
	_, err := sip.ParseAddressValue(values[0], &got, &params)
	require.NoError(t, err)
	assert.Equal(t, aor.String(), got.String(), "Referred-By is our address-of-record")
	assert.Equal(t, 0, params.Length(), "Referred-By carries no header parameters, got %q", values[0])
	assert.NotEqual(t, contact.String(), got.String(), "Referred-By names our Contact")
}

// TestReferredByNamesOurAddressOfRecord checks, for each session type and each
// Refer form, that the REFER on the wire carries Referred-By with our
// address-of-record in the dialog (RFC 3892 sections 2 and 3): the URI of our
// From on a call we placed, of our To on one we answered.
func TestReferredByNamesOurAddressOfRecord(t *testing.T) {
	referTo := sip.Uri{User: "target", Host: "127.0.0.9", Port: 5060}

	t.Run("client_session", func(t *testing.T) {
		rec := &referRecorder{}
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			res := rec.respond(req)
			if req.IsInvite() {
				res.SetBody(peerSDP())
			}
			return res
		})
		d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
		require.NoError(t, err)
		t.Cleanup(func() { d.Close() })
		_, err = d.Invite(t.Context(), InviteClientOptions{})
		require.NoError(t, err)
		require.NoError(t, d.Ack(t.Context()))

		aor := d.InviteRequest.From().Address
		contact := d.InviteRequest.Contact().Address
		require.NotEqual(t, aor.String(), contact.String(), "the test needs a From apart from the Contact")
		for _, form := range clientReferForms(d) {
			t.Run(form.name, func(t *testing.T) {
				status, err := form.refer(t.Context(), referTo)
				requireRefused(t, status, err)
				requireReferredByAOR(t, rec.last(t), aor, contact)
				assert.NotEqual(t, d.InviteRequest.To().Address.String(), aor.String(), "Referred-By names the peer")
			})
		}
	})

	t.Run("server_session", func(t *testing.T) {
		rec := &referRecorder{}
		ua, err := sipgo.NewUA()
		require.NoError(t, err)
		t.Cleanup(func() { ua.Close() })
		client, err := sipgo.NewClient(ua)
		require.NoError(t, err)
		client.TxRequester = &clientTxRequester{onRequest: rec.respond}
		dialogUA := &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "dg", Host: "127.0.0.1", Port: 5090}}}
		d, _ := newAnsweredTestServerCallOverUA(t, dialogUA)

		aor := d.InviteRequest.To().Address
		contact := d.InviteResponse.Contact().Address
		for _, form := range serverReferForms(d) {
			t.Run(form.name, func(t *testing.T) {
				status, err := form.refer(t.Context(), referTo)
				requireRefused(t, status, err)
				requireReferredByAOR(t, rec.last(t), aor, contact)
				assert.NotEqual(t, d.InviteRequest.From().Address.String(), aor.String(), "Referred-By names the peer")
			})
		}
	})
}

// TestReferredByFromHeadersReplacesOurs checks that a Referred-By the caller
// passes among a REFER's headers replaces the one the session puts there.
func TestReferredByFromHeadersReplacesOurs(t *testing.T) {
	referTo := sip.Uri{User: "target", Host: "127.0.0.9", Port: 5060}
	own := sip.NewHeader("Referred-By", "<sip:operator@example.com>")

	rec := &referRecorder{}
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		res := rec.respond(req)
		if req.IsInvite() {
			res.SetBody(peerSDP())
		}
		return res
	})
	d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	_, err = d.Invite(t.Context(), InviteClientOptions{})
	require.NoError(t, err)
	require.NoError(t, d.Ack(t.Context()))

	// The first three forms take headers; ReferAndObserve takes none.
	for _, form := range clientReferForms(d)[:3] {
		t.Run(form.name, func(t *testing.T) {
			status, err := form.refer(t.Context(), referTo, own)
			requireRefused(t, status, err)
			assert.Equal(t, []string{own.Value()}, rec.last(t))
		})
	}
}
