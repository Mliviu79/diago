package diago

import (
	"strings"
	"sync"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referHeaderRecorder answers every REFER 403 and keeps the headers each REFER
// carried. Every other request is answered 200.
type referHeaderRecorder struct {
	mu      sync.Mutex
	headers [][]recordedHeader
}

// recordedHeader is one header of a recorded REFER.
type recordedHeader struct {
	name  string
	value string
}

func (r *referHeaderRecorder) respond(req *sip.Request) *sip.Response {
	if req.Method != sip.REFER {
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	}
	var hs []recordedHeader
	for _, h := range req.Headers() {
		hs = append(hs, recordedHeader{name: h.Name(), value: h.Value()})
	}
	r.mu.Lock()
	r.headers = append(r.headers, hs)
	r.mu.Unlock()
	return sip.NewResponseFromRequest(req, sip.StatusForbidden, "Forbidden", nil)
}

// compactNames maps a header name to its compact form, so lastValues counts
// a Refer-To sent as "r" and a Referred-By sent as "b".
var compactNames = map[string]string{"Refer-To": "r", "Referred-By": "b"}

// lastValues returns the values of every header named name or its compact
// form, compared case-insensitively, that the last REFER recorded carried.
func (r *referHeaderRecorder) lastValues(t *testing.T, name string) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.headers, "no REFER was sent")
	var values []string
	for _, h := range r.headers[len(r.headers)-1] {
		if strings.EqualFold(h.name, name) || strings.EqualFold(h.name, compactNames[name]) {
			values = append(values, h.value)
		}
	}
	return values
}

// TestReferHeadersReplaceOursWhateverTheirCase checks, for each session type
// and each Refer form that takes headers, that a Refer-To or Referred-By the
// caller passes replaces ours whatever the case of its name (RFC 3261 section
// 7.3.1) and also in its compact form "r" or "b" (RFC 3261 section 7.3.3), so
// the REFER carries exactly one of each, and that every other
// header the caller passes reaches the wire, a misspelt "reffered-by" among
// them.
func TestReferHeadersReplaceOursWhateverTheirCase(t *testing.T) {
	referTo := sip.Uri{User: "target", Host: "127.0.0.9", Port: 5060}
	callerReferTo := "<sip:caller-target@example.com>"
	callerReferredBy := "<sip:operator@example.com>"

	spellings := []struct {
		name       string
		referTo    string
		referredBy string
	}{
		{name: "canonical", referTo: "Refer-To", referredBy: "Referred-By"},
		{name: "lowercase", referTo: "refer-to", referredBy: "referred-by"},
		{name: "uppercase", referTo: "REFER-TO", referredBy: "REFERRED-BY"},
		{name: "compact", referTo: "r", referredBy: "b"},
		{name: "compact_uppercase", referTo: "R", referredBy: "B"},
	}

	check := func(t *testing.T, rec *referHeaderRecorder, forms []referForm) {
		for _, form := range forms {
			for _, sp := range spellings {
				t.Run(form.name+"_"+sp.name, func(t *testing.T) {
					status, err := form.refer(t.Context(), referTo,
						sip.NewHeader(sp.referTo, callerReferTo),
						sip.NewHeader(sp.referredBy, callerReferredBy),
						sip.NewHeader("X-Transfer-Reason", "queue"),
						sip.NewHeader("reffered-by", "<sip:typo@example.com>"),
					)
					requireRefused(t, status, err)
					assert.Equal(t, []string{callerReferTo}, rec.lastValues(t, "Refer-To"))
					assert.Equal(t, []string{callerReferredBy}, rec.lastValues(t, "Referred-By"))
					assert.Equal(t, []string{"queue"}, rec.lastValues(t, "X-Transfer-Reason"))
					assert.Equal(t, []string{"<sip:typo@example.com>"}, rec.lastValues(t, "reffered-by"))
				})
			}
			t.Run(form.name+"_ours_kept", func(t *testing.T) {
				status, err := form.refer(t.Context(), referTo, sip.NewHeader("X-Transfer-Reason", "queue"))
				requireRefused(t, status, err)
				assert.Equal(t, []string{"<" + referTo.String() + ">"}, rec.lastValues(t, "Refer-To"))
				assert.Len(t, rec.lastValues(t, "Referred-By"), 1)
				assert.Equal(t, []string{"queue"}, rec.lastValues(t, "X-Transfer-Reason"))
			})
		}
	}

	t.Run("client_session", func(t *testing.T) {
		rec := &referHeaderRecorder{}
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
		check(t, rec, clientReferForms(d)[:3])
	})

	t.Run("server_session", func(t *testing.T) {
		rec := &referHeaderRecorder{}
		ua, err := sipgo.NewUA()
		require.NoError(t, err)
		t.Cleanup(func() { ua.Close() })
		client, err := sipgo.NewClient(ua)
		require.NoError(t, err)
		client.TxRequester = &clientTxRequester{onRequest: rec.respond}
		dialogUA := &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "dg", Host: "127.0.0.1", Port: 5090}}}
		d, _ := newAnsweredTestServerCallOverUA(t, dialogUA)

		check(t, rec, serverReferForms(d)[:3])
	})
}
